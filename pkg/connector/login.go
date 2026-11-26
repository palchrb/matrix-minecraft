package connector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

const (
	flowProvisioningSecret = "provisioning-secret"
	flowAddServer          = "add-server"
)

func adminLoginID(user *bridgev2.User) networkid.UserLoginID {
	return networkid.UserLoginID("admin:" + strings.TrimPrefix(string(user.MXID), "@"))
}

// adminClient returns the user's running admin client, or nil if they
// haven't logged in with the provisioning secret yet.
func adminClient(ctx context.Context, user *bridgev2.User) (*bridgev2.UserLogin, *MCClient) {
	ul, err := user.Bridge.GetExistingUserLoginByID(ctx, adminLoginID(user))
	if err != nil || ul == nil || ul.UserMXID != user.MXID {
		return nil, nil
	}
	client, ok := ul.Client.(*MCClient)
	if !ok {
		return nil, nil
	}
	return ul, client
}

// ─── Provisioning secret: creates the admin login ───────────────────────────

// MCAdminLogin implements the "provisioning-secret" flow, which creates the
// admin UserLogin that owns all servers and the Matrix Space.
type MCAdminLogin struct {
	User      *bridgev2.User
	Connector *MCConnector
}

var _ bridgev2.LoginProcessUserInput = (*MCAdminLogin)(nil)

func (l *MCAdminLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "fi.mau.minecraft.enter_secret",
		Instructions: "Enter the provisioning secret to authorize the Minecraft bridge.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type:    bridgev2.LoginInputFieldTypePassword,
					ID:      "provisioning_secret",
					Name:    "Provisioning Secret",
					Pattern: ".+",
				},
			},
		},
	}, nil
}

func (l *MCAdminLogin) SubmitUserInput(ctx context.Context,
	input map[string]string) (*bridgev2.LoginStep, error) {

	// Check permissions — mautrix-go evaluates the permissions block in config.yaml
	if !l.User.Permissions.Admin {
		return nil, fmt.Errorf("only admins can provision this bridge")
	}

	// Validate provisioning secret
	// Environment variable takes priority over config file
	expected := os.Getenv("PROVISIONING_SECRET")
	if expected == "" {
		expected = l.Connector.Config.ProvisioningSecret
	}
	if expected == "" {
		return nil, fmt.Errorf("provisioning secret is not configured on the server")
	}
	if input["provisioning_secret"] != expected {
		return nil, fmt.Errorf("invalid provisioning secret")
	}

	// Re-running the flow must not replace the running client: NewLogin
	// would load a second MCClient while the first keeps running.
	if ul, _ := adminClient(ctx, l.User); ul != nil {
		return &bridgev2.LoginStep{
			Type:         bridgev2.LoginStepTypeComplete,
			StepID:       "fi.mau.minecraft.complete",
			Instructions: "Already authorized. Add a server with `login add-server`.",
			CompleteParams: &bridgev2.LoginCompleteParams{
				UserLoginID: ul.ID,
				UserLogin:   ul,
			},
		}, nil
	}

	ul, err := l.User.NewLogin(ctx, &database.UserLogin{
		ID:         adminLoginID(l.User),
		RemoteName: "Admin",
		Metadata: &MCLoginMetadata{
			CreatedAt: time.Now(),
		},
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = newMCClient(l.Connector, login)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create admin login: %w", err)
	}

	// bridgev2 does not call Connect() after NewLogin — only on restart via
	// StartLogins.
	go ul.Client.Connect(ul.Log.WithContext(context.Background()))

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "fi.mau.minecraft.complete",
		Instructions: "Authorized. Now add a Minecraft server with `login add-server`.",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (l *MCAdminLogin) Cancel() {}

// ─── Add server: connects a Minecraft server's plugin ───────────────────────

// MCAddServerLogin implements the "add-server" flow. It doesn't create a new
// UserLogin: the server is added to the existing admin login, which owns
// all portals and the shared Space.
type MCAddServerLogin struct {
	User      *bridgev2.User
	Connector *MCConnector
}

var _ bridgev2.LoginProcessUserInput = (*MCAddServerLogin)(nil)

func (l *MCAddServerLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	if !l.User.Permissions.Admin {
		return nil, fmt.Errorf("only bridge admins can add servers")
	}
	if ul, _ := adminClient(ctx, l.User); ul == nil {
		return nil, fmt.Errorf("log in with `login %s` first", flowProvisioningSecret)
	}
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: "fi.mau.minecraft.add_server",
		Instructions: "Connect a Minecraft server running the matrix-minecraft plugin. " +
			"Get its token from the server console with `matrixbridge token` " +
			"(or `docker exec <container> rcon-cli matrixbridge token`).",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeURL,
					ID:   "url",
					Name: "Plugin URL",
					Description: "e.g. http://minecraft-mc-1:25580 on a Docker network, " +
						"or https://mc.example.com behind a reverse proxy",
					Validate: func(s string) (string, error) {
						u, err := parseServerURL(s)
						if err != nil {
							return "", err
						}
						return u.String(), nil
					},
				},
				{
					Type: bridgev2.LoginInputFieldTypeToken,
					ID:   "token",
					Name: "Plugin token",
				},
				{
					Type:        bridgev2.LoginInputFieldTypeUsername,
					ID:          "name",
					Name:        "Room name",
					Description: "Name of the Matrix room for this server, e.g. Minecraft survival",
				},
			},
		},
	}, nil
}

func (l *MCAddServerLogin) SubmitUserInput(ctx context.Context,
	input map[string]string) (*bridgev2.LoginStep, error) {

	if !l.User.Permissions.Admin {
		return nil, fmt.Errorf("only bridge admins can add servers")
	}
	ul, client := adminClient(ctx, l.User)
	if client == nil {
		return nil, fmt.Errorf("log in with `login %s` first", flowProvisioningSecret)
	}

	u, err := validateServerURL(ctx, input["url"])
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(input["token"])
	name := strings.TrimSpace(input["name"])
	if token == "" || name == "" {
		return nil, fmt.Errorf("token and room name are required")
	}
	cfg := ServerConfig{
		ID:    serverIDFromURL(u),
		Name:  name,
		URL:   u.String(),
		Token: token,
	}

	info, err := NewPluginStream(cfg.URL, cfg.Token, cfg.ID, l.Connector.log).Probe(ctx)
	if errors.Is(err, ErrUnauthorized) {
		return nil, fmt.Errorf("the plugin at %s rejected the token; check `matrixbridge token` in the server console", cfg.URL)
	} else if err != nil {
		return nil, fmt.Errorf("couldn't reach the plugin at %s: %w", cfg.URL, err)
	}

	roomID, err := client.AddServer(ctx, cfg)
	if err != nil {
		return nil, err
	}

	instructions := fmt.Sprintf("Connected to %s (%s) as server `%s`.", info.Server, info.MOTD, cfg.ID)
	if roomID != "" {
		instructions += fmt.Sprintf(" Room: https://matrix.to/#/%s", roomID)
	}
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "fi.mau.minecraft.server_added",
		Instructions: instructions,
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (l *MCAddServerLogin) Cancel() {}
