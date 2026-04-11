package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

type Provisioner struct {
	docker      *dockerclient.Client
	bridge      *bridgev2.Bridge
	config      *Config
	connector   *MCConnector
	log         zerolog.Logger
	labelPrefix string
}

func NewProvisioner(docker *dockerclient.Client, br *bridgev2.Bridge,
	cfg *Config, conn *MCConnector, log zerolog.Logger) *Provisioner {
	return &Provisioner{
		docker:      docker,
		bridge:      br,
		config:      cfg,
		connector:   conn,
		log:         log,
		labelPrefix: cfg.LabelPrefix,
	}
}

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

// SyncAll scans Docker for labeled containers and provisions them.
func (p *Provisioner) SyncAll(ctx context.Context, adminLogin *bridgev2.UserLogin) error {
	p.log.Info().Msg("Scanning Docker for MC containers")

	// Ensure the admin login's space room exists before provisioning servers.
	// All server portals will be placed inside this shared space instead of
	// each server login creating its own space.
	if _, err := adminLogin.GetSpaceRoom(ctx); err != nil {
		p.log.Warn().Err(err).
			Msg("Failed to ensure shared space exists, server logins may create their own")
	}

	containers, err := p.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", p.labelPrefix+".enable=true"),
			filters.Arg("status", "running"),
		),
	})
	if err != nil {
		return fmt.Errorf("ContainerList failed: %w", err)
	}

	p.log.Info().Int("count", len(containers)).Msg("Found labeled containers")

	for _, c := range containers {
		name := cleanName(c.Names[0])

		displayName := name
		if dn, ok := c.Labels[p.labelPrefix+".name"]; ok && dn != "" {
			displayName = dn
		}

		avatarMXC := ""
		if mxc, ok := c.Labels[p.labelPrefix+".avatar"]; ok && mxc != "" {
			avatarMXC = mxc
		}

		info, err := p.docker.ContainerInspect(ctx, c.ID)
		if err != nil {
			p.log.Warn().Err(err).Str("container", name).
				Msg("ContainerInspect failed, skipping")
			continue
		}

		rconPassword := extractEnv(info.Config.Env, "RCON_PASSWORD")
		if rconPassword == "" {
			p.log.Warn().Str("container", name).
				Msg("No RCON_PASSWORD in container env, skipping")
			continue
		}

		rconPort := p.config.DefaultRCONPort
		if portStr, ok := c.Labels[p.labelPrefix+".rcon-port"]; ok {
			fmt.Sscanf(portStr, "%d", &rconPort)
		}

		if err := p.provisionServer(ctx, adminLogin, MCLoginMetadata{
			CreatedAt:     time.Now(),
			ContainerID:   c.ID,
			ContainerName: name,
			DisplayName:   displayName,
			RCONHost:      name, // container name = hostname in Docker network
			RCONPort:      rconPort,
			RCONPassword:  rconPassword,
			AvatarMXC:     avatarMXC,
		}); err != nil {
			p.log.Error().Err(err).Str("container", name).
				Msg("Provisioning failed")
		}
	}
	return nil
}

func (p *Provisioner) provisionServer(ctx context.Context,
	adminLogin *bridgev2.UserLogin, meta MCLoginMetadata) error {

	loginID := networkid.UserLoginID("server:" + meta.ContainerName)

	// Already provisioned? Update metadata and reconnect if needed
	if existing := p.bridge.GetCachedUserLoginByID(loginID); existing != nil {
		p.log.Debug().Str("container", meta.ContainerName).
			Msg("Already provisioned, updating metadata")
		if existingMeta, ok := existing.Metadata.(*MCLoginMetadata); ok {
			changed := false
			if existingMeta.AvatarMXC != meta.AvatarMXC {
				existingMeta.AvatarMXC = meta.AvatarMXC
				changed = true
			}
			if existingMeta.DisplayName != meta.DisplayName {
				existingMeta.DisplayName = meta.DisplayName
				existing.RemoteName = meta.DisplayName
				changed = true
			}
			if changed {
				if err := existing.Save(ctx); err != nil {
					p.log.Warn().Err(err).Str("container", meta.ContainerName).
						Msg("Failed to save updated metadata")
				}
				// Update the portal so changed avatar/name is reflected in the Matrix room
				if client, ok := existing.Client.(*MCClient); ok {
					portalKey := makePortalKey(meta.ContainerName)
					if portal, err := p.bridge.GetPortalByKey(ctx, portalKey); err == nil && portal.MXID != "" {
						chatInfo, _ := client.GetChatInfo(ctx, portal)
						portal.UpdateInfo(ctx, chatInfo, existing, nil, time.Time{})
						p.log.Debug().Str("container", meta.ContainerName).
							Msg("Portal info updated after metadata change")
					}
				}
			}
		}
		// Migrate server logins that were created before the shared-space fix:
		// if this login has its own SpaceRoom (left over from the old behavior
		// where every server got its own space), move the portal into the
		// admin-owned shared space and update the DB field.
		p.migrateToSharedSpace(ctx, adminLogin, existing)
		// Reconnect if RCON is disconnected (e.g. after container restart)
		if client, ok := existing.Client.(*MCClient); ok && !client.RCON.IsConnected() {
			p.log.Info().Str("container", meta.ContainerName).
				Msg("RCON disconnected, reconnecting")
			existing.Client.Connect(ctx)
		}
		return nil
	}

	p.log.Info().Str("container", meta.ContainerName).
		Str("display_name", meta.DisplayName).
		Msg("Provisioning server")

	// Pre-populate the server login's SpaceRoom with the admin login's
	// shared space so GetSpaceRoom returns the existing space instead of
	// creating a new one when the portal is added.
	sharedSpace := adminLogin.SpaceRoom

	ul, err := adminLogin.User.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: meta.DisplayName,
		SpaceRoom:  sharedSpace,
		Metadata:   &meta,
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			return p.connector.initServerClient(login)
		},
	})
	if err != nil {
		return fmt.Errorf("NewLogin failed: %w", err)
	}

	ul.Client.Connect(ctx)
	return nil
}

// migrateToSharedSpace moves an existing server login's portal from its own
// private space (created before the shared-space fix) into the admin-owned
// shared space. It then updates the login row in the DB so GetSpaceRoom stops
// returning the old private space.
//
// The old (now orphan) space is NOT deleted — it may still contain users, so
// the admin should clean it up manually in their Matrix client.
func (p *Provisioner) migrateToSharedSpace(ctx context.Context,
	adminLogin, serverLogin *bridgev2.UserLogin) {

	sharedSpace := adminLogin.SpaceRoom
	if sharedSpace == "" || serverLogin.SpaceRoom == sharedSpace {
		return
	}

	oldSpace := serverLogin.SpaceRoom
	meta, ok := serverLogin.Metadata.(*MCLoginMetadata)
	if !ok {
		return
	}

	p.log.Info().
		Str("container", meta.ContainerName).
		Str("old_space", string(oldSpace)).
		Str("new_space", string(sharedSpace)).
		Msg("Migrating server login to shared space")

	portalKey := makePortalKey(meta.ContainerName)
	portal, err := p.bridge.GetPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		// No portal yet — just fix the DB field so the next portal creation
		// uses the shared space.
		serverLogin.SpaceRoom = sharedSpace
		if err := serverLogin.Save(ctx); err != nil {
			p.log.Warn().Err(err).Str("container", meta.ContainerName).
				Msg("Failed to save migrated SpaceRoom")
		}
		return
	}

	// Add the portal as a child of the shared space.
	via := []string{p.bridge.Matrix.ServerName()}
	if _, err := p.bridge.Bot.SendState(ctx, sharedSpace, event.StateSpaceChild, portal.MXID.String(), &event.Content{
		Parsed: &event.SpaceChildEventContent{Via: via},
	}, time.Time{}); err != nil {
		p.log.Warn().Err(err).
			Str("container", meta.ContainerName).
			Str("space", string(sharedSpace)).
			Msg("Failed to add portal to shared space")
		return
	}

	// Remove the portal from its old private space (if we still have a ref).
	if oldSpace != "" {
		if _, err := p.bridge.Bot.SendState(ctx, oldSpace, event.StateSpaceChild, portal.MXID.String(), &event.Content{
			Parsed: &event.SpaceChildEventContent{Via: nil},
		}, time.Time{}); err != nil {
			p.log.Debug().Err(err).
				Str("container", meta.ContainerName).
				Str("old_space", string(oldSpace)).
				Msg("Failed to remove portal from old private space (ignored)")
		}
	}

	// Persist the new SpaceRoom on the login.
	serverLogin.SpaceRoom = sharedSpace
	if err := serverLogin.Save(ctx); err != nil {
		p.log.Warn().Err(err).
			Str("container", meta.ContainerName).
			Msg("Failed to save migrated SpaceRoom")
	}
}


// WatchEvents listens for Docker events for container start/stop.
// Blocks — always call in a goroutine.
func (p *Provisioner) WatchEvents(ctx context.Context, adminLogin *bridgev2.UserLogin) {
	p.log.Info().Msg("Starting Docker event watcher")
	backoff := 5 * time.Second
	for {
		if err := p.watchOnce(ctx, adminLogin); err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Warn().Err(err).Dur("retry_in", backoff).
				Msg("Event stream failed, retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func (p *Provisioner) watchOnce(ctx context.Context,
	adminLogin *bridgev2.UserLogin) error {

	msgCh, errCh := p.docker.Events(ctx, types.EventsOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("event", "start"),
			filters.Arg("event", "stop"),
			filters.Arg("event", "die"),
			filters.Arg("label", p.labelPrefix+".enable=true"),
		),
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			return err
		case ev := <-msgCh:
			p.handleDockerEvent(ctx, ev, adminLogin)
		}
	}
}

func (p *Provisioner) handleDockerEvent(ctx context.Context,
	ev events.Message, adminLogin *bridgev2.UserLogin) {

	name := cleanName(ev.Actor.Attributes["name"])
	loginID := networkid.UserLoginID("server:" + name)

	switch ev.Action {
	case "start":
		p.log.Info().Str("container", name).Msg("Container started")
		// Wait briefly for RCON to start
		time.Sleep(5 * time.Second)
		if err := p.SyncAll(ctx, adminLogin); err != nil {
			p.log.Error().Err(err).Msg("SyncAll failed after container start")
		}

	case "stop", "die":
		p.log.Info().Str("container", name).Msg("Container stopped")
		if login := p.bridge.GetCachedUserLoginByID(loginID); login != nil {
			login.Client.Disconnect()
			login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateTransientDisconnect,
				Error:      "container-stopped",
				Message:    fmt.Sprintf("Container %s is not available", name),
			})
		}
	}
}
