package connector

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// MCAdminLogin implements bridgev2.LoginProcess and LoginProcessUserInput.
// Only login flow: provide a provisioning secret.
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

	// Create admin UserLogin
	loginID := networkid.UserLoginID(
		"admin:" + strings.TrimPrefix(string(l.User.MXID), "@"))

	ul, err := l.User.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: "Admin",
		Metadata: &MCLoginMetadata{
			CreatedAt: time.Now(),
		},
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = &MCAdminClient{
				UserLogin: login,
				Connector: l.Connector,
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create admin login: %w", err)
	}

	// Start auto-provisioning in the background
	go func() {
		bgCtx := context.Background()
		if err := l.Connector.provisioner.SyncAll(bgCtx, ul); err != nil {
			l.Connector.log.Error().Err(err).Msg("Initial server sync failed")
		}
		go l.Connector.provisioner.WatchEvents(bgCtx, ul)
	}()

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "fi.mau.minecraft.complete",
		Instructions: "Authorized. Scanning for Minecraft servers...",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (l *MCAdminLogin) Cancel() {}
