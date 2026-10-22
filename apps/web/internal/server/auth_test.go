package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
)

const origin = "http://app.test"

// TestAuthWiring drives packages/auth through the real server: auth
// endpoints, the session middleware, the caller adapter and the users API.
func TestAuthWiring(t *testing.T) {
	t.Parallel()
	pool := dbtest.New(t, dbtest.App)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(auth.Config{Secret: strings.Repeat("s", 32), BaseURL: origin, Logger: log}, pool, auth.NewDevMailer(log))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{
		Config: config.Config{TrustedOrigins: []string{origin}},
		Log:    log,
		DB:     pinger{},
		Auth:   a,
		Users:  api.NewUsers(services.NewUserService(database.NewUserRepository(pool)), CallerFrom),
	})
	do := func(method, path, body string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, origin+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		return serve(h, req)
	}

	rec := do("POST", "/api/auth/sign-up/email", `{"name":"Ada","email":"ada@example.com","password":"password123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-up: %d %s", rec.Code, rec.Body)
	}
	var signUp struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &signUp)
	cookie := rec.Result().Cookies()[0]
	withCookie := []string{"Cookie", cookie.Name + "=" + cookie.Value, "Origin", origin}

	rec = do("GET", config.APIRoutes.Users, "", withCookie...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"ada@example.com"`) {
		t.Fatalf("GET users with cookie: %d %s", rec.Code, rec.Body)
	}
	rec = do("PATCH", config.APIRoutes.Users, `{"name":"Ada Lovelace"}`, withCookie...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"Ada Lovelace"`) {
		t.Fatalf("PATCH users: %d %s", rec.Code, rec.Body)
	}
	bearer := []string{"Authorization", "Bearer " + signUp.Token}
	rec = do("GET", config.APIRoutes.Users, "", bearer...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"Ada Lovelace"`) {
		t.Fatalf("GET users with bearer: %d %s", rec.Code, rec.Body)
	}

	if rec = do("POST", "/api/auth/sign-out", "", withCookie...); rec.Code != http.StatusOK {
		t.Fatalf("sign-out: %d %s", rec.Code, rec.Body)
	}
	for name, headers := range map[string][]string{"cookie": withCookie, "bearer": bearer} {
		rec = do("GET", config.APIRoutes.Users, "", headers...)
		if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != `{"data":null,"error":"Unauthorized"}` {
			t.Errorf("%s after sign-out: %d %s", name, rec.Code, rec.Body)
		}
	}
}
