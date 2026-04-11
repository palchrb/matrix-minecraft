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

// MCLoginMetadata is stored on the admin UserLogin.
//
// Historically this type also held per-server fields (container name, RCON
// credentials, avatar) back when each Minecraft server was modelled as its
// own UserLogin. Those fields are kept so the struct still deserialises old
// rows cleanly, but they are unused on the current single-login model and
// should be considered deprecated — per-server state now lives in-memory on
// MCClient.
type MCLoginMetadata struct {
	CreatedAt time.Time `json:"created_at"`

	// Deprecated: only used by legacy `server:*` rows left over from the
	// one-UserLogin-per-server design. Kept so the JSON unmarshal still
	// succeeds when we sweep those rows on startup.
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
	RCONHost      string `json:"rcon_host,omitempty"`
	RCONPort      int    `json:"rcon_port,omitempty"`
	RCONPassword  string `json:"rcon_password,omitempty"`
	AvatarMXC     string `json:"avatar_mxc,omitempty"`
}

// MCConnector implements bridgev2.NetworkConnector.
type MCConnector struct {
	br             *bridgev2.Bridge
	Config         Config
	docker         *dockerclient.Client
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

	// Get bot avatar from config to use as space icon (NetworkIcon)
	if mx, ok := mc.br.Matrix.(*matrix.Connector); ok {
		if avatar := mx.Config.AppService.Bot.ParsedAvatar; !avatar.IsEmpty() {
			mc.networkIconMXC = id.ContentURIString(avatar.String())
			mc.log.Info().Str("mxc", string(mc.networkIconMXC)).Msg("Space icon set from bot avatar")
		}
	}

	// Delete leftover per-server UserLogin rows from the old architecture
	// where each container was its own login. They no longer have a matching
	// Client type and bridgev2 would log an error for each on startup.
	if err := mc.cleanupLegacyServerLogins(ctx); err != nil {
		mc.log.Warn().Err(err).
			Msg("Failed to clean up legacy server:* UserLogin rows")
	}

	return nil
}

// cleanupLegacyServerLogins removes `server:*` UserLogin rows left over from
// the old one-UserLogin-per-Minecraft-server design.
//
// It uses the raw UserLoginQuery.Delete (which just issues a SQL DELETE)
// instead of UserLogin.Delete from the bridgev2 Go API, because the Go API
// would also try to delete the login's SpaceRoom — and after the shared-space
// migration those legacy rows point at the admin's space, which must not be
// deleted. The user_portal rows cascade via the schema FK, so no separate
// cleanup is needed there.
func (mc *MCConnector) cleanupLegacyServerLogins(ctx context.Context) error {
	userIDs, err := mc.br.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		return fmt.Errorf("list users with logins: %w", err)
	}
	var removed int
	for _, userID := range userIDs {
		logins, err := mc.br.DB.UserLogin.GetAllForUser(ctx, userID)
		if err != nil {
			mc.log.Warn().Err(err).
				Str("user_id", string(userID)).
				Msg("Failed to list logins for user during legacy cleanup")
			continue
		}
		for _, login := range logins {
			if !strings.HasPrefix(string(login.ID), "server:") {
				continue
			}
			mc.log.Info().
				Str("login_id", string(login.ID)).
				Msg("Removing legacy per-server UserLogin row")
			if err := mc.br.DB.UserLogin.Delete(ctx, login.ID); err != nil {
				mc.log.Warn().Err(err).
					Str("login_id", string(login.ID)).
					Msg("Failed to delete legacy login row")
				continue
			}
			removed++
		}
	}
	if removed > 0 {
		mc.log.Info().Int("count", removed).
			Msg("Legacy server:* UserLogin rows cleaned up")
	}
	return nil
}

// updateSpaceAvatar updates the space avatar for a login, but only if it has
// actually changed. Called from MCClient.Connect so the space avatar reflects
// the current bot avatar after a config change.
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

// LoadUserLogin is called by bridgev2 to hydrate an in-memory UserLogin. The
// refactored bridge only has one login shape: `admin:<mxid>`. Legacy
// `server:*` rows are cleaned up in Start() before bridgev2 has a chance to
// load them, so any row reaching this function should be an admin row.
func (mc *MCConnector) LoadUserLogin(ctx context.Context,
	login *bridgev2.UserLogin) error {

	if strings.HasPrefix(string(login.ID), "admin:") {
		login.Client = newMCClient(mc, login)
		return nil
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

// makePortalKey creates the PortalKey for a Minecraft server.
//
// Receiver is intentionally left empty: portals are shared (anyone the admin
// invites ends up in the same room), and the bridgev2 framework treats an
// empty receiver as "multi-user portal". This matches how the bridge behaved
// before the refactor, so existing portal rows keep working without any
// schema migration.
func makePortalKey(containerName string) networkid.PortalKey {
	return networkid.PortalKey{
		ID: networkid.PortalID(containerName),
	}
}

// Ensure GetConfig is available (defined in config.go)
var _ configupgrade.Upgrader = configupgrade.SimpleUpgrader(upgradeConfig)
