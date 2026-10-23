package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

// signedIn is the context of a UI request signed in as user (what the auth
// middleware leaves on it), or signed out with apitest.Anonymous.
func signedIn(test *testing.T, as apitest.Session) context.Context {
	return as(httptest.NewRequestWithContext(test.Context(), "GET", "/", nil)).Context()
}

// TestClientMethods: every in-process client method reaches its endpoint's
// service call with the caller's actor and input, as the HTTP route does.
func TestClientMethods(test *testing.T) {
	test.Parallel()
	name := "Ada L"
	cases := map[string]struct {
		expect func(service *mocks.MockUserServiceMockRecorder)
		call   func(ctx context.Context, client *api.Client) error
	}{
		"Me": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.GetProfile(anyCtx, actor, "u1").Return(ada, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				current, err := client.Me(ctx)
				if err == nil && (current.User.ID != "u1" || current.Session.ID != actor.SessionID) {
					return errors.New("wrong user or session")
				}
				return err
			},
		},
		"UpdateMe validates in the API": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.UpdateProfile(anyCtx, actor, "u1", validate.UpdateUserSettings{Name: &name}).Return(ada, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				padded := "  Ada L  " // trimmed by Validate, as in the HTTP route
				_, err := client.UpdateMe(ctx, validate.UpdateUserSettings{Name: &padded})
				return err
			},
		},
		"RequestEmailChange": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.RequestEmailChange(anyCtx, actor, "u1", validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: "/dashboard"}).Return(nil)
			},
			func(ctx context.Context, client *api.Client) error {
				return client.RequestEmailChange(ctx, validate.UpdateUserEmail{NewEmail: " Ada@New.Example ", CallbackURL: "/dashboard"})
			},
		},
		"ChangePassword": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.ChangePassword(anyCtx, actor, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456"}).Return(nil)
			},
			func(ctx context.Context, client *api.Client) error {
				return client.ChangePassword(ctx, validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456"})
			},
		},
		"ListSessions": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.ListSessions(anyCtx, actor, "u1").Return([]services.Session{{ID: "s2"}}, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				_, err := client.ListSessions(ctx)
				return err
			},
		},
		"RevokeSession reads the path value": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.RevokeSession(anyCtx, actor, "u1", "s2").Return(nil)
			},
			func(ctx context.Context, client *api.Client) error { return client.RevokeSession(ctx, "s2") },
		},
		"RevokeOtherSessions": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.RevokeOtherSessions(anyCtx, actor, "u1").Return(nil)
			},
			func(ctx context.Context, client *api.Client) error { return client.RevokeOtherSessions(ctx) },
		},
		"DeleteMe": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.DeleteAccount(anyCtx, actor, "u1").Return(nil)
			},
			func(ctx context.Context, client *api.Client) error { return client.DeleteMe(ctx) },
		},
		"DataSummary": {
			func(service *mocks.MockUserServiceMockRecorder) {
				service.DataSummary(anyCtx, actor, "u1").Return(services.UserDataSummary{User: ada}, nil)
			},
			func(ctx context.Context, client *api.Client) error {
				_, err := client.DataSummary(ctx)
				return err
			},
		},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			service := mocks.NewMockUserService(gomock.NewController(test))
			testCase.expect(service.EXPECT())
			client := api.NewClient(api.NewUsers(service, apitest.Gate()))
			if err := testCase.call(signedIn(test, apitest.AsUser(ada)), client); err != nil {
				test.Fatal(err)
			}
		})
	}
}

// TestClientRefusesLikeTheRoute: the in-process client never reaches the
// service when the API would refuse: signed out, or invalid input.
func TestClientRefusesLikeTheRoute(test *testing.T) {
	test.Parallel()
	service := mocks.NewMockUserService(gomock.NewController(test)) // no calls expected
	client := api.NewClient(api.NewUsers(service, apitest.Gate()))

	if _, err := client.Me(signedIn(test, apitest.Anonymous)); !errors.Is(err, access.ErrSignedOut) {
		test.Errorf("signed out: err = %v, want access.ErrSignedOut", err)
	}
	empty := ""
	_, err := client.UpdateMe(signedIn(test, apitest.AsUser(ada)), validate.UpdateUserSettings{Name: &empty})
	if fields, ok := errors.AsType[*validate.Error](err); !ok || fields.Fields["name"] == "" {
		test.Errorf("invalid input: err = %v, want a *validate.Error on name", err)
	}
}

// TestEndpointPolicyHoldsForBothWays: one custom policy on one endpoint
// denies the same caller over HTTP (403 envelope) and in-process
// (access.ErrDenied), and lets the allowed caller through both ways.
func TestEndpointPolicyHoldsForBothWays(test *testing.T) {
	test.Parallel()
	grace := ada
	grace.ID, grace.Email = "u2", "grace@example.com"
	ran := 0
	endpoint := api.Endpoint[api.NoInput, api.Success]{
		Method: "GET", Path: "/probe/{id}", Policy: onlyAda,
		Handle: func(request *http.Request, _ access.Caller, _ api.NoInput) (api.Success, error) {
			ran++
			return api.Success{Success: request.PathValue("id") == "42"}, nil
		},
	}
	gate := apitest.Gate()
	mux := http.NewServeMux()
	mux.Handle(endpoint.Pattern(), endpoint.Serve(gate))

	if res := apitest.Do(test, mux, apitest.AsUser(grace), "GET", "/probe/42", ""); res.Status != http.StatusForbidden {
		test.Errorf("HTTP as grace: status %d, want 403", res.Status)
	}
	if _, err := endpoint.Call(signedIn(test, apitest.AsUser(grace)), gate, api.NoInput{}, api.PathValue{Name: "id", Value: "42"}); !errors.Is(err, access.ErrDenied) {
		test.Errorf("in-process as grace: err = %v, want access.ErrDenied", err)
	}
	if ran != 0 {
		test.Fatalf("handler ran %d times for a denied caller", ran)
	}

	if res := apitest.Do(test, mux, apitest.AsUser(ada), "GET", "/probe/42", ""); res.Status != http.StatusOK || !apitest.Data[api.Success](test, res).Success {
		test.Errorf("HTTP as ada: status %d, data %s", res.Status, res.Data)
	}
	got, err := endpoint.Call(signedIn(test, apitest.AsUser(ada)), gate, api.NoInput{}, api.PathValue{Name: "id", Value: "42"})
	if err != nil || !got.Success {
		test.Errorf("in-process as ada: %+v, %v", got, err)
	}
}
