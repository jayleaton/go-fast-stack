package api

import (
	"context"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
)

// Client is apiclient.Client in-process: each method calls one endpoint of
// Users through Endpoint.Call, so the UI's reads and writes take the same
// access policy, validation and handler as the HTTP request would, without
// a network hop. ctx must be the context of the request being served (it
// carries the session auth.Middleware loaded).
type Client struct {
	users  *Users
	social *Social // nil: no social sign-in endpoints (SocialProviders is empty)
}

var _ apiclient.Client = (*Client)(nil)

// NewClient returns the in-process client over the endpoints.
func NewClient(users *Users) *Client {
	return &Client{users: users}
}

// WithSocial adds the social sign-in endpoints and returns client.
func (client *Client) WithSocial(social *Social) *Client {
	client.social = social
	return client
}

// SocialProviders is GET /auth/social/providers.
func (client *Client) SocialProviders(ctx context.Context) ([]string, error) {
	if client.social == nil {
		return []string{}, nil
	}
	list, err := client.social.providers.Call(ctx, client.social.gate, NoInput{})
	return list.Providers, err
}

// StartSocialSignIn is POST /auth/social/{provider}/start.
func (client *Client) StartSocialSignIn(ctx context.Context, provider string, input apiclient.StartSocialSignIn) (apiclient.SocialStart, error) {
	if client.social == nil {
		return apiclient.SocialStart{}, apiclient.ErrProviderNotFound
	}
	return client.social.start.Call(ctx, client.social.gate, input, PathValue{Name: "provider", Value: provider})
}

// FinishSocialSignIn is POST /auth/social/{provider}/finish.
func (client *Client) FinishSocialSignIn(ctx context.Context, provider string, input apiclient.FinishSocialSignIn) (apiclient.SocialOutcome, error) {
	if client.social == nil {
		return apiclient.SocialOutcome{}, apiclient.ErrProviderNotFound
	}
	return client.social.finish.Call(ctx, client.social.gate, input, PathValue{Name: "provider", Value: provider})
}

// Me is GET /users/me.
func (client *Client) Me(ctx context.Context) (CurrentUser, error) {
	return client.users.me.Call(ctx, client.users.gate, NoInput{})
}

// UpdateMe is PATCH /users/me.
func (client *Client) UpdateMe(ctx context.Context, input apiclient.UpdateUserSettings) (UserUpdated, error) {
	return client.users.updateSelf.Call(ctx, client.users.gate, input)
}

// RequestEmailChange is POST /users/me/email.
func (client *Client) RequestEmailChange(ctx context.Context, input apiclient.UpdateUserEmail) error {
	_, err := client.users.changeEmail.Call(ctx, client.users.gate, input)
	return err
}

// ChangePassword is POST /users/me/password.
func (client *Client) ChangePassword(ctx context.Context, input apiclient.UpdateUserPassword) error {
	_, err := client.users.changePassword.Call(ctx, client.users.gate, input)
	return err
}

// ListSessions is GET /users/me/sessions.
func (client *Client) ListSessions(ctx context.Context) ([]apiclient.Session, error) {
	return client.users.listSessions.Call(ctx, client.users.gate, NoInput{})
}

// RevokeSession is DELETE /users/me/sessions/{id}.
func (client *Client) RevokeSession(ctx context.Context, sessionID string) error {
	_, err := client.users.revokeSession.Call(ctx, client.users.gate, NoInput{}, PathValue{Name: "id", Value: sessionID})
	return err
}

// RevokeOtherSessions is DELETE /users/me/sessions/others.
func (client *Client) RevokeOtherSessions(ctx context.Context) error {
	_, err := client.users.revokeOtherSessions.Call(ctx, client.users.gate, NoInput{})
	return err
}

// DeleteMe is DELETE /users/me.
func (client *Client) DeleteMe(ctx context.Context) error {
	_, err := client.users.delete.Call(ctx, client.users.gate, NoInput{})
	return err
}

// DataSummary is GET /users/me/data.
func (client *Client) DataSummary(ctx context.Context) (apiclient.UserDataSummary, error) {
	return client.users.dataSummary.Call(ctx, client.users.gate, NoInput{})
}
