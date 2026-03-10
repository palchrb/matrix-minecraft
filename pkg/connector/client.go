package connector

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MCClient implementerer bridgev2.NetworkAPI for en enkelt Minecraft-server.
// Hver server-login har sin egen MCClient.
type MCClient struct {
	UserLogin   *bridgev2.UserLogin
	Connector   *MCConnector
	Meta        *MCLoginMetadata
	RCON        *RCONClient
	LogTailer   *LogTailer
	AvatarFetch *AvatarFetcher

	lineCh chan ChatLine
	cancel context.CancelFunc
	log    zerolog.Logger
	once   sync.Once
}

var _ bridgev2.NetworkAPI = (*MCClient)(nil)

func (c *MCClient) Connect(ctx context.Context) {
	c.log = c.Connector.log.With().
		Str("container", c.Meta.ContainerName).Logger()

	c.lineCh = make(chan ChatLine, 64)
	tailCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel

	// Koble til RCON
	if err := c.RCON.Connect(ctx); err != nil {
		c.log.Error().Err(err).Msg("RCON-tilkobling feilet")
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "rcon-connect-failed",
			Message:    fmt.Sprintf("Kunne ikke koble til RCON: %v", err),
		})
		return
	}

	// Start log-tailing for chat/death/advancement i bakgrunnen
	go c.LogTailer.Start(tailCtx, c.lineCh)
	go c.receiveLoop(tailCtx)

	// Start RCON-basert spiller-overvåking for join/leave
	go c.watchPlayers(tailCtx)

	// Ensure the portal (Matrix room) exists for this server
	portalKey := makePortalKey(c.Meta.ContainerName)
	portal, err := c.UserLogin.Bridge.GetPortalByKey(ctx, portalKey)
	if err != nil {
		c.log.Warn().Err(err).Msg("Kunne ikke hente portal")
	} else if portal.MXID == "" {
		chatInfo, _ := c.GetChatInfo(ctx, portal)
		if createErr := portal.CreateMatrixRoom(ctx, c.UserLogin, chatInfo); createErr != nil {
			c.log.Warn().Err(createErr).Msg("Kunne ikke opprette Matrix-rom")
		} else {
			c.log.Info().Str("room", string(portal.MXID)).Msg("Matrix-rom opprettet for server")
		}
	}

	c.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnected,
	})
	c.log.Info().Msg("Server-klient tilkoblet")
}

func (c *MCClient) Disconnect() {
	c.log.Info().Msg("Kobler fra server")
	if c.cancel != nil {
		c.cancel()
	}
	c.RCON.Disconnect()
}

func (c *MCClient) IsLoggedIn() bool {
	return c.RCON.IsConnected()
}

func (c *MCClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *MCClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	// Server-klienter eier ingen ghost-brukere direkte
	return false
}

func (c *MCClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	name := c.Meta.DisplayName
	if name == "" {
		name = c.Meta.ContainerName
	}
	info := &bridgev2.ChatInfo{
		Name: &name,
		Type: ptrTo(database.RoomTypeDefault),
		Members: &bridgev2.ChatMemberList{
			IsFull: true,
			Members: []bridgev2.ChatMember{{
				EventSender: bridgev2.EventSender{
					IsFromMe: true,
					Sender:   networkid.UserID(c.UserLogin.UserMXID),
				},
				Membership: event.MembershipJoin,
				PowerLevel: ptrInt(50),
			}},
		},
	}

	// Prøv å hente server-icon.png fra MC-containeren som portal-avatar.
	// Fallback til portal_avatar_mxc fra config.
	if iconData := c.fetchServerIcon(ctx); iconData != nil {
		info.Avatar = &bridgev2.Avatar{
			ID: networkid.AvatarID("server-icon-" + c.Meta.ContainerName),
			Get: func(ctx context.Context) ([]byte, error) {
				return iconData, nil
			},
		}
	} else if mxc := c.Connector.Config.PortalAvatarMXC; mxc != "" {
		info.Avatar = &bridgev2.Avatar{
			ID:  networkid.AvatarID("portal-avatar"),
			MXC: id.ContentURIString(mxc),
		}
	}
	return info, nil
}

// fetchServerIcon henter server-icon.png fra MC-containeren via Docker cp.
func (c *MCClient) fetchServerIcon(ctx context.Context) []byte {
	reader, _, err := c.Connector.docker.CopyFromContainer(
		ctx, c.Meta.ContainerName, "/data/server-icon.png",
	)
	if err != nil {
		c.log.Debug().Err(err).Msg("Kunne ikke hente server-icon.png fra container")
		return nil
	}
	defer reader.Close()

	// Docker CopyFromContainer returnerer en tar-strøm
	tr := tar.NewReader(reader)
	if _, err := tr.Next(); err != nil {
		c.log.Debug().Err(err).Msg("Kunne ikke lese tar-header for server-icon")
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
	if err != nil {
		c.log.Debug().Err(err).Msg("Kunne ikke lese server-icon data")
		return nil
	}
	c.log.Debug().Int("bytes", len(data)).Msg("Hentet server-icon.png fra container")
	return data
}

func ptrInt(v int) *int {
	return &v
}

func (c *MCClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	username := string(ghost.ID)

	info := &bridgev2.UserInfo{
		Name: &username,
	}

	// Hent avatar fra Starlight Skins API.
	// Siden AggressiveUpdateInfo er aktivert kalles denne på hver melding,
	// så vi sjekker om ghosten allerede har avatar satt for å unngå
	// unødvendige HTTP-kall (bare re-sjekk hvert 1. time).
	if c.AvatarFetch != nil {
		var etag string
		var skipFetch bool
		if meta, ok := ghost.Metadata.(*GhostMetadata); ok {
			etag = meta.AvatarETag
			// Hvis avatar allerede er satt og hentet for < 1 time siden, skip
			if ghost.AvatarSet && etag != "" &&
				!meta.AvatarFetchedAt.IsZero() &&
				time.Since(meta.AvatarFetchedAt) < 1*time.Hour {
				skipFetch = true
			}
		}
		if !skipFetch {
			c.log.Debug().Str("player", username).
				Str("etag", etag).
				Bool("avatar_set", ghost.AvatarSet).
				Msg("Henter avatar")
			result, err := c.AvatarFetch.Fetch(ctx, username, etag)
			if err != nil {
				c.log.Warn().Err(err).Str("player", username).
					Msg("Avatar-henting feilet")
			} else if result.Changed && result.Data != nil {
				c.log.Debug().Str("player", username).
					Int("bytes", len(result.Data)).
					Msg("Ny avatar mottatt, laster opp")
				info.Avatar = &bridgev2.Avatar{
					ID: networkid.AvatarID(fmt.Sprintf("mc-avatar-%s-%s",
						username, result.ETag)),
					Get: func(ctx context.Context) ([]byte, error) {
						return result.Data, nil
					},
				}
				info.ExtraUpdates = bridgev2.MergeExtraUpdaters(info.ExtraUpdates,
					func(ctx context.Context, ghost *bridgev2.Ghost) bool {
						meta, ok := ghost.Metadata.(*GhostMetadata)
						if !ok {
							return false
						}
						meta.AvatarETag = result.ETag
						meta.AvatarFetchedAt = time.Now()
						return true
					})
			} else {
				c.log.Debug().Str("player", username).
					Bool("changed", result.Changed).
					Msg("Avatar uendret (304)")
				// Oppdater FetchedAt selv ved 304 for å resette TTL
				info.ExtraUpdates = bridgev2.MergeExtraUpdaters(info.ExtraUpdates,
					func(ctx context.Context, ghost *bridgev2.Ghost) bool {
						meta, ok := ghost.Metadata.(*GhostMetadata)
						if !ok {
							return false
						}
						meta.AvatarFetchedAt = time.Now()
						return true
					})
			}
		}
	}

	return info, nil
}

func (c *MCClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	return &event.RoomFeatures{
		MaxTextLength: 256,
		ReadReceipts:  false,
	}
}

func (c *MCClient) HandleMatrixMessage(ctx context.Context,
	msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {

	content := msg.Content

	// Kun tekstmeldinger
	if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice {
		return nil, fmt.Errorf("meldingstypen %s støttes ikke", content.MsgType)
	}

	text := content.Body
	if text == "" {
		return nil, fmt.Errorf("tom melding")
	}

	// Hent avsendernavn fra Matrix
	senderName := string(msg.Event.Sender)
	if msg.OrigSender != nil {
		senderName = msg.OrigSender.FormattedName
	} else {
		member, err := msg.Portal.Bridge.Matrix.GetMemberInfo(ctx,
			msg.Portal.MXID, msg.Event.Sender)
		if err == nil && member != nil && member.Displayname != "" {
			senderName = member.Displayname
		}
	}

	// Send via RCON tellraw
	if err := c.RCON.SendMessage(ctx, senderName, text); err != nil {
		return nil, fmt.Errorf("RCON SendMessage feilet: %w", err)
	}

	msgID := networkid.MessageID(fmt.Sprintf("matrix-%s-%d",
		msg.Event.ID, time.Now().UnixMilli()))

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID: msgID,
		},
	}, nil
}

// receiveLoop leser ChatLine fra log-tailing og sender til broen.
func (c *MCClient) receiveLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-c.lineCh:
			c.handleChatLine(line)
		}
	}
}

func (c *MCClient) handleChatLine(line ChatLine) {
	c.log.Debug().
		Str("player", line.PlayerName).
		Str("message", line.Message).
		Int("event", int(line.Event)).
		Msg("Minecraft hendelse mottatt")

	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[ChatLine]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(ctx zerolog.Context) zerolog.Context {
				return ctx.
					Str("player", line.PlayerName).
					Str("container", line.ContainerName).
					Int("event_type", int(line.Event))
			},
			PortalKey:    makePortalKey(line.ContainerName),
			Sender:       bridgev2.EventSender{Sender: networkid.UserID(line.PlayerName)},
			CreatePortal: true,
			Timestamp:    line.Timestamp,
		},
		Data: line,
		ID:   networkid.MessageID(fmt.Sprintf("mc-%s-%d", line.PlayerName, line.Timestamp.UnixNano())),
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal,
			intent bridgev2.MatrixAPI, data ChatLine) (*bridgev2.ConvertedMessage, error) {
			msgType := event.MsgText
			body := data.Message

			switch data.Event {
			case EventJoin:
				msgType = event.MsgNotice
				body = "☑ " + data.PlayerName + " " + data.Message
			case EventLeave:
				msgType = event.MsgNotice
				body = "☐ " + data.PlayerName + " " + data.Message
			case EventDeath:
				msgType = event.MsgNotice
				body = "☠ " + data.PlayerName + " " + data.Message
			case EventAdvancement:
				msgType = event.MsgNotice
				body = "🏆 " + data.PlayerName + " " + data.Message
			}

			return &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: msgType,
						Body:    body,
					},
				}},
			}, nil
		},
	})
}

// watchPlayers poller RCON "list" og sender join/leave-events
// ved å sammenligne med forrige spiller-liste.
func (c *MCClient) watchPlayers(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	known := make(map[string]bool)
	firstRun := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			players, err := c.RCON.List()
			if err != nil {
				c.log.Warn().Err(err).Msg("RCON list feilet")
				// Prøv å koble til RCON på nytt
				if reconnErr := c.RCON.Connect(ctx); reconnErr != nil {
					c.log.Warn().Err(reconnErr).Msg("RCON reconnect feilet")
				}
				continue
			}

			current := make(map[string]bool, len(players))
			for _, p := range players {
				current[p] = true
			}

			if firstRun {
				// Første gang: bare lagre listen, ikke send events
				known = current
				firstRun = false
				c.log.Debug().Int("players", len(known)).Msg("Initiell spiller-liste hentet")
				continue
			}

			// Finn nye spillere (joined)
			for p := range current {
				if !known[p] {
					c.log.Debug().Str("player", p).Msg("Spiller koblet til (RCON)")
					c.handleChatLine(ChatLine{
						PlayerName:    p,
						Message:       "joined the game",
						Timestamp:     time.Now(),
						ContainerName: c.Meta.ContainerName,
						Event:         EventJoin,
					})
				}
			}

			// Finn spillere som har forlatt (left)
			for p := range known {
				if !current[p] {
					c.log.Debug().Str("player", p).Msg("Spiller koblet fra (RCON)")
					c.handleChatLine(ChatLine{
						PlayerName:    p,
						Message:       "left the game",
						Timestamp:     time.Now(),
						ContainerName: c.Meta.ContainerName,
						Event:         EventLeave,
					})
				}
			}

			known = current
		}
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
