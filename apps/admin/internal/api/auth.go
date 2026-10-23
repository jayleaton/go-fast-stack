package api

//go:generate go tool mockgen -source=auth.go -destination=mocks/auth.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// AuthResult and TOTPSetup are the client's types (apiclient).
type (
	AuthResult = apiclient.AuthResult
	TOTPSetup  = apiclient.TOTPSetup
)

// Authenticator is the port the admin sign-in endpoints call: the admin
// auth instance, adapted by server.Authenticator (which captures the
// cookies auth would set and translates its errors into apiclient.Err*,
// and a two-factor challenge into *apiclient.TwoFactorChallenge). Auditing
// happens inside auth (Config.Events), whichever path a sign-in takes.
type Authenticator interface {
	SignIn(ctx context.Context, input validate.SignIn) (AuthResult, error)
	// SignOut ends the session with sessionToken ("" when signed out) and
	// returns the cookie that clears it.
	SignOut(ctx context.Context, sessionToken string) (AuthResult, error)
	SetPassword(ctx context.Context, input validate.SetPassword) (AuthResult, error)
	RequestPasswordReset(ctx context.Context, input validate.CreatePasswordReset) error
	VerifyTwoFactor(ctx context.Context, input validate.VerifyTwoFactor) (AuthResult, error)
	StartTwoFactorSetup(ctx context.Context) (TOTPSetup, error)
	EnableTwoFactor(ctx context.Context, adminID string, input validate.EnableTwoFactor) (TOTPSetup, error)
	ConfirmTwoFactor(ctx context.Context, adminID string, input validate.ConfirmTwoFactor) error
}

// Sessions are the admin UI's sign-in flows as API endpoints, called
// in-process by api.Client (cmd/admin mounts no JSON API on the network).
// Signing in is public; enrolling a second factor on the account needs a
// session.
type Sessions struct {
	auth Authenticator
	gate *access.Gate

	signIn         Endpoint[validate.SignIn, AuthResult]
	signOut        Endpoint[NoInput, AuthResult]
	setPassword    Endpoint[validate.SetPassword, AuthResult]
	requestReset   Endpoint[validate.CreatePasswordReset, Success]
	verifyTwoFact  Endpoint[validate.VerifyTwoFactor, AuthResult]
	startSetup     Endpoint[NoInput, TOTPSetup]
	enableTwoFact  Endpoint[validate.EnableTwoFactor, TOTPSetup]
	confirmTwoFact Endpoint[validate.ConfirmTwoFactor, Success]
}

// NewSessions returns the sign-in endpoints over auth.
func NewSessions(auth Authenticator, gate *access.Gate) *Sessions {
	sessions := &Sessions{auth: auth, gate: gate}
	routes := config.APIRoutes
	sessions.signIn = Endpoint[validate.SignIn, AuthResult]{"POST", routes.SignIn, access.Public, sessions.create}
	sessions.signOut = Endpoint[NoInput, AuthResult]{"POST", routes.SignOut, access.Public, sessions.end}
	sessions.setPassword = Endpoint[validate.SetPassword, AuthResult]{"POST", routes.SetPassword, access.Public, sessions.choosePassword}
	sessions.requestReset = Endpoint[validate.CreatePasswordReset, Success]{"POST", routes.PasswordReset, access.Public, sessions.sendReset}
	sessions.verifyTwoFact = Endpoint[validate.VerifyTwoFactor, AuthResult]{"POST", routes.TwoFactor, access.Public, sessions.verify}
	sessions.startSetup = Endpoint[NoInput, TOTPSetup]{"POST", routes.TwoFactorSetup, access.Public, sessions.startEnrolment}
	sessions.enableTwoFact = Endpoint[validate.EnableTwoFactor, TOTPSetup]{"POST", routes.TwoFactorEnable, access.SignedIn, sessions.enable}
	sessions.confirmTwoFact = Endpoint[validate.ConfirmTwoFactor, Success]{"POST", routes.TwoFactorConfirm, access.SignedIn, sessions.confirm}
	return sessions
}

func (sessions *Sessions) create(request *http.Request, _ access.Caller, input validate.SignIn) (AuthResult, error) {
	return sessions.auth.SignIn(request.Context(), input)
}

func (sessions *Sessions) end(request *http.Request, caller access.Caller, _ NoInput) (AuthResult, error) {
	return sessions.auth.SignOut(request.Context(), caller.Session.Token)
}

func (sessions *Sessions) choosePassword(request *http.Request, _ access.Caller, input validate.SetPassword) (AuthResult, error) {
	return sessions.auth.SetPassword(request.Context(), input)
}

func (sessions *Sessions) sendReset(request *http.Request, _ access.Caller, input validate.CreatePasswordReset) (Success, error) {
	err := sessions.auth.RequestPasswordReset(request.Context(), input)
	return Success{err == nil}, err
}

func (sessions *Sessions) verify(request *http.Request, _ access.Caller, input validate.VerifyTwoFactor) (AuthResult, error) {
	return sessions.auth.VerifyTwoFactor(request.Context(), input)
}

func (sessions *Sessions) startEnrolment(request *http.Request, _ access.Caller, _ NoInput) (TOTPSetup, error) {
	return sessions.auth.StartTwoFactorSetup(request.Context())
}

func (sessions *Sessions) enable(request *http.Request, caller access.Caller, input validate.EnableTwoFactor) (TOTPSetup, error) {
	return sessions.auth.EnableTwoFactor(request.Context(), caller.Admin.ID, input)
}

func (sessions *Sessions) confirm(request *http.Request, caller access.Caller, input validate.ConfirmTwoFactor) (Success, error) {
	err := sessions.auth.ConfirmTwoFactor(request.Context(), caller.Admin.ID, input)
	return Success{err == nil}, err
}
