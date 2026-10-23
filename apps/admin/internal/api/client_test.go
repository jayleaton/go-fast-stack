package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// signedIn is the context of a UI request signed in as as (what the admin
// auth middleware leaves on it), or signed out with apitest.Anonymous.
func signedIn(test *testing.T, as apitest.Session) context.Context {
	return as(httptest.NewRequestWithContext(test.Context(), "GET", "/", nil)).Context()
}

// newClient is the in-process client over mocked services, behind the test gate.
func newClient(test *testing.T) (svcs, *api.Client) {
	ctrl := gomock.NewController(test)
	users := mocks.NewMockUserService(ctrl)
	admins := mocks.NewMockAdminService(ctrl)
	audit := mocks.NewMockAuditService(ctrl)
	analytics := mocks.NewMockAnalyticsService(ctrl)
	gate := apitest.Gate()
	client := api.NewClient(api.NewUsers(users, gate), api.NewAdmins(admins, gate), api.NewAudit(audit, gate), api.NewAnalytics(analytics, gate), api.NewSessions(mocks.NewMockAuthenticator(ctrl), gate))
	return svcs{users.EXPECT(), admins.EXPECT(), audit.EXPECT(), analytics.EXPECT()}, client
}

// TestClientMethods: every in-process client method reaches its endpoint's
// service call with the caller's actor, the path's ids and the validated
// input, as the HTTP route does.
func TestClientMethods(test *testing.T) {
	test.Parallel()
	cases := map[string]struct {
		expect func(recorders svcs)
		call   func(ctx context.Context, client *api.Client) error
	}{
		"ListUsers": {
			func(recorders svcs) {
				recorders.users.ListUsers(anyCtx, actor, gomock.Any()).Return(services.UserPage{}, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				_, err := client.ListUsers(ctx, validate.ListUsers{})
				return err
			},
		},
		"GetUser": {
			func(recorders svcs) { recorders.users.GetUser(anyCtx, actor, "u1").Return(services.UserDetail{}, nil) },
			func(ctx context.Context, client *api.Client) error {
				_, err := client.GetUser(ctx, "u1")
				return err
			},
		},
		"BanUser validates in the API": {
			func(recorders svcs) {
				recorders.users.BanUser(anyCtx, actor, "u1", validate.BanUser{BanReason: "spam"}).Return(nil)
			},
			func(ctx context.Context, client *api.Client) error {
				return client.BanUser(ctx, "u1", validate.BanUser{BanReason: "  spam  "})
			},
		},
		"UnbanUser": {
			func(recorders svcs) { recorders.users.UnbanUser(anyCtx, actor, "u1").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.UnbanUser(ctx, "u1") },
		},
		"SetRole": {
			func(recorders svcs) {
				recorders.users.SetRole(anyCtx, actor, "u1", validate.SetUserRole{Role: "editor"}).Return(nil)
			},
			func(ctx context.Context, client *api.Client) error {
				return client.SetRole(ctx, "u1", validate.SetUserRole{Role: " editor "})
			},
		},
		"RevokeSessions": {
			func(recorders svcs) { recorders.users.RevokeSessions(anyCtx, actor, "u1").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.RevokeSessions(ctx, "u1") },
		},
		"RevokeSession reads both path values": {
			func(recorders svcs) { recorders.users.RevokeSession(anyCtx, actor, "u1", "s2").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.RevokeSession(ctx, "u1", "s2") },
		},
		"ListAdmins": {
			func(recorders svcs) { recorders.admins.ListAdmins(anyCtx, actor).Return(nil, nil) },
			func(ctx context.Context, client *api.Client) error {
				_, err := client.ListAdmins(ctx)
				return err
			},
		},
		"InviteAdmin": {
			func(recorders svcs) {
				recorders.admins.InviteAdmin(anyCtx, actor, gomock.Any()).Return(services.Admin{}, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				_, err := client.InviteAdmin(ctx, validate.CreateAdminInvite{Email: "ada@example.com", Name: "Ada"})
				return err
			},
		},
		"DisableAdmin": {
			func(recorders svcs) { recorders.admins.DisableAdmin(anyCtx, actor, "a2").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.DisableAdmin(ctx, "a2") },
		},
		"EnableAdmin": {
			func(recorders svcs) { recorders.admins.EnableAdmin(anyCtx, actor, "a2").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.EnableAdmin(ctx, "a2") },
		},
		"RemoveAdmin": {
			func(recorders svcs) { recorders.admins.RemoveAdmin(anyCtx, actor, "a2").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.RemoveAdmin(ctx, "a2") },
		},
		"SendPasswordReset": {
			func(recorders svcs) { recorders.admins.SendPasswordReset(anyCtx, actor, "a2").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.SendPasswordReset(ctx, "a2") },
		},
		"ResetTwoFactor": {
			func(recorders svcs) { recorders.admins.ResetTwoFactor(anyCtx, actor, "a2").Return(nil) },
			func(ctx context.Context, client *api.Client) error { return client.ResetTwoFactor(ctx, "a2") },
		},
		"ListAuditLog applies the default page size": {
			func(recorders svcs) {
				recorders.audit.List(anyCtx, actor, validate.ListAuditLog{Limit: validate.DefaultLimit}).Return(services.AuditPage{}, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				_, err := client.ListAuditLog(ctx, validate.ListAuditLog{})
				return err
			},
		},
		"Overview": {
			func(recorders svcs) { recorders.analytics.Overview(anyCtx, actor).Return(services.Overview{}, nil) },
			func(ctx context.Context, client *api.Client) error {
				_, err := client.Overview(ctx)
				return err
			},
		},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			recorders, client := newClient(test)
			testCase.expect(recorders)
			if err := testCase.call(signedIn(test, apitest.AsAdmin(grace)), client); err != nil {
				test.Fatal(err)
			}
		})
	}
}

// TestClientRefusesLikeTheRoute: the in-process client never reaches a
// service when the API would refuse: signed out, or invalid input.
func TestClientRefusesLikeTheRoute(test *testing.T) {
	test.Parallel()
	_, client := newClient(test) // no service calls expected

	if _, err := client.ListAdmins(signedIn(test, apitest.Anonymous)); !errors.Is(err, access.ErrSignedOut) {
		test.Errorf("signed out: err = %v, want access.ErrSignedOut", err)
	}
	err := client.SetRole(signedIn(test, apitest.AsAdmin(grace)), "u1", validate.SetUserRole{Role: "Not A Role!"})
	if fields, ok := errors.AsType[*validate.Error](err); !ok || fields.Fields["role"] == "" {
		test.Errorf("invalid input: err = %v, want a *validate.Error on role", err)
	}
}

// TestEndpointPolicyHoldsForBothWays: one custom policy on one endpoint
// denies the same admin over HTTP (403 envelope) and in-process
// (access.ErrDenied), and lets the allowed admin through both ways.
func TestEndpointPolicyHoldsForBothWays(test *testing.T) {
	test.Parallel()
	other := grace
	other.ID, other.Email = "a2", "ada@example.com"
	onlyGrace := func(request *http.Request, caller access.Caller) error {
		if err := access.SignedIn(request, caller); err != nil {
			return err
		}
		if caller.Admin.Email != grace.Email {
			return fmt.Errorf("%w: not grace", access.ErrDenied)
		}
		return nil
	}
	ran := 0
	endpoint := api.Endpoint[api.NoInput, api.Success]{
		Method: "GET", Path: "/probe/{id}", Policy: onlyGrace,
		Handle: func(request *http.Request, _ access.Caller, _ api.NoInput) (api.Success, error) {
			ran++
			return api.Success{Success: request.PathValue("id") == "42"}, nil
		},
	}
	gate := apitest.Gate()
	mux := http.NewServeMux()
	mux.Handle(endpoint.Pattern(), endpoint.Serve(gate))
	probe := api.PathValue{Name: "id", Value: "42"}

	if res := apitest.Do(test, mux, apitest.AsAdmin(other), "GET", "/probe/42", ""); res.Status != http.StatusForbidden {
		test.Errorf("HTTP as other: status %d, want 403", res.Status)
	}
	if _, err := endpoint.Call(signedIn(test, apitest.AsAdmin(other)), gate, api.NoInput{}, probe); !errors.Is(err, access.ErrDenied) {
		test.Errorf("in-process as other: err = %v, want access.ErrDenied", err)
	}
	if ran != 0 {
		test.Fatalf("handler ran %d times for a denied admin", ran)
	}
	if res := apitest.Do(test, mux, apitest.AsAdmin(grace), "GET", "/probe/42", ""); res.Status != http.StatusOK {
		test.Errorf("HTTP as grace: status %d", res.Status)
	}
	if got, err := endpoint.Call(signedIn(test, apitest.AsAdmin(grace)), gate, api.NoInput{}, probe); err != nil || !got.Success {
		test.Errorf("in-process as grace: %+v, %v", got, err)
	}
}
