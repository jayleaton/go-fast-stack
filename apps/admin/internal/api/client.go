package api

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
)

// Client is apiclient.Client in-process: each method calls one Endpoint
// through Endpoint.Call, so the admin UI's reads and writes take the same
// access policy, validation and handler as the HTTP request would, without
// a network hop. ctx must be the context of the request being served (it
// carries the session the admin auth middleware loaded).
type Client struct {
	users     *Users
	admins    *Admins
	audit     *Audit
	analytics *Analytics
	sessions  *Sessions
}

var _ apiclient.Client = (*Client)(nil)

// NewClient returns the in-process client over the endpoint groups.
func NewClient(users *Users, admins *Admins, audit *Audit, analytics *Analytics, sessions *Sessions) *Client {
	return &Client{users: users, admins: admins, audit: audit, analytics: analytics, sessions: sessions}
}

func id(value string) PathValue { return PathValue{Name: "id", Value: value} }

// ListUsers is GET /users.
func (client *Client) ListUsers(ctx context.Context, input apiclient.ListUsers) (apiclient.UserPage, error) {
	return client.users.list.Call(ctx, client.users.gate, input)
}

// GetUser is GET /users/{id}.
func (client *Client) GetUser(ctx context.Context, userID string) (apiclient.UserDetail, error) {
	return client.users.get.Call(ctx, client.users.gate, NoInput{}, id(userID))
}

// BanUser is POST /users/{id}/ban.
func (client *Client) BanUser(ctx context.Context, userID string, input apiclient.BanUser) error {
	_, err := client.users.ban.Call(ctx, client.users.gate, input, id(userID))
	return err
}

// UnbanUser is POST /users/{id}/unban.
func (client *Client) UnbanUser(ctx context.Context, userID string) error {
	_, err := client.users.unban.Call(ctx, client.users.gate, NoInput{}, id(userID))
	return err
}

// SetRole is PUT /users/{id}/role.
func (client *Client) SetRole(ctx context.Context, userID string, input apiclient.SetUserRole) error {
	_, err := client.users.setRole.Call(ctx, client.users.gate, input, id(userID))
	return err
}

// RevokeSessions is DELETE /users/{id}/sessions.
func (client *Client) RevokeSessions(ctx context.Context, userID string) error {
	_, err := client.users.revokeSessions.Call(ctx, client.users.gate, NoInput{}, id(userID))
	return err
}

// RevokeSession is DELETE /users/{id}/sessions/{sessionId}.
func (client *Client) RevokeSession(ctx context.Context, userID, sessionID string) error {
	_, err := client.users.revokeSession.Call(ctx, client.users.gate, NoInput{}, id(userID), PathValue{Name: "sessionId", Value: sessionID})
	return err
}

// ListAdmins is GET /admins.
func (client *Client) ListAdmins(ctx context.Context) ([]apiclient.Admin, error) {
	return client.admins.list.Call(ctx, client.admins.gate, NoInput{})
}

// InviteAdmin is POST /admins/invites.
func (client *Client) InviteAdmin(ctx context.Context, input apiclient.CreateAdminInvite) (apiclient.Admin, error) {
	return client.admins.invite.Call(ctx, client.admins.gate, input)
}

// DisableAdmin is POST /admins/{id}/disable.
func (client *Client) DisableAdmin(ctx context.Context, adminID string) error {
	_, err := client.admins.disable.Call(ctx, client.admins.gate, NoInput{}, id(adminID))
	return err
}

// EnableAdmin is POST /admins/{id}/enable.
func (client *Client) EnableAdmin(ctx context.Context, adminID string) error {
	_, err := client.admins.enable.Call(ctx, client.admins.gate, NoInput{}, id(adminID))
	return err
}

// RemoveAdmin is DELETE /admins/{id}.
func (client *Client) RemoveAdmin(ctx context.Context, adminID string) error {
	_, err := client.admins.remove.Call(ctx, client.admins.gate, NoInput{}, id(adminID))
	return err
}

// SendPasswordReset is POST /admins/{id}/password-reset.
func (client *Client) SendPasswordReset(ctx context.Context, adminID string) error {
	_, err := client.admins.passwordReset.Call(ctx, client.admins.gate, NoInput{}, id(adminID))
	return err
}

// ResetTwoFactor is POST /admins/{id}/two-factor/reset.
func (client *Client) ResetTwoFactor(ctx context.Context, adminID string) error {
	_, err := client.admins.resetTwoFactor.Call(ctx, client.admins.gate, NoInput{}, id(adminID))
	return err
}

// ListAuditLog is GET /audit.
func (client *Client) ListAuditLog(ctx context.Context, input apiclient.ListAuditLog) (apiclient.AuditPage, error) {
	return client.audit.list.Call(ctx, client.audit.gate, input)
}

// Overview is GET /analytics.
func (client *Client) Overview(ctx context.Context) (apiclient.Overview, error) {
	return client.analytics.overview.Call(ctx, client.analytics.gate, NoInput{})
}

// SignIn is the sign-in endpoint (a *TwoFactorChallenge error when a second
// factor is needed).
func (client *Client) SignIn(ctx context.Context, input apiclient.SignIn) (AuthResult, error) {
	return client.sessions.signIn.Call(ctx, client.sessions.gate, input)
}

// SignOut is the sign-out endpoint, for the request's own session.
func (client *Client) SignOut(ctx context.Context) (AuthResult, error) {
	return client.sessions.signOut.Call(ctx, client.sessions.gate, NoInput{})
}

// SetPassword spends an invite or reset link.
func (client *Client) SetPassword(ctx context.Context, input apiclient.SetPassword) (AuthResult, error) {
	return client.sessions.setPassword.Call(ctx, client.sessions.gate, input)
}

// RequestPasswordReset emails a reset link to an admin's address.
func (client *Client) RequestPasswordReset(ctx context.Context, input apiclient.CreatePasswordReset) error {
	_, err := client.sessions.requestReset.Call(ctx, client.sessions.gate, input)
	return err
}

// VerifyTwoFactor finishes a two-factor sign-in.
func (client *Client) VerifyTwoFactor(ctx context.Context, input apiclient.VerifyTwoFactor) (AuthResult, error) {
	return client.sessions.verifyTwoFact.Call(ctx, client.sessions.gate, input)
}

// StartTwoFactorSetup enrols the SetupRequired challenge.
func (client *Client) StartTwoFactorSetup(ctx context.Context) (TOTPSetup, error) {
	return client.sessions.startSetup.Call(ctx, client.sessions.gate, NoInput{})
}

// EnableTwoFactor starts optional enrolment for the signed-in admin.
func (client *Client) EnableTwoFactor(ctx context.Context, input apiclient.EnableTwoFactor) (TOTPSetup, error) {
	return client.sessions.enableTwoFact.Call(ctx, client.sessions.gate, input)
}

// ConfirmTwoFactor confirms optional enrolment with the first code.
func (client *Client) ConfirmTwoFactor(ctx context.Context, input apiclient.ConfirmTwoFactor) error {
	_, err := client.sessions.confirmTwoFact.Call(ctx, client.sessions.gate, input)
	return err
}
