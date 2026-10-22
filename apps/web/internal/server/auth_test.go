package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
	"github.com/jayleaton/go-fast-stack/packages/database"
	"github.com/jayleaton/go-fast-stack/packages/database/dbtest"
)

const origin = "http://app.test"

type doFunc func(method, path, body string, headers ...string) *httptest.ResponseRecorder

// newWired returns the real server over packages/auth, the user repository
// and the credentials adapter on a fresh database.
func newWired(t *testing.T) doFunc {
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
		Users:  api.NewUsers(services.NewUserService(database.NewUserRepository(pool), NewAccountCredentials(a)), CallerFrom),
	})
	return func(method, path, body string, headers ...string) *httptest.ResponseRecorder {
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
}

// TestAuthWiring drives packages/auth through the real server: auth
// endpoints, the session middleware, the caller adapter and the users API
// over the real repository.
func TestAuthWiring(t *testing.T) {
	t.Parallel()
	do := newWired(t)

	rec := do("POST", "/api/auth/sign-up/email", `{"name":"Ada","email":"ada@example.com","password":"password123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-up: %d %s", rec.Code, rec.Body)
	}
	var signUp struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &signUp)
	cookie := rec.Result().Cookies()[0]
	// The mobile app sends the cookie with Expo's expo-origin header; a
	// browser (Origin) never reaches the API.
	withCookie := []string{"Cookie", cookie.Name + "=" + cookie.Value, "Expo-Origin", origin}
	if rec := do("GET", config.APIRoutes.Me, "", "Cookie", cookie.Name+"="+cookie.Value, "Origin", origin); rec.Code != http.StatusNotFound {
		t.Fatalf("GET users from a browser: %d %s", rec.Code, rec.Body)
	}

	rec = do("GET", config.APIRoutes.Me, "", withCookie...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"ada@example.com"`) {
		t.Fatalf("GET users with cookie: %d %s", rec.Code, rec.Body)
	}
	rec = do("PATCH", config.APIRoutes.Me, `{"name":"Ada Lovelace"}`, withCookie...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"Ada Lovelace"`) {
		t.Fatalf("PATCH users: %d %s", rec.Code, rec.Body)
	}
	bearer := []string{"Authorization", "Bearer " + signUp.Token}
	rec = do("GET", config.APIRoutes.Me, "", bearer...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"Ada Lovelace"`) {
		t.Fatalf("GET users with bearer: %d %s", rec.Code, rec.Body)
	}

	rec = do("GET", config.APIRoutes.MeData, "", bearer...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sessions":1`) {
		t.Fatalf("GET data summary: %d %s", rec.Code, rec.Body)
	}

	if rec = do("POST", "/api/auth/sign-out", "", withCookie...); rec.Code != http.StatusOK {
		t.Fatalf("sign-out: %d %s", rec.Code, rec.Body)
	}
	for name, headers := range map[string][]string{"cookie": withCookie, "bearer": bearer} {
		rec = do("GET", config.APIRoutes.Me, "", headers...)
		if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != `{"data":null,"error":"Unauthorized"}` {
			t.Errorf("%s after sign-out: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// TestAccountWiring drives every /users/me route through the credentials
// adapter over real packages/auth, so each auth error is seen translated.
func TestAccountWiring(t *testing.T) {
	t.Parallel()
	do := newWired(t)
	token := func(path, body string) []string {
		t.Helper()
		rec := do("POST", path, body)
		var res struct{ Token string }
		if err := json.Unmarshal(rec.Body.Bytes(), &res); rec.Code != http.StatusOK || err != nil || res.Token == "" {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return []string{"Authorization", "Bearer " + res.Token}
	}
	expect := func(as []string, method, path, body string, status int, snippet string) string {
		t.Helper()
		rec := do(method, path, body, as...)
		if rec.Code != status || !strings.Contains(rec.Body.String(), snippet) {
			t.Fatalf("%s %s %s: %d %s, want %d containing %s", method, path, body, rec.Code, rec.Body, status, snippet)
		}
		return rec.Body.String()
	}
	signIn := func(password string) []string {
		return token("/api/auth/sign-in/email", `{"email":"ada@example.com","password":"`+password+`"}`)
	}
	r := config.APIRoutes

	ada := token("/api/auth/sign-up/email", `{"name":"Ada","email":"ada@example.com","password":"password123"}`)
	token("/api/auth/sign-up/email", `{"name":"Eve","email":"eve@example.com","password":"password123"}`)
	phone := signIn("password123")

	var sessions struct{ Data []services.Session }
	_ = json.Unmarshal([]byte(expect(ada, "GET", r.MeSessions, "", 200, `"current":true`)), &sessions)
	if len(sessions.Data) != 2 || strings.Contains(fmt.Sprint(sessions.Data), "Bearer") {
		t.Fatalf("sessions = %+v", sessions.Data)
	}
	var current, other string
	for _, s := range sessions.Data {
		if s.Current {
			current = s.ID
		} else {
			other = s.ID
		}
	}

	expect(ada, "POST", r.MePassword, `{"currentPassword":"wrong-password","newPassword":"password456"}`, 400, "Current password is incorrect")
	expect(ada, "POST", r.MePassword, `{"currentPassword":"password123","newPassword":"password456"}`, 200, `"success":true`)
	signIn("password456")

	expect(ada, "POST", r.MeEmail, `{"newEmail":"ADA@example.com"}`, 400, "New email must be different")
	expect(ada, "POST", r.MeEmail, `{"newEmail":"eve@example.com"}`, 409, "Email is already in use")
	expect(ada, "POST", r.MeEmail, `{"newEmail":"ada@new.example"}`, 200, `"success":true`)
	expect(ada, "POST", r.MeEmail, `{"newEmail":"ada@new.example","callbackURL":"https://evil.example/"}`, 400, "Invalid callback URL")
	expect(ada, "POST", r.MeEmail, `{"newEmail":"ada@new.example"}`, 429, "Too many requests")

	expect(ada, "DELETE", config.MeSessionPath(current), "", 400, "Use sign out")
	expect(ada, "DELETE", config.MeSessionPath(other), "", 200, `"success":true`)
	expect(phone, "GET", r.Me, "", 401, "Unauthorized")
	expect(ada, "DELETE", config.MeSessionPath(other), "", 404, "Session not found")

	signIn("password456")
	expect(ada, "DELETE", r.MeOtherSessions, "", 200, `"success":true`)
	expect(ada, "GET", r.MeData, "", 200, `"sessions":1`)

	expect(ada, "DELETE", r.Me, "", 200, `"success":true`)
	expect(ada, "GET", r.Me, "", 401, "Unauthorized")
}
