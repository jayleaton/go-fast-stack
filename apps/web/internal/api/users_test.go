package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services/servicetest"
)

type response struct {
	Data    map[string]any    `json:"data"`
	Error   *string           `json:"error"`
	Details map[string]string `json:"details"`
}

// newServer returns a mux where requests with "X-Test-User: <id>" are signed in.
func newServer(users ...services.User) *http.ServeMux {
	repo := servicetest.NewUsers(users...)
	caller := func(ctx context.Context) (api.Caller, bool) {
		id, _ := ctx.Value(userKey{}).(string)
		u, err := repo.GetUserByID(ctx, id)
		return api.Caller{User: u, Session: api.Session{ID: "s1", UserID: u.ID}}, err == nil
	}
	mux := http.NewServeMux()
	api.NewUsers(services.NewUserService(repo), caller).Routes(mux)
	return mux
}

type userKey struct{}

func do(t *testing.T, mux http.Handler, method, path, user, body string) (int, response) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		req = req.WithContext(context.WithValue(req.Context(), userKey{}, user))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: content-type %q", method, path, ct)
	}
	var res response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("%s %s: body %q: %v", method, path, rec.Body, err)
	}
	if (res.Data == nil) == (res.Error == nil) {
		t.Fatalf("%s %s: envelope must have exactly one of data/error: %s", method, path, rec.Body)
	}
	return rec.Code, res
}

func TestUsersAPI(t *testing.T) {
	mux := newServer(
		services.User{ID: "u1", Name: "Ada", Email: "ada@example.com"},
		services.User{ID: "u2", Name: "Eve", Email: "eve@example.com"},
	)
	users, byID := config.APIRoutes.Users, config.UserPath

	cases := []struct {
		name, method, path, user, body string
		status                         int
		check                          func(t *testing.T, r response)
	}{
		{"get requires session", "GET", users, "", "", 401, nil},
		{"get current user", "GET", users, "u1", "", 200, func(t *testing.T, r response) {
			if r.Data["user"].(map[string]any)["email"] != "ada@example.com" || r.Data["session"].(map[string]any)["id"] != "s1" {
				t.Errorf("data = %v", r.Data)
			}
		}},
		{"patch self", "PATCH", users, "u1", `{"name":"Ada L"}`, 200, func(t *testing.T, r response) {
			if r.Data["success"] != true || r.Data["user"].(map[string]any)["name"] != "Ada L" {
				t.Errorf("data = %v", r.Data)
			}
		}},
		{"patch self by id", "PATCH", byID("u1"), "u1", `{"image":"https://x/a.png"}`, 200, nil},
		{"patch other user is forbidden", "PATCH", byID("u2"), "u1", `{"name":"x"}`, 403, nil},
		{"invalid body", "PATCH", users, "u1", `{"name":""}`, 400, func(t *testing.T, r response) {
			if *r.Error != "Invalid request data" || r.Details["name"] == "" {
				t.Errorf("error = %v details = %v", *r.Error, r.Details)
			}
		}},
		{"email is not a settings field", "PATCH", users, "u1", `{"email":"x@y.z"}`, 400, nil},
		{"patch requires session", "PATCH", users, "", `{"name":"x"}`, 401, nil},
		{"delete self", "DELETE", users, "u2", "", 200, nil},
		{"deleted user has no session", "GET", users, "u2", "", 401, nil},
	}
	for _, c := range cases {
		status, res := do(t, mux, c.method, c.path, c.user, c.body)
		if status != c.status {
			t.Errorf("%s: status %d, want %d (%+v)", c.name, status, c.status, res)
			continue
		}
		if c.check != nil {
			c.check(t, res)
		}
	}
}
