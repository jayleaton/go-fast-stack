package api

//go:generate go tool mockgen -source=auth.go -destination=mocks/auth.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// AuthResult is apiclient.AuthResult: whether a session was created and
// the Set-Cookie lines that carry it.
type AuthResult = apiclient.AuthResult

// Authenticator is the port the sign-in endpoints call: packages/auth's
// email and password flows, adapted by server.Authenticator (which captures
// the cookies auth would set and translates auth's errors into the
// apiclient.Err* values).
type Authenticator interface {
	SignUp(ctx context.Context, input validate.SignUpEmail) (AuthResult, error)
	SignIn(ctx context.Context, input validate.SignInEmail) (AuthResult, error)
	// SignOut ends the session with sessionToken ("" when signed out) and
	// returns the cookie that clears it.
	SignOut(ctx context.Context, sessionToken string) (AuthResult, error)
	RequestPasswordReset(ctx context.Context, input validate.CreatePasswordReset) error
	ResetPassword(ctx context.Context, input validate.ResetPassword) error
	VerifyEmail(ctx context.Context, input validate.VerifyEmail) (AuthResult, error)
	SendVerificationEmail(ctx context.Context, input validate.CreateVerificationEmail) error
	CheckCallbackURL(callbackURL string) error
	ErrorForCode(code string) error
}

// Sessions are the UI's sign-in flows as API endpoints: the same gate and
// validation as every other endpoint, called in-process by api.Client. They
// are not mounted over HTTP: native clients use packages/auth's own
// endpoints (/api/auth/*), and these answer with session cookies, which
// only the UI server sets. Social sign-in endpoints (SocialProviders,
// StartSocialSignIn, SocialCallback) follow the same shape.
type Sessions struct {
	auth Authenticator
	gate *access.Gate

	signUp           Endpoint[validate.SignUpEmail, AuthResult]
	signIn           Endpoint[validate.SignInEmail, AuthResult]
	signOut          Endpoint[NoInput, AuthResult]
	requestReset     Endpoint[validate.CreatePasswordReset, Success]
	resetPassword    Endpoint[validate.ResetPassword, Success]
	verifyEmail      Endpoint[validate.VerifyEmail, AuthResult]
	sendVerification Endpoint[validate.CreateVerificationEmail, Success]
}

// NewSessions returns the sign-in endpoints over auth.
func NewSessions(auth Authenticator, gate *access.Gate) *Sessions {
	sessions := &Sessions{auth: auth, gate: gate}
	routes := config.APIRoutes
	sessions.signUp = Endpoint[validate.SignUpEmail, AuthResult]{"POST", routes.SignUp, access.Public, sessions.createUser}
	sessions.signIn = Endpoint[validate.SignInEmail, AuthResult]{"POST", routes.SignIn, access.Public, sessions.create}
	sessions.signOut = Endpoint[NoInput, AuthResult]{"POST", routes.SignOut, access.Public, sessions.end}
	sessions.requestReset = Endpoint[validate.CreatePasswordReset, Success]{"POST", routes.PasswordReset, access.Public, sessions.sendReset}
	sessions.resetPassword = Endpoint[validate.ResetPassword, Success]{"POST", routes.Password, access.Public, sessions.reset}
	sessions.verifyEmail = Endpoint[validate.VerifyEmail, AuthResult]{"POST", routes.VerifyEmail, access.Public, sessions.verify}
	sessions.sendVerification = Endpoint[validate.CreateVerificationEmail, Success]{"POST", routes.VerificationEmail, access.Public, sessions.sendVerify}
	return sessions
}

func (sessions *Sessions) createUser(request *http.Request, _ access.Caller, input validate.SignUpEmail) (AuthResult, error) {
	return sessions.auth.SignUp(request.Context(), input)
}

func (sessions *Sessions) create(request *http.Request, _ access.Caller, input validate.SignInEmail) (AuthResult, error) {
	return sessions.auth.SignIn(request.Context(), input)
}

func (sessions *Sessions) end(request *http.Request, caller access.Caller, _ NoInput) (AuthResult, error) {
	return sessions.auth.SignOut(request.Context(), caller.Session.Token)
}

func (sessions *Sessions) sendReset(request *http.Request, _ access.Caller, input validate.CreatePasswordReset) (Success, error) {
	err := sessions.auth.RequestPasswordReset(request.Context(), input)
	return Success{err == nil}, err
}

func (sessions *Sessions) reset(request *http.Request, _ access.Caller, input validate.ResetPassword) (Success, error) {
	err := sessions.auth.ResetPassword(request.Context(), input)
	return Success{err == nil}, err
}

func (sessions *Sessions) verify(request *http.Request, _ access.Caller, input validate.VerifyEmail) (AuthResult, error) {
	return sessions.auth.VerifyEmail(request.Context(), input)
}

func (sessions *Sessions) sendVerify(request *http.Request, _ access.Caller, input validate.CreateVerificationEmail) (Success, error) {
	err := sessions.auth.SendVerificationEmail(request.Context(), input)
	return Success{err == nil}, err
}
