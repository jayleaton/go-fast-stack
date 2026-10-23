// Package access is route access: who may reach a route at all. Every route
// in both transports declares a Policy where it is registered,
//
//	mux.Handle("GET "+routes.Me, api.Route(users.gate, access.SignedIn, users.get))  // JSON API
//	page(access.SignedIn, "GET "+config.AppRoutes.Dashboard, ui.dashboard)           // UI
//
// and the transport's route handler asks the one Gate to resolve the caller
// and enforce the policy before the handler runs. The transports only choose
// the response: the API answers 401/403 in the envelope, the UI redirects to
// sign-in or answers 403.
//
// This is the coarse gate. Whether this caller may act on this record (their
// own account, a document in their tenant) is a business rule and stays in
// the service, which gets the caller as a services.Actor.
//
// Extend access here, in one place, without touching middleware or handlers:
// add what a policy needs to Caller (Roles, TenantID), fill it in the
// Resolver (server.Callers), and add a Policy next to SignedIn. A route then
// opts in on its registration line: access.Role("admin"). docs/route-access.md
// has the worked examples for roles and tenants.
package access

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
)

// Session is the auth session a request came in on.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Token is the session's credential. Only sign-out uses it; it is never
	// rendered or sent (json:"-").
	Token string `json:"-"`
}

// Caller is who sent a request. On a public route a signed-out request gets
// the zero Caller. Add what your policies need here (Roles, TenantID) and
// fill it in the Resolver.
type Caller struct {
	User    services.User
	Session Session
}

// SignedIn reports whether the request carries a valid session.
func (caller Caller) SignedIn() bool { return caller.Session.ID != "" }

// Actor is the caller as the services see it.
func (caller Caller) Actor() services.Actor {
	return services.Actor{UserID: caller.User.ID, Email: caller.User.Email, SessionID: caller.Session.ID}
}

// Resolver reads the caller of a request: the zero Caller when it has no
// session. An error (a failed role or tenant lookup) is a 500, never a
// silent sign-out. server.Callers adapts packages/auth to it.
type Resolver func(request *http.Request) (Caller, error)

var (
	// ErrSignedOut means the route needs a session and the request has
	// none: 401 in the API, a redirect to sign-in in the UI.
	ErrSignedOut = errors.New("access: signed out")
	// ErrDenied means the caller is signed in but the policy refuses them:
	// 403 in both transports. Wrap it to say why in logs
	// (fmt.Errorf("%w: role %q", access.ErrDenied, role)); the response
	// never carries the reason.
	ErrDenied = errors.New("access: denied")
)

// Policy decides whether a caller may reach a route: nil lets the request
// in, ErrSignedOut or (a wrapped) ErrDenied refuse it. It sees the request so
// a policy can compare the caller with the route (a tenant in the path).
// Policies are coarse; record-level rules belong to the service.
type Policy func(request *http.Request, caller Caller) error

// Public lets everyone in; the handler still gets the caller when there is a
// session (a page that changes when signed in).
func Public(*http.Request, Caller) error { return nil }

// SignedIn needs a session.
func SignedIn(_ *http.Request, caller Caller) error {
	if !caller.SignedIn() {
		return ErrSignedOut
	}
	return nil
}

// Gate is the one place a route's access is decided. Build one in cmd and
// give it to both transports.
type Gate struct {
	resolve Resolver
}

// NewGate returns the gate over resolve.
func NewGate(resolve Resolver) *Gate {
	return &Gate{resolve: resolve}
}

// Admit resolves the caller of request and checks policy. It returns the
// request carrying the resolved caller, so API calls made while serving it
// (the UI's in-process api.Client) reuse it instead of resolving again; only
// a Gate can attach one. Only the route handlers (api.Route, api.Endpoint,
// the UI's page and action) call it (forbidigo).
func (gate *Gate) Admit(request *http.Request, policy Policy) (Caller, *http.Request, error) {
	caller, err := gate.callerOf(request)
	if err != nil {
		return Caller{}, request, err
	}
	request = request.WithContext(context.WithValue(request.Context(), resolvedKey{}, resolved{gate: gate, caller: caller}))
	if err := policy(request, caller); err != nil {
		return caller, request, err
	}
	return caller, request, nil
}

// resolvedKey holds the caller a Gate already resolved for a request.
type resolvedKey struct{}

type resolved struct {
	gate   *Gate
	caller Caller
}

func (gate *Gate) callerOf(request *http.Request) (Caller, error) {
	if earlier, ok := request.Context().Value(resolvedKey{}).(resolved); ok && earlier.gate == gate {
		return earlier.caller, nil
	}
	return gate.resolve(request)
}

// FromContext adapts a context reader (auth.UserFrom behind an adapter, a
// test session) to a Resolver.
func FromContext(read func(ctx context.Context) (Caller, bool)) Resolver {
	return func(request *http.Request) (Caller, error) {
		if caller, ok := read(request.Context()); ok {
			return caller, nil
		}
		return Caller{}, nil
	}
}
