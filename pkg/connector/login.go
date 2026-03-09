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

// MCAdminLogin implementerer bridgev2.LoginProcess og LoginProcessUserInput.
// Eneste login-flow: oppgi provisioning secret.
type MCAdminLogin struct {
	User      *bridgev2.User
	Connector *MCConnector
}

var _ bridgev2.LoginProcessUserInput = (*MCAdminLogin)(nil)

func (l *MCAdminLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "fi.mau.minecraft.enter_secret",
		Instructions: "Oppgi provisioning secret for å autorisere Minecraft-broen.",
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

	// Sjekk permissions – mautrix-go evaluerer permissions-blokken i config.yaml
	if !l.User.Permissions.Admin {
		return nil, fmt.Errorf("kun admin kan provisjonere denne broen")
	}

	// Valider provisioning secret
	// Env-variabel har prioritet over config-fil
	expected := os.Getenv("PROVISIONING_SECRET")
	if expected == "" {
		expected = l.Connector.Config.ProvisioningSecret
	}
	if expected == "" {
		return nil, fmt.Errorf("provisioning secret er ikke konfigurert på serveren")
	}
	if input["provisioning_secret"] != expected {
		return nil, fmt.Errorf("ugyldig provisioning secret")
	}

	// Opprett admin UserLogin
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
		return nil, fmt.Errorf("kunne ikke opprette admin-login: %w", err)
	}

	// Start auto-provisjonering i bakgrunnen
	go func() {
		bgCtx := context.Background()
		if err := l.Connector.provisioner.SyncAll(bgCtx, ul); err != nil {
			l.Connector.log.Error().Err(err).Msg("Initial server-sync feilet")
		}
		go l.Connector.provisioner.WatchEvents(bgCtx, ul)
	}()

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "fi.mau.minecraft.complete",
		Instructions: "Autorisert. Scanner etter Minecraft-servere...",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func (l *MCAdminLogin) Cancel() {}
