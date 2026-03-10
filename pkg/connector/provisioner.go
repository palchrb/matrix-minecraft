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

// SyncAll scanner Docker for merkede containere og provisjonerer dem.
func (p *Provisioner) SyncAll(ctx context.Context, adminLogin *bridgev2.UserLogin) error {
	p.log.Info().Msg("Scanner Docker etter MC-containere")

	containers, err := p.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", p.labelPrefix+".enable=true"),
			filters.Arg("status", "running"),
		),
	})
	if err != nil {
		return fmt.Errorf("ContainerList feilet: %w", err)
	}

	p.log.Info().Int("count", len(containers)).Msg("Fant merkede containere")

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
				Msg("ContainerInspect feilet, hopper over")
			continue
		}

		rconPassword := extractEnv(info.Config.Env, "RCON_PASSWORD")
		if rconPassword == "" {
			p.log.Warn().Str("container", name).
				Msg("Ingen RCON_PASSWORD i container-env, hopper over")
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
			RCONHost:      name, // container-namn = hostname i Docker-nettverk
			RCONPort:      rconPort,
			RCONPassword:  rconPassword,
			AvatarMXC:     avatarMXC,
		}); err != nil {
			p.log.Error().Err(err).Str("container", name).
				Msg("Provisjonering feilet")
		}
	}
	return nil
}

func (p *Provisioner) provisionServer(ctx context.Context,
	adminLogin *bridgev2.UserLogin, meta MCLoginMetadata) error {

	loginID := networkid.UserLoginID("server:" + meta.ContainerName)

	// Allerede provisjonert? Oppdater metadata (avatar/navn kan ha endret seg)
	if existing := p.bridge.GetCachedUserLoginByID(loginID); existing != nil {
		p.log.Debug().Str("container", meta.ContainerName).
			Msg("Allerede provisjonert, oppdaterer metadata")
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
						Msg("Kunne ikke lagre oppdatert metadata")
				}
			}
		}
		return nil
	}

	p.log.Info().Str("container", meta.ContainerName).
		Str("display_name", meta.DisplayName).
		Msg("Provisjonerer server")

	ul, err := adminLogin.User.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: meta.DisplayName,
		Metadata:   &meta,
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			return p.connector.initServerClient(login)
		},
	})
	if err != nil {
		return fmt.Errorf("NewLogin feilet: %w", err)
	}

	ul.Client.Connect(ctx)
	return nil
}

// WatchEvents lytter på Docker-events for container start/stop.
// Blokkerer – kall alltid i goroutine.
func (p *Provisioner) WatchEvents(ctx context.Context, adminLogin *bridgev2.UserLogin) {
	p.log.Info().Msg("Starter Docker event-watcher")
	backoff := 5 * time.Second
	for {
		if err := p.watchOnce(ctx, adminLogin); err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Warn().Err(err).Dur("retry_in", backoff).
				Msg("Event-stream feilet, prøver igjen")
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
		p.log.Info().Str("container", name).Msg("Container startet")
		// Vent litt så RCON rekker å starte
		time.Sleep(5 * time.Second)
		if err := p.SyncAll(ctx, adminLogin); err != nil {
			p.log.Error().Err(err).Msg("SyncAll feilet etter container start")
		}

	case "stop", "die":
		p.log.Info().Str("container", name).Msg("Container stoppet")
		if login := p.bridge.GetCachedUserLoginByID(loginID); login != nil {
			login.Client.Disconnect()
			login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateTransientDisconnect,
				Error:      "container-stopped",
				Message:    fmt.Sprintf("Container %s er ikke tilgjengelig", name),
			})
		}
	}
}
