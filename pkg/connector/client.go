package connector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
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

	// Start log-tailing i bakgrunnen
	go c.LogTailer.Start(tailCtx, c.lineCh)
	go c.receiveLoop(tailCtx)

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
	return &bridgev2.ChatInfo{
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
	}, nil
}

func ptrInt(v int) *int {
	return &v
}

func (c *MCClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	username := string(ghost.ID)

	info := &bridgev2.UserInfo{
		Name: &username,
	}

	// Hent avatar fra MC-heads API
	if c.AvatarFetch != nil {
		var lastMod time.Time
		if meta, ok := ghost.Metadata.(*GhostMetadata); ok {
			lastMod = meta.AvatarLastModified
		}
		result, err := c.AvatarFetch.Fetch(ctx, username, lastMod)
		if err != nil {
			c.log.Warn().Err(err).Str("player", username).
				Msg("Avatar-henting feilet")
		} else if result.Changed && result.Data != nil {
			info.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(fmt.Sprintf("mc-avatar-%s-%d",
					username, result.LastModified.Unix())),
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
					meta.AvatarLastModified = result.LastModified
					meta.AvatarValid = result.AccountValid
					return true
				})
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
		Msg("Minecraft chat-melding mottatt")

	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[ChatLine]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(ctx zerolog.Context) zerolog.Context {
				return ctx.
					Str("player", line.PlayerName).
					Str("container", line.ContainerName)
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
			return &bridgev2.ConvertedMessage{
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgText,
						Body:    data.Message,
					},
				}},
			}, nil
		},
	})
}

func ptrTo[T any](v T) *T {
	return &v
}
