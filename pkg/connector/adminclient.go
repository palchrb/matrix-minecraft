package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// MCAdminClient implementerer bridgev2.NetworkAPI for admin-login.
// Admin-logins har ingen direkte servertilkobling – de styrer provisjonering.
type MCAdminClient struct {
	UserLogin *bridgev2.UserLogin
	Connector *MCConnector
}

var _ bridgev2.NetworkAPI = (*MCAdminClient)(nil)

func (c *MCAdminClient) Connect(ctx context.Context) {
	// Admin-login har ingen persistent tilkobling
}

func (c *MCAdminClient) Disconnect() {}

func (c *MCAdminClient) IsLoggedIn() bool {
	return true
}

func (c *MCAdminClient) LogoutRemote(ctx context.Context) {
	// Ingenting å rydde opp for admin
}

func (c *MCAdminClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	return false
}

func (c *MCAdminClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	return nil, fmt.Errorf("admin-login har ingen chat-info")
}

func (c *MCAdminClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	return nil, fmt.Errorf("admin-login har ingen brukerinfo")
}

func (c *MCAdminClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	return &event.RoomFeatures{}
}

func (c *MCAdminClient) HandleMatrixMessage(ctx context.Context,
	msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, fmt.Errorf("admin-login kan ikke sende meldinger")
}

// Unused but kept to satisfy the interface for future use.
var _ database.MetaTypeCreator = func() any { return &MCLoginMetadata{} }
