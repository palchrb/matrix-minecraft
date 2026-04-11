package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// mcServer is the internal state for a single Minecraft container the bridge
// is connected to. One MCClient manages many mcServer instances — one per
// labeled container discovered via Docker.
type mcServer struct {
	containerName string
	containerID   string
	displayName   string
	avatarMXC     string

	rcon      *RCONClient
	logTailer *LogTailer

	lineCh chan ChatLine

	cancel context.CancelFunc
}

// MCClient implements bridgev2.NetworkAPI for the admin user.
//
// A single MCClient owns one Matrix Space (the admin login's SpaceRoom) and
// one portal per discovered Minecraft container, keyed by container name.
// This matches the pattern used by matrix-garmin-messenger: one UserLogin,
// many portals — instead of the previous one-UserLogin-per-server design.
type MCClient struct {
	UserLogin *bridgev2.UserLogin
	Connector *MCConnector

	AvatarFetch *AvatarFetcher

	servers   map[string]*mcServer
	serversMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	log    zerolog.Logger
}

var _ bridgev2.NetworkAPI = (*MCClient)(nil)

func newMCClient(conn *MCConnector, login *bridgev2.UserLogin) *MCClient {
	return &MCClient{
		UserLogin:   login,
		Connector:   conn,
		AvatarFetch: conn.avatarFetcher,
		servers:     make(map[string]*mcServer),
		log: conn.log.With().
			Str("login_id", string(login.ID)).
			Logger(),
	}
}

// Connect is called by bridgev2 on startup for every loaded login, and
// manually by the login flow after the admin provides the provisioning
// secret. It ensures the admin's shared space exists, discovers all
// labeled Minecraft containers, and starts the Docker event watcher.
func (c *MCClient) Connect(ctx context.Context) {
	// Cancel any previous Connect()'s background work before starting fresh.
	if c.cancel != nil {
		c.cancel()
	}
	bgCtx, cancel := context.WithCancel(c.UserLogin.Bridge.BackgroundCtx)
	c.ctx = bgCtx
	c.cancel = cancel

	// Make sure the admin login's Matrix Space is created eagerly. All
	// per-server portals will be added to this single shared space by
	// bridgev2's MarkInPortal flow.
	if _, err := c.UserLogin.GetSpaceRoom(ctx); err != nil {
		c.log.Warn().Err(err).Msg("Failed to ensure shared space room exists")
	}

	// Update the space avatar to match the bot avatar in config, in case
	// it changed.
	c.Connector.updateSpaceAvatar(ctx, c.UserLogin)

	if err := c.discoverAll(ctx); err != nil {
		c.log.Error().Err(err).Msg("Initial Docker discovery failed")
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "docker-discovery-failed",
			Message:    fmt.Sprintf("Initial Docker discovery failed: %v", err),
		})
		return
	}

	go c.watchDockerEvents(bgCtx)

	c.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateConnected,
	})
}

// Disconnect stops all per-server connections and the Docker event watcher.
func (c *MCClient) Disconnect() {
	c.log.Info().Msg("Disconnecting admin client")
	if c.cancel != nil {
		c.cancel()
	}
	c.serversMu.Lock()
	defer c.serversMu.Unlock()
	for name, s := range c.servers {
		c.log.Debug().Str("container", name).Msg("Stopping server")
		if s.cancel != nil {
			s.cancel()
		}
		s.rcon.Disconnect()
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

// getServer returns the internal mcServer for a given container name, or nil.
func (c *MCClient) getServer(containerName string) *mcServer {
	c.serversMu.Lock()
	defer c.serversMu.Unlock()
	return c.servers[containerName]
}

// GetChatInfo returns the ChatInfo for a portal. portal.ID is the container
// name — look it up in the servers map to find display name and avatar.
func (c *MCClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	s := c.getServer(string(portal.ID))

	// Default name = container name (portal.ID), in case the server was
	// removed between events.
	name := string(portal.ID)
	var avatarMXC string
	if s != nil {
		if s.displayName != "" {
			name = s.displayName
		}
		avatarMXC = s.avatarMXC
	}

	info := &bridgev2.ChatInfo{
		Name: &name,
		Type: ptrTo(database.RoomTypeDefault),
		Members: &bridgev2.ChatMemberList{
			IsFull: true,
			Members: []bridgev2.ChatMember{{
				EventSender: bridgev2.EventSender{
					IsFromMe: true,
				},
				Membership: event.MembershipJoin,
				PowerLevel: ptrInt(50),
			}},
		},
	}

	if avatarMXC != "" {
		info.Avatar = &bridgev2.Avatar{
			ID:  networkid.AvatarID("label-avatar-" + string(portal.ID)),
			MXC: id.ContentURIString(avatarMXC),
		}
	}
	return info, nil
}

func ptrInt(v int) *int {
	return &v
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

	// Fetch avatar from Starlight Skins API.
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
					Msg("Avatar unchanged (304)")
				// No DB update needed on 304 — the existing ETag and TTL
				// will naturally expire and trigger a new fetch later.
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

// HandleMatrixMessage routes a Matrix message to the correct server's RCON.
// portal.ID is the container name, so we look it up in the servers map.
func (c *MCClient) HandleMatrixMessage(ctx context.Context,
	msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {

	content := msg.Content

	// Only text messages
	if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice {
		return nil, fmt.Errorf("message type %s is not supported", content.MsgType)
	}

	text := content.Body
	if text == "" {
		return nil, fmt.Errorf("empty message")
	}

	containerName := string(msg.Portal.ID)
	s := c.getServer(containerName)
	if s == nil {
		return nil, fmt.Errorf("no active server for portal %s", containerName)
	}

	// Get sender name from Matrix
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
	if err := s.rcon.SendMessage(ctx, senderName, text); err != nil {
		return nil, fmt.Errorf("RCON SendMessage failed: %w", err)
	}

	msgID := networkid.MessageID(fmt.Sprintf("matrix-%s-%d",
		msg.Event.ID, time.Now().UnixMilli()))

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID: msgID,
		},
	}, nil
}

// ─── Docker discovery & event watching ───────────────────────────────────────

// discoverAll scans Docker for labeled containers and starts an mcServer for
// each one that isn't already tracked. Containers that are no longer running
// are stopped.
func (c *MCClient) discoverAll(ctx context.Context) error {
	labelPrefix := c.Connector.Config.LabelPrefix
	if labelPrefix == "" {
		labelPrefix = "mc-bridge"
	}

	containers, err := c.Connector.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelPrefix+".enable=true"),
			filters.Arg("status", "running"),
		),
	})
	if err != nil {
		return fmt.Errorf("ContainerList failed: %w", err)
	}

	c.log.Info().Int("count", len(containers)).Msg("Found labeled containers")

	found := make(map[string]bool, len(containers))
	for _, ct := range containers {
		name := cleanName(ct.Names[0])
		found[name] = true

		displayName := name
		if dn, ok := ct.Labels[labelPrefix+".name"]; ok && dn != "" {
			displayName = dn
		}

		avatarMXC := ""
		if mxc, ok := ct.Labels[labelPrefix+".avatar"]; ok && mxc != "" {
			avatarMXC = mxc
		}

		info, err := c.Connector.docker.ContainerInspect(ctx, ct.ID)
		if err != nil {
			c.log.Warn().Err(err).Str("container", name).
				Msg("ContainerInspect failed, skipping")
			continue
		}

		rconPassword := extractEnv(info.Config.Env, "RCON_PASSWORD")
		if rconPassword == "" {
			c.log.Warn().Str("container", name).
				Msg("No RCON_PASSWORD in container env, skipping")
			continue
		}

		rconPort := c.Connector.Config.DefaultRCONPort
		if portStr, ok := ct.Labels[labelPrefix+".rcon-port"]; ok {
			fmt.Sscanf(portStr, "%d", &rconPort)
		}

		if err := c.startServer(ctx, mcServerParams{
			ContainerID:   ct.ID,
			ContainerName: name,
			DisplayName:   displayName,
			AvatarMXC:     avatarMXC,
			RCONHost:      name,
			RCONPort:      rconPort,
			RCONPassword:  rconPassword,
		}); err != nil {
			c.log.Error().Err(err).Str("container", name).
				Msg("Failed to start server")
		}
	}

	// Stop any tracked servers whose containers are no longer running.
	c.serversMu.Lock()
	var toStop []string
	for name := range c.servers {
		if !found[name] {
			toStop = append(toStop, name)
		}
	}
	c.serversMu.Unlock()
	for _, name := range toStop {
		c.stopServer(name, "container not running")
	}

	return nil
}

type mcServerParams struct {
	ContainerID   string
	ContainerName string
	DisplayName   string
	AvatarMXC     string
	RCONHost      string
	RCONPort      int
	RCONPassword  string
}

// startServer adds a new mcServer to the internal map, connects RCON, starts
// log tailing, and ensures the Matrix portal exists. If the server is
// already tracked, it updates mutable fields and reconnects RCON if needed.
func (c *MCClient) startServer(ctx context.Context, p mcServerParams) error {
	c.serversMu.Lock()
	existing, ok := c.servers[p.ContainerName]
	if ok {
		// Update mutable metadata in place.
		changed := false
		if existing.displayName != p.DisplayName {
			existing.displayName = p.DisplayName
			changed = true
		}
		if existing.avatarMXC != p.AvatarMXC {
			existing.avatarMXC = p.AvatarMXC
			changed = true
		}
		c.serversMu.Unlock()

		if changed {
			c.updatePortalInfo(ctx, p.ContainerName)
		}
		if !existing.rcon.IsConnected() {
			c.log.Info().Str("container", p.ContainerName).
				Msg("RCON disconnected, reconnecting")
			if err := existing.rcon.Connect(ctx); err != nil {
				c.log.Warn().Err(err).Str("container", p.ContainerName).
					Msg("RCON reconnect failed")
			}
		}
		return nil
	}

	// Brand-new server — build state, attach RCON + log tailer, and create
	// the portal.
	srvCtx, srvCancel := context.WithCancel(c.ctx)
	srv := &mcServer{
		containerName: p.ContainerName,
		containerID:   p.ContainerID,
		displayName:   p.DisplayName,
		avatarMXC:     p.AvatarMXC,
		rcon: NewRCONClient(p.RCONHost, p.RCONPort, p.RCONPassword,
			c.Connector.Config, c.log),
		logTailer: NewLogTailer(c.Connector.docker, p.ContainerName, c.log),
		lineCh:    make(chan ChatLine, 64),
		cancel:    srvCancel,
	}
	c.servers[p.ContainerName] = srv
	c.serversMu.Unlock()

	c.log.Info().
		Str("container", p.ContainerName).
		Str("display_name", p.DisplayName).
		Msg("Starting server")

	if err := srv.rcon.Connect(ctx); err != nil {
		c.log.Warn().Err(err).Str("container", p.ContainerName).
			Msg("RCON connection failed")
		// Keep the server tracked anyway so we retry on the next event.
	}

	go srv.logTailer.Start(srvCtx, srv.lineCh)
	go c.receiveLoop(srvCtx, srv)

	// Ensure the Matrix portal exists for this container.
	portalKey := makePortalKey(p.ContainerName)
	portal, err := c.UserLogin.Bridge.GetPortalByKey(ctx, portalKey)
	if err != nil {
		c.log.Warn().Err(err).Str("container", p.ContainerName).
			Msg("GetPortalByKey failed")
		return nil
	}

	chatInfo, _ := c.GetChatInfo(ctx, portal)
	if portal.MXID == "" {
		if createErr := portal.CreateMatrixRoom(ctx, c.UserLogin, chatInfo); createErr != nil {
			c.log.Warn().Err(createErr).Str("container", p.ContainerName).
				Msg("Failed to create Matrix room")
		} else {
			c.log.Info().
				Str("container", p.ContainerName).
				Str("room", string(portal.MXID)).
				Msg("Matrix room created for server")
		}
	} else {
		// Existing room: refresh name/avatar and re-mark the admin as being
		// in this portal so it gets added to the shared space.
		portal.UpdateInfo(ctx, chatInfo, c.UserLogin, nil, time.Time{})
	}

	return nil
}

// updatePortalInfo pushes a fresh ChatInfo into the portal for a running
// server after its display name or avatar has changed.
func (c *MCClient) updatePortalInfo(ctx context.Context, containerName string) {
	portalKey := makePortalKey(containerName)
	portal, err := c.UserLogin.Bridge.GetPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		return
	}
	chatInfo, _ := c.GetChatInfo(ctx, portal)
	portal.UpdateInfo(ctx, chatInfo, c.UserLogin, nil, time.Time{})
	c.log.Debug().Str("container", containerName).
		Msg("Portal info updated")
}

// stopServer tears down a single mcServer and removes it from the map. The
// Matrix room is intentionally left in place.
func (c *MCClient) stopServer(containerName, reason string) {
	c.serversMu.Lock()
	srv, ok := c.servers[containerName]
	if !ok {
		c.serversMu.Unlock()
		return
	}
	delete(c.servers, containerName)
	c.serversMu.Unlock()

	c.log.Info().Str("container", containerName).Str("reason", reason).
		Msg("Stopping server")
	if srv.cancel != nil {
		srv.cancel()
	}
	srv.rcon.Disconnect()
}

// watchDockerEvents listens for Docker container start/stop/die events and
// adjusts internal server state accordingly. Blocks until ctx is cancelled.
func (c *MCClient) watchDockerEvents(ctx context.Context) {
	labelPrefix := c.Connector.Config.LabelPrefix
	if labelPrefix == "" {
		labelPrefix = "mc-bridge"
	}

	c.log.Info().Msg("Starting Docker event watcher")
	backoff := 5 * time.Second
	for {
		if err := c.watchOnce(ctx, labelPrefix); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn().Err(err).Dur("retry_in", backoff).
				Msg("Event stream failed, retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func (c *MCClient) watchOnce(ctx context.Context, labelPrefix string) error {
	msgCh, errCh := c.Connector.docker.Events(ctx, types.EventsOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("event", "start"),
			filters.Arg("event", "stop"),
			filters.Arg("event", "die"),
			filters.Arg("label", labelPrefix+".enable=true"),
		),
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return err
		case ev := <-msgCh:
			c.handleDockerEvent(ctx, ev)
		}
	}
}

func (c *MCClient) handleDockerEvent(ctx context.Context, ev events.Message) {
	name := cleanName(ev.Actor.Attributes["name"])

	switch ev.Action {
	case "start":
		c.log.Info().Str("container", name).Msg("Container started")
		// Wait briefly for RCON to come up.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if err := c.discoverAll(ctx); err != nil {
			c.log.Error().Err(err).Msg("discoverAll failed after container start")
		}
	case "stop", "die":
		c.log.Info().Str("container", name).Msg("Container stopped")
		c.stopServer(name, string(ev.Action))
	}
}

// extractEnv is a small helper used when parsing RCON_PASSWORD out of a
// container's env list.
func extractEnv(envList []string, key string) string {
	prefix := key + "="
	for _, e := range envList {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}

func cleanName(name string) string {
	return strings.TrimPrefix(name, "/")
}

// ─── Log tailer → Matrix ─────────────────────────────────────────────────────

// receiveLoop reads ChatLines from a single server's log tailer and sends
// them into the bridge as remote events, keyed by container name.
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
		c.log.Debug().
			Str("player", line.PlayerName).
			Int("event", int(line.Event)).
			Msg("Dropping non-chat event (bridge_all_events=false)")
		return
	}

	c.log.Debug().
		Str("player", line.PlayerName).
		Str("message", line.Message).
		Int("event", int(line.Event)).
		Msg("Minecraft event received")

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

func ptrTo[T any](v T) *T {
	return &v
}
