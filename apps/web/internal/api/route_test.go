package api_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api/apitest"
)

// allowedEmail is who onlyAda lets in.
const allowedEmail = "ada@example.com"

// onlyAda is a policy a team would write (a role, a tenant): it is plugged
// into the route's registration line, with no change to middleware or to
// the route handler.
func onlyAda(_ *http.Request, caller access.Caller) error {
	if !caller.SignedIn() {
		return access.ErrSignedOut
	}
	if caller.User.Email != allowedEmail {
		return fmt.Errorf("%w: %s is not %s", access.ErrDenied, caller.User.Email, allowedEmail)
	}
	return nil
}

// echo answers with who the handler was given, so a test can see the
// caller the gate admitted.
func echo(_ *http.Request, caller access.Caller) (api.Success, error) {
	return api.Success{Success: caller.SignedIn()}, nil
}

func TestRoute(test *testing.T) {
	test.Parallel()
	grace := ada
	grace.ID, grace.Email = "u2", "grace@example.com"
	failing := access.NewGate(func(*http.Request) (access.Caller, error) {
		return access.Caller{}, errors.New("tenant lookup: connection refused")
	})

	cases := map[string]struct {
		gate     *access.Gate
		policy   access.Policy
		as       apitest.Session
		status   int
		signedIn bool // what the handler saw, on a 200
		message  string
	}{
		"public, signed out":         {apitest.Gate(), access.Public, apitest.Anonymous, http.StatusOK, false, ""},
		"public, signed in":          {apitest.Gate(), access.Public, apitest.AsUser(ada), http.StatusOK, true, ""},
		"signed in, signed out":      {apitest.Gate(), access.SignedIn, apitest.Anonymous, http.StatusUnauthorized, false, "Unauthorized"},
		"signed in, signed in":       {apitest.Gate(), access.SignedIn, apitest.AsUser(ada), http.StatusOK, true, ""},
		"custom policy, signed out":  {apitest.Gate(), onlyAda, apitest.Anonymous, http.StatusUnauthorized, false, "Unauthorized"},
		"custom policy, allowed":     {apitest.Gate(), onlyAda, apitest.AsUser(ada), http.StatusOK, true, ""},
		"custom policy, denied":      {apitest.Gate(), onlyAda, apitest.AsUser(grace), http.StatusForbidden, false, "Forbidden"},
		"resolver fails, never 401s": {failing, access.Public, apitest.AsUser(ada), http.StatusInternalServerError, false, "Internal server error"},
	}
	for name, testCase := range cases {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			ran := false
			mux := http.NewServeMux()
			mux.Handle("GET /probe", api.Route(testCase.gate, testCase.policy, func(request *http.Request, caller access.Caller) (api.Success, error) {
				ran = true
				return echo(request, caller)
			}))
			res := apitest.Do(test, mux, testCase.as, "GET", "/probe", "")
			if res.Status != testCase.status {
				test.Fatalf("status = %d, want %d (error %v)", res.Status, testCase.status, res.Error)
			}
			if testCase.status != http.StatusOK {
				if ran {
					test.Error("handler ran on a refused request")
				}
				if res.Error == nil || *res.Error != testCase.message {
					test.Errorf("error = %v, want %q", res.Error, testCase.message)
				}
				return
			}
			if got := apitest.Data[api.Success](test, res); got.Success != testCase.signedIn {
				test.Errorf("handler saw signed in = %v, want %v", got.Success, testCase.signedIn)
			}
		})
	}
}
