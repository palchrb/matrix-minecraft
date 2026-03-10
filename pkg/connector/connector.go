package connector

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/rs/zerolog"
	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// MCLoginMetadata er den eneste metadata-typen for alle UserLogin-rader.
// Admin-logins har kun CreatedAt satt.
// Server-logins har alle feltene satt.
type MCLoginMetadata struct {
	CreatedAt     time.Time `json:"created_at"`
	ContainerID   string    `json:"container_id,omitempty"`
	ContainerName string    `json:"container_name,omitempty"`
	DisplayName   string    `json:"display_name,omitempty"`
	RCONHost      string    `json:"rcon_host,omitempty"`
	RCONPort      int       `json:"rcon_port,omitempty"`
	RCONPassword  string    `json:"rcon_password,omitempty"`
}

// MCConnector implementerer bridgev2.NetworkConnector.
type MCConnector struct {
	br            *bridgev2.Bridge
	Config        Config
	docker        *dockerclient.Client
	provisioner   *Provisioner
	avatarFetcher *AvatarFetcher
	log           zerolog.Logger
}

var _ bridgev2.NetworkConnector = (*MCConnector)(nil)

func (mc *MCConnector) Init(bridge *bridgev2.Bridge) {
	mc.br = bridge
	mc.log = bridge.Log.With().Str("component", "mc-connector").Logger()

	// Env-variabel overstyrer config-fil for provisioning secret
	if s := os.Getenv("PROVISIONING_SECRET"); s != "" {
		mc.Config.ProvisioningSecret = s
	}
}

func (mc *MCConnector) Start(ctx context.Context) error {
	mc.log.Info().Msg("Starter Minecraft connector")

	var err error
	mc.docker, err = dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("Docker-klient feilet: %w", err)
	}

	if _, err := mc.docker.Ping(ctx); err != nil {
		return fmt.Errorf("Docker API ikke tilgjengelig: %w", err)
	}
	mc.log.Info().Msg("Docker API tilkoblet")

	avatarURL := mc.Config.AvatarAPIURL
	if avatarURL == "" {
		avatarURL = "https://starlightskins.lunareclipse.studio/render/default/%s/bust"
		mc.log.Info().Str("url", avatarURL).Msg("avatar_api_url ikke satt, bruker standard")
	}
	mc.avatarFetcher = NewAvatarFetcher(avatarURL, mc.log)
	mc.provisioner = NewProvisioner(mc.docker, mc.br, &mc.Config, mc, mc.log)

	// Ved restart: gjenopprett server-tilkoblinger hvis admin allerede er logget inn
	go mc.restoreOnRestart(ctx)

	return nil
}

// restoreOnRestart finner admin-login i cache og gjenoppretter provisjonering.
// Kalles automatisk ved bridge-oppstart.
func (mc *MCConnector) restoreOnRestart(ctx context.Context) {
	// Vent litt for at logins skal lastes fra DB
	time.Sleep(2 * time.Second)

	// Hent alle brukere med logins fra DB
	userIDs, err := mc.br.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		mc.log.Error().Err(err).Msg("Kunne ikke hente bruker-IDer for restore")
		return
	}
	for _, userID := range userIDs {
		user, err := mc.br.GetUserByMXID(ctx, userID)
		if err != nil {
			mc.log.Error().Err(err).Msg("Kunne ikke hente bruker for restore")
			continue
		}
		for _, login := range user.GetUserLogins() {
			if strings.HasPrefix(string(login.ID), "admin:") {
				mc.log.Info().Msg("Admin-login funnet, gjenoppretter server-tilkoblinger")
				if err := mc.provisioner.SyncAll(ctx, login); err != nil {
					mc.log.Error().Err(err).Msg("Restore SyncAll feilet")
				}
				go mc.provisioner.WatchEvents(ctx, login)
			}
		}
	}
}

// initServerClient initialiserer MCClient for en server-login.
// Brukes både ved første provisjonering og ved restart (LoadUserLogin).
func (mc *MCConnector) initServerClient(login *bridgev2.UserLogin) error {
	meta, ok := login.Metadata.(*MCLoginMetadata)
	if !ok || meta.ContainerName == "" {
		return fmt.Errorf("ugyldig server-login metadata")
	}
	login.Client = &MCClient{
		UserLogin:   login,
		Connector:   mc,
		Meta:        meta,
		RCON:        NewRCONClient(meta.RCONHost, meta.RCONPort, meta.RCONPassword, mc.Config, mc.log),
		LogTailer:   NewLogTailer(mc.docker, meta.ContainerName, mc.log),
		AvatarFetch: mc.avatarFetcher,
	}
	return nil
}

func (mc *MCConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		// Re-request brukerinfo på innkommende meldinger slik at avatar-henting
		// forsøkes igjen hvis den feilet første gang.
		AggressiveUpdateInfo: true,
	}
}

func (mc *MCConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

func (mc *MCConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "Minecraft",
		NetworkURL:       "https://minecraft.net",
		NetworkID:        "minecraft",
		BeeperBridgeType: "github.com/palchrb/matrix-minecraft",
		DefaultPort:      29333,
	}
}

func (mc *MCConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Ghost: func() any {
			return &GhostMetadata{}
		},
		UserLogin: func() any {
			return &MCLoginMetadata{}
		},
	}
}

func (mc *MCConnector) LoadUserLogin(ctx context.Context,
	login *bridgev2.UserLogin) error {

	if strings.HasPrefix(string(login.ID), "admin:") {
		login.Client = &MCAdminClient{
			UserLogin: login,
			Connector: mc,
		}
		return nil
	}

	if strings.HasPrefix(string(login.ID), "server:") {
		return mc.initServerClient(login)
	}

	return fmt.Errorf("ukjent login-ID format: %s", login.ID)
}

func (mc *MCConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Provisioning Secret",
		Description: "Autoriser broen med provisioning secret",
		ID:          "provisioning-secret",
	}}
}

func (mc *MCConnector) CreateLogin(ctx context.Context,
	user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != "provisioning-secret" {
		return nil, fmt.Errorf("ukjent login-flow: %s", flowID)
	}
	return &MCAdminLogin{User: user, Connector: mc}, nil
}

// makePortalKey lager portal-nøkkel for en server.
// Ingen Receiver – portalen er delt (alle bruker samme rom).
func makePortalKey(containerName string) networkid.PortalKey {
	return networkid.PortalKey{
		ID: networkid.PortalID(containerName),
		// Bevisst ingen Receiver: admin-styrt delt portal
	}
}

// Ensure GetConfig is available (defined in config.go)
var _ configupgrade.Upgrader = configupgrade.SimpleUpgrader(upgradeConfig)
