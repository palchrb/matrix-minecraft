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
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MCLoginMetadata is the only metadata type for all UserLogin rows.
// Admin logins only have CreatedAt set.
// Server logins have all fields set.
type MCLoginMetadata struct {
	CreatedAt     time.Time `json:"created_at"`
	ContainerID   string    `json:"container_id,omitempty"`
	ContainerName string    `json:"container_name,omitempty"`
	DisplayName   string    `json:"display_name,omitempty"`
	RCONHost      string    `json:"rcon_host,omitempty"`
	RCONPort      int       `json:"rcon_port,omitempty"`
	RCONPassword  string    `json:"rcon_password,omitempty"`
	AvatarMXC     string    `json:"avatar_mxc,omitempty"` // From mc-bridge.avatar Docker label
}

// MCConnector implements bridgev2.NetworkConnector.
type MCConnector struct {
	br             *bridgev2.Bridge
	Config         Config
	docker         *dockerclient.Client
	provisioner    *Provisioner
	avatarFetcher  *AvatarFetcher
	log            zerolog.Logger
	networkIconMXC id.ContentURIString // Bot avatar from config, used as space icon
}

var _ bridgev2.NetworkConnector = (*MCConnector)(nil)

func (mc *MCConnector) Init(bridge *bridgev2.Bridge) {
	mc.br = bridge
	mc.log = bridge.Log.With().Str("component", "mc-connector").Logger()

	// Environment variable overrides config file for provisioning secret
	if s := os.Getenv("PROVISIONING_SECRET"); s != "" {
		mc.Config.ProvisioningSecret = s
	}
}

func (mc *MCConnector) Start(ctx context.Context) error {
	mc.log.Info().Msg("Starting Minecraft connector")

	var err error
	mc.docker, err = dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("Docker client failed: %w", err)
	}

	if _, err := mc.docker.Ping(ctx); err != nil {
		return fmt.Errorf("Docker API not available: %w", err)
	}
	mc.log.Info().Msg("Docker API connected")

	avatarURL := mc.Config.AvatarAPIURL
	if avatarURL == "" {
		avatarURL = "https://starlightskins.lunareclipse.studio/render/default/%s/bust"
		mc.log.Info().Str("url", avatarURL).Msg("avatar_api_url not set, using default")
	}
	mc.avatarFetcher = NewAvatarFetcher(avatarURL, mc.log)
	mc.provisioner = NewProvisioner(mc.docker, mc.br, &mc.Config, mc, mc.log)

	// Get bot avatar from config to use as space icon (NetworkIcon)
	if mx, ok := mc.br.Matrix.(*matrix.Connector); ok {
		if avatar := mx.Config.AppService.Bot.ParsedAvatar; !avatar.IsEmpty() {
			mc.networkIconMXC = id.ContentURIString(avatar.String())
			mc.log.Info().Str("mxc", string(mc.networkIconMXC)).Msg("Space icon set from bot avatar")
		}
	}

	// On restart: restore server connections if admin is already logged in
	go mc.restoreOnRestart(ctx)

	return nil
}

// restoreOnRestart finds admin login in cache and restores provisioning.
// Called automatically on bridge startup.
func (mc *MCConnector) restoreOnRestart(ctx context.Context) {
	// Wait briefly for logins to be loaded from DB
	time.Sleep(2 * time.Second)

	// Get all users with logins from DB
	userIDs, err := mc.br.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		mc.log.Error().Err(err).Msg("Failed to get user IDs for restore")
		return
	}
	for _, userID := range userIDs {
		user, err := mc.br.GetUserByMXID(ctx, userID)
		if err != nil {
			mc.log.Error().Err(err).Msg("Failed to get user for restore")
			continue
		}
		for _, login := range user.GetUserLogins() {
			if strings.HasPrefix(string(login.ID), "admin:") {
				mc.log.Info().Msg("Admin login found, restoring server connections")
				// Ensure the shared space room exists on the admin login before
				// SyncAll runs, so any migration of pre-existing server logins
				// has a target space to move their portals into.
				if _, err := login.GetSpaceRoom(ctx); err != nil {
					mc.log.Warn().Err(err).
						Msg("Failed to ensure shared space on restore")
				}
				if err := mc.provisioner.SyncAll(ctx, login); err != nil {
					mc.log.Error().Err(err).Msg("Restore SyncAll failed")
				}
				go mc.provisioner.WatchEvents(ctx, login)
				// Update the shared-space avatar. All server logins are
				// migrated to share this admin-owned space, so updating it
				// once here is enough.
				mc.updateSpaceAvatar(ctx, login)
			}
		}
	}
}

// updateSpaceAvatar updates the space avatar for an existing login,
// but only if it has actually changed.
func (mc *MCConnector) updateSpaceAvatar(ctx context.Context, login *bridgev2.UserLogin) {
	if mc.networkIconMXC == "" || login.SpaceRoom == "" {
		return
	}
	// Check current avatar first to avoid unnecessary state events
	if stateGetter, ok := mc.br.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState); ok {
		currentState, err := stateGetter.GetStateEvent(ctx, login.SpaceRoom, event.StateRoomAvatar, "")
		if err == nil && currentState != nil {
			if avatarContent := currentState.Content.AsRoomAvatar(); avatarContent != nil && avatarContent.URL == mc.networkIconMXC {
				mc.log.Debug().
					Str("space_room", string(login.SpaceRoom)).
					Msg("Space avatar already up to date, skipping")
				return
			}
		}
	}
	_, err := mc.br.Bot.SendState(ctx, login.SpaceRoom, event.StateRoomAvatar, "", &event.Content{
		Parsed: &event.RoomAvatarEventContent{
			URL: mc.networkIconMXC,
		},
	}, time.Time{})
	if err != nil {
		mc.log.Warn().Err(err).
			Str("space_room", string(login.SpaceRoom)).
			Msg("Failed to update space avatar")
	} else {
		mc.log.Info().
			Str("space_room", string(login.SpaceRoom)).
			Str("mxc", string(mc.networkIconMXC)).
			Msg("Space avatar updated")
	}
}

// initServerClient initializes MCClient for a server login.
// Used both during initial provisioning and on restart (LoadUserLogin).
func (mc *MCConnector) initServerClient(login *bridgev2.UserLogin) error {
	meta, ok := login.Metadata.(*MCLoginMetadata)
	if !ok || meta.ContainerName == "" {
		return fmt.Errorf("invalid server login metadata")
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
		// Re-request user info on incoming messages so that avatar fetching
		// is retried if it failed the first time.
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
		NetworkIcon:      mc.networkIconMXC,
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

	return fmt.Errorf("unknown login ID format: %s", login.ID)
}

func (mc *MCConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Provisioning Secret",
		Description: "Authorize the bridge with a provisioning secret",
		ID:          "provisioning-secret",
	}}
}

func (mc *MCConnector) CreateLogin(ctx context.Context,
	user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != "provisioning-secret" {
		return nil, fmt.Errorf("unknown login flow: %s", flowID)
	}
	return &MCAdminLogin{User: user, Connector: mc}, nil
}

// makePortalKey creates a portal key for a server.
// No Receiver — the portal is shared (all users use the same room).
func makePortalKey(containerName string) networkid.PortalKey {
	return networkid.PortalKey{
		ID: networkid.PortalID(containerName),
		// Intentionally no Receiver: admin-managed shared portal
	}
}

// Ensure GetConfig is available (defined in config.go)
var _ configupgrade.Upgrader = configupgrade.SimpleUpgrader(upgradeConfig)
