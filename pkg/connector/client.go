package connector

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
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

// mcServer is the running state for one configured Minecraft server.
type mcServer struct {
	cfg    ServerConfig
	stream *PluginStream
	lineCh chan ChatLine

	cancel context.CancelFunc
}

// MCClient implements bridgev2.NetworkAPI for the admin user.
//
// A single MCClient owns one Matrix Space (the admin login's SpaceRoom) and
// one portal per configured Minecraft server, keyed by server ID. Servers
// are added with "login add-server" and stored in the login's metadata.
type MCClient struct {
	UserLogin *bridgev2.UserLogin
	Connector *MCConnector

	AvatarFetch *AvatarFetcher

	// mu guards servers, ctx and the server list in the login metadata.
	mu      sync.Mutex
	servers map[string]*mcServer
	ctx     context.Context
	cancel  context.CancelFunc

	// msgSeq keeps remote message IDs unique when two events share a
	// timestamp (the plugin reports millisecond precision).
	msgSeq atomic.Uint64

	log zerolog.Logger
}

var _ bridgev2.NetworkAPI = (*MCClient)(nil)

func newMCClient(conn *MCConnector, login *bridgev2.UserLogin) *MCClient {
	ctx, cancel := context.WithCancel(login.Bridge.BackgroundCtx)
	return &MCClient{
		UserLogin:   login,
		Connector:   conn,
		AvatarFetch: conn.avatarFetcher,
		servers:     make(map[string]*mcServer),
		ctx:         ctx,
		cancel:      cancel,
		log: conn.log.With().
			Str("login_id", string(login.ID)).
			Logger(),
	}
}

func (c *MCClient) metadata() *MCLoginMetadata {
	meta, ok := c.UserLogin.Metadata.(*MCLoginMetadata)
	if !ok || meta == nil {
		meta = &MCLoginMetadata{}
		c.UserLogin.Metadata = meta
	}
	return meta
}

// Connect is called by bridgev2 on startup for every loaded login. It
// starts a plugin connection for every configured server. The admin
// login's Matrix Space is created lazily by bridgev2 the first time a
// portal triggers MarkInPortal (with the NetworkIcon we declare in
// GetName() as its initial avatar), so there's no need to pre-create it.
func (c *MCClient) Connect(ctx context.Context) {
	c.mu.Lock()
	c.stopAllLocked()
	c.ctx, c.cancel = context.WithCancel(c.UserLogin.Bridge.BackgroundCtx)
	servers := slices.Clone(c.metadata().Servers)
	c.mu.Unlock()

	if len(servers) == 0 {
		c.log.Info().Msg("No Minecraft servers configured yet; add one with `login add-server`")
	}
	for _, cfg := range servers {
		c.startServer(ctx, cfg)
	}

	// Push the current bot avatar onto the shared space in case it changed
	// in config.yaml since the space was first created.
	c.Connector.updateSpaceAvatar(ctx, c.UserLogin)

	c.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnected,
	})
}

// Disconnect stops all plugin connections.
func (c *MCClient) Disconnect() {
	c.log.Info().Msg("Disconnecting admin client")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopAllLocked()
	c.cancel()
}

func (c *MCClient) stopAllLocked() {
	for _, s := range c.servers {
		s.cancel()
	}
	c.servers = make(map[string]*mcServer)
}

func (c *MCClient) IsLoggedIn() bool {
	return true
}

func (c *MCClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *MCClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	return string(userID) == string(c.UserLogin.UserMXID)
}

// getServer returns the running server for a portal ID, or nil.
func (c *MCClient) getServer(serverID string) *mcServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.servers[serverID]
}

// ─── Server management ──────────────────────────────────────────────────────

// ServerStatus is one row of list-servers.
type ServerStatus struct {
	Config ServerConfig
	Stream StreamStatus
	Active bool
}

// AddServer stores (or updates) a server, (re)starts its plugin connection
// and makes sure its portal room exists. Returns the room ID.
func (c *MCClient) AddServer(ctx context.Context, cfg ServerConfig) (id.RoomID, error) {
	c.mu.Lock()
	meta := c.metadata()
	idx := slices.IndexFunc(meta.Servers, func(s ServerConfig) bool { return s.ID == cfg.ID })
	if idx >= 0 {
		meta.Servers[idx] = cfg
	} else {
		meta.Servers = append(meta.Servers, cfg)
	}
	err := c.UserLogin.Save(ctx)
	c.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("failed to save server: %w", err)
	}
	c.log.Info().Str("server", cfg.ID).Str("url", cfg.URL).Msg("Server added")
	return c.startServer(ctx, cfg), nil
}

// RemoveServer stops a server and forgets it. Its Matrix room is kept.
func (c *MCClient) RemoveServer(ctx context.Context, serverID string) (bool, error) {
	c.mu.Lock()
	meta := c.metadata()
	before := len(meta.Servers)
	meta.Servers = slices.DeleteFunc(meta.Servers, func(s ServerConfig) bool { return s.ID == serverID })
	removed := len(meta.Servers) != before
	if s, ok := c.servers[serverID]; ok {
		s.cancel()
		delete(c.servers, serverID)
	}
	var err error
	if removed {
		err = c.UserLogin.Save(ctx)
	}
	c.mu.Unlock()
	if removed {
		c.log.Info().Str("server", serverID).Msg("Server removed")
	}
	return removed, err
}

// ListServers returns the configured servers and their connection state.
func (c *MCClient) ListServers() []ServerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ServerStatus
	for _, cfg := range c.metadata().Servers {
		st := ServerStatus{Config: cfg}
		if s, ok := c.servers[cfg.ID]; ok {
			st.Active = true
			st.Stream = s.stream.Status()
		}
		out = append(out, st)
	}
	return out
}

// startServer (re)starts the plugin connection for a server and ensures its
// Matrix portal exists. Returns the portal's room ID, if it has one.
func (c *MCClient) startServer(ctx context.Context, cfg ServerConfig) id.RoomID {
	c.mu.Lock()
	if old, ok := c.servers[cfg.ID]; ok {
		old.cancel()
	}
	srvCtx, srvCancel := context.WithCancel(c.ctx)
	srv := &mcServer{
		cfg:    cfg,
		stream: NewPluginStream(cfg.URL, cfg.Token, cfg.ID, c.log),
		lineCh: make(chan ChatLine, 64),
		cancel: srvCancel,
	}
	c.servers[cfg.ID] = srv
	c.mu.Unlock()

	c.log.Info().Str("server", cfg.ID).Str("name", cfg.Name).Msg("Starting server")
	go srv.stream.Start(srvCtx, srv.lineCh)
	go c.receiveLoop(srvCtx, srv)

	// Ensure the Matrix portal exists and that the admin login is marked as
	// "in" it (the user_portal row). Without that row, bridgev2's
	// FindPreferredLogin can't route inbound Matrix messages to
	// HandleMatrixMessage at all.
	portal, err := c.UserLogin.Bridge.GetPortalByKey(ctx, makePortalKey(cfg.ID))
	if err != nil {
		c.log.Warn().Err(err).Str("server", cfg.ID).Msg("GetPortalByKey failed")
		return ""
	}
	chatInfo, _ := c.GetChatInfo(ctx, portal)

	// CreateMatrixRoom is idempotent: for an existing room it only marks the
	// login as being in the portal.
	if err := portal.CreateMatrixRoom(ctx, c.UserLogin, chatInfo); err != nil {
		c.log.Warn().Err(err).Str("server", cfg.ID).Msg("CreateMatrixRoom failed")
	}
	c.UserLogin.MarkInPortal(ctx, portal)

	// Push a fresh ChatInfo so the room name reflects the configured name.
	// bridgev2 skips state events that wouldn't change anything.
	if portal.MXID != "" {
		portal.UpdateInfo(ctx, chatInfo, c.UserLogin, nil, time.Time{})
	}
	c.log.Info().
		Str("server", cfg.ID).
		Str("portal_mxid", string(portal.MXID)).
		Msg("Server portal ready")
	return portal.MXID
}

// ─── Matrix ↔ Minecraft ─────────────────────────────────────────────────────

// GetChatInfo returns the ChatInfo for a portal. portal.ID is the server ID.
// No avatar is set, so a room avatar chosen in Matrix is left alone.
func (c *MCClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	name := string(portal.ID)
	if s := c.getServer(string(portal.ID)); s != nil && s.cfg.Name != "" {
		name = s.cfg.Name
	}
	return &bridgev2.ChatInfo{
		Name: &name,
		Type: ptrTo(database.RoomTypeDefault),
		Members: &bridgev2.ChatMemberList{
			IsFull: true,
			Members: []bridgev2.ChatMember{{
				EventSender: bridgev2.EventSender{
					IsFromMe: true,
				},
				Membership: event.MembershipJoin,
				PowerLevel: ptrTo(50),
			}},
		},
	}, nil
}

func (c *MCClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	// The bridge user itself is not an MC player, don't set ghost info
	if c.IsThisUser(ctx, ghost.ID) {
		return nil, nil
	}

	username := string(ghost.ID)

	info := &bridgev2.UserInfo{
		Name: &username,
	}

	// Fetch avatar (rendered from the player's skin by default).
	// Since AggressiveUpdateInfo is enabled, this is called on every message,
	// so we check if the ghost already has an avatar set to avoid
	// unnecessary HTTP calls (only re-check every 1 hour).
	if c.AvatarFetch != nil {
		var etag string
		var skipFetch bool
		if meta, ok := ghost.Metadata.(*GhostMetadata); ok {
			etag = meta.AvatarETag
			// If avatar is already set and fetched less than 1 hour ago, skip
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
				Msg("Fetching avatar")
			result, err := c.AvatarFetch.Fetch(ctx, username, etag)
			if err != nil {
				c.log.Warn().Err(err).Str("player", username).
					Msg("Avatar fetch failed")
			} else if result.Changed && result.Data != nil {
				c.log.Debug().Str("player", username).
					Int("bytes", len(result.Data)).
					Msg("New avatar received, uploading")
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
					Msg("Avatar unchanged")
				// Restart the TTL so we don't look the skin up again on
				// every message once the first hour has passed.
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

// HandleMatrixMessage shows a Matrix message in game through the server's
// plugin. An error here makes bridgev2 mark the message as not delivered.
func (c *MCClient) HandleMatrixMessage(ctx context.Context,
	msg *bridgev2.MatrixMessage) (resp *bridgev2.MatrixMessageResponse, retErr error) {

	serverID := string(msg.Portal.ID)
	log := c.log.With().
		Str("server", serverID).
		Str("matrix_event", string(msg.Event.ID)).
		Str("sender", string(msg.Event.Sender)).
		Logger()

	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Msg("HandleMatrixMessage panicked")
			retErr = fmt.Errorf("handler panicked: %v", r)
		}
		if retErr != nil {
			log.Warn().Err(retErr).Msg("Failed to send Matrix message to Minecraft")
		}
	}()

	if msg.Content == nil {
		return nil, fmt.Errorf("nil content in MatrixMessage")
	}
	content := msg.Content
	if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice {
		return nil, fmt.Errorf("message type %s is not supported", content.MsgType)
	}
	text := content.Body
	if text == "" {
		return nil, fmt.Errorf("empty message")
	}

	s := c.getServer(serverID)
	if s == nil {
		return nil, fmt.Errorf("no server configured for this room")
	}

	senderName := string(msg.Event.Sender)
	if msg.OrigSender != nil {
		senderName = msg.OrigSender.FormattedName
	} else {
		member, err := msg.Portal.Bridge.Matrix.GetMemberInfo(ctx,
			msg.Portal.MXID, msg.Event.Sender)
		if err != nil {
			log.Warn().Err(err).Msg("GetMemberInfo failed, falling back to MXID as sender name")
		} else if member != nil && member.Displayname != "" {
			senderName = member.Displayname
		}
	}

	if err := s.stream.Send(ctx, senderName, text, c.sendFormat()); err != nil {
		return nil, err
	}
	log.Debug().Str("sender_name", senderName).Msg("Matrix message shown in game")

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID: networkid.MessageID(fmt.Sprintf("matrix-%s-%d",
				msg.Event.ID, time.Now().UnixMilli())),
		},
	}, nil
}

func (c *MCClient) sendFormat() SendFormat {
	cfg := c.Connector.Config
	return SendFormat{
		Prefix:       cfg.PrefixText,
		PrefixColor:  cfg.PrefixColor,
		SenderColor:  cfg.SenderColor,
		MessageColor: cfg.MessageColor,
	}
}

// receiveLoop forwards one server's game events into the bridge.
func (c *MCClient) receiveLoop(ctx context.Context, srv *mcServer) {
	for {
		select {
		case <-ctx.Done():
			return
		case line := <-srv.lineCh:
			c.handleChatLine(line)
		}
	}
}

func (c *MCClient) handleChatLine(line ChatLine) {
	// Filter out non-chat events if bridge_all_events is false
	if !c.Connector.Config.BridgeAllEvents && line.Event != EventChat {
		return
	}

	c.UserLogin.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[ChatLine]{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventMessage,
			LogContext: func(ctx zerolog.Context) zerolog.Context {
				return ctx.
					Str("player", line.PlayerName).
					Str("server", line.ServerID).
					Int("event_type", int(line.Event))
			},
			PortalKey:    makePortalKey(line.ServerID),
			Sender:       bridgev2.EventSender{Sender: networkid.UserID(line.PlayerName)},
			CreatePortal: true,
			Timestamp:    line.Timestamp,
		},
		Data: line,
		ID: networkid.MessageID(fmt.Sprintf("mc-%s-%d-%d",
			line.PlayerName, line.Timestamp.UnixNano(), c.msgSeq.Add(1))),
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal,
			intent bridgev2.MatrixAPI, data ChatLine) (*bridgev2.ConvertedMessage, error) {
			msgType := event.MsgText
			body := data.Message
			text := data.PlayerName + " " + data.Message
			if data.FullText != "" {
				text = data.FullText
			}

			switch data.Event {
			case EventJoin:
				msgType = event.MsgNotice
				body = "☑ " + text
			case EventLeave:
				msgType = event.MsgNotice
				body = "☐ " + text
			case EventDeath:
				msgType = event.MsgNotice
				body = "☠ " + text
			case EventAdvancement:
				msgType = event.MsgNotice
				body = "🏆 " + text
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

func ptrTo[T any](v T) *T {
	return &v
}
