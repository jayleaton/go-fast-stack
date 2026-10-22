package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // TOTP (RFC 6238) is HMAC-SHA-1
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
	"github.com/jayleaton/ai-patterns-go/packages/database/dbtest"
)

const (
	adminOrigin = "http://admin.test"
	appOrigin   = "http://app.test"
)

// adminDB is the admin database: the admin auth tables, then the audit log.
var adminDB = dbtest.Template{
	Name:        "admin",
	Fingerprint: dbtest.Fingerprint(auth.Migrations, adminrepo.Migrations),
	Setup: func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := auth.Migrate(ctx, pool); err != nil {
			return err
		}
		return adminrepo.Migrate(ctx, pool)
	},
}

// outbox is an auth.Mailer that keeps the links instead of sending them.
type outbox struct {
	mu    sync.Mutex
	links map[string]string // to -> last link
}

func (o *outbox) keep(to, link string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.links[to] = link
	return nil
}

func (o *outbox) link(t *testing.T, to string) string {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	l, ok := o.links[to]
	if !ok {
		t.Fatalf("no email to %s", to)
	}
	return l
}

func (o *outbox) SendVerificationEmail(_ context.Context, to, link string) error {
	return o.keep(to, link)
}
func (o *outbox) SendPasswordResetEmail(_ context.Context, to, link string) error {
	return o.keep(to, link)
}
func (o *outbox) SendSetPasswordEmail(_ context.Context, to, link string) error {
	return o.keep(to, link)
}
func (o *outbox) SendEmailChangeNotice(context.Context, string, string) error { return nil }

type wired struct {
	t         *testing.T
	admin     http.Handler // the admin app
	app       *auth.Auth   // the web app's auth instance on the app database
	admins    *auth.Auth
	mail      *outbox
	adminSvc  *services.AdminService
	adminPool *pgxpool.Pool
	auditLog  *adminrepo.AuditLog
	log       *slog.Logger
}

// newAdminInstance is one admin auth instance (as each replica of the admin
// app runs it) on the shared admin database, audited like cmd/admin does.
func newAdminInstance(t *testing.T, adminPool *pgxpool.Pool, auditLog *adminrepo.AuditLog, mail auth.Mailer, log *slog.Logger) *auth.Auth {
	t.Helper()
	admins, err := auth.New(auth.Config{
		Secret: strings.Repeat("b", 32), BaseURL: adminOrigin, CookiePrefix: "admin", DisableSignUp: true,
		TwoFactor: &auth.TwoFactorConfig{}, Logger: log,
		Events: NewAuditEvents(services.NewAuthEventService(auditLog), log),
	}, adminPool, mail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admins.Shutdown(context.Background()) })
	return admins
}

// newWired builds the real admin server (cmd/admin's wiring) over two fresh
// databases, plus the web app's auth instance on the app database.
func newWired(t *testing.T) *wired {
	t.Helper()
	appPool, adminPool := dbtest.New(t, dbtest.App), dbtest.New(t, adminDB)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mail := &outbox{links: map[string]string{}}

	app, err := auth.New(auth.Config{Secret: strings.Repeat("a", 32), BaseURL: appOrigin, Logger: log}, appPool, mail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	auditLog := adminrepo.NewAuditLog(adminPool)
	admins := newAdminInstance(t, adminPool, auditLog, mail, log)
	var p services.Policy
	adminStore := adminrepo.NewAdmins(adminPool)
	adminSvc := services.NewAdminService(NewAdminAccounts(admins, adminStore), auditLog, p)
	callers := NewCallers(admins)
	h := New(Deps{
		Config: config.Config{TrustedOrigins: []string{adminOrigin}},
		Log:    log,
		DBs:    map[string]Pinger{"app": appPool, "admin": adminPool},
		Auth:   admins,
		API: []Router{
			api.NewUsers(services.NewUserService(NewUserDirectory(auth.NewUserAdmin(appPool)), auditLog, p), callers.API),
			api.NewAdmins(adminSvc, callers.API),
			api.NewAudit(services.NewAuditService(auditLog, p), callers.API),
			api.NewAnalytics(services.NewAnalyticsService(adminrepo.NewStats(appPool), p), callers.API),
		},
	})
	return &wired{t: t, admin: h, app: app, admins: admins, mail: mail, adminSvc: adminSvc, adminPool: adminPool, auditLog: auditLog, log: log}
}

// appSignedIn asks the app's auth endpoints (as the web app serves them)
// whether token is a live session.
func (w *wired) appSignedIn(token string) bool {
	w.t.Helper()
	req := httptest.NewRequest(http.MethodGet, appOrigin+"/api/auth/get-session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := serve(w.app.Middleware(w.app.Handler()), req)
	return rec.Code == http.StatusOK && strings.TrimSpace(rec.Body.String()) != "null"
}

// raw sends one JSON request to the admin app with optional headers.
func (w *wired) raw(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	w.t.Helper()
	req := httptest.NewRequest(method, adminOrigin+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return serve(w.admin, req)
}

// auditActions lists the audit log's actions (newest first) about target,
// with their details.
func (w *wired) auditActions(bearer, target string) []string {
	w.t.Helper()
	// Auth events are audited on auth's event worker: wait for it.
	if err := w.admins.Shutdown(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	var page struct{ Data services.AuditPage }
	w.do("GET", config.APIRoutes.Audit+"?limit=100&targetId="+url.QueryEscape(target), "", bearer, 200, &page)
	var out []string
	for _, e := range page.Data.Entries {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		line := e.Action
		for _, k := range keys {
			line += " " + k + "=" + e.Details[k]
		}
		out = append(out, line)
	}
	return out
}

// wrongTOTP is a code that is not valid for secret anywhere near now (a fixed
// "000000" is valid once in ~300k windows).
func wrongTOTP(t *testing.T, secret string) string {
	t.Helper()
	valid := map[string]bool{}
	for d := int64(-2); d <= 2; d++ {
		valid[totp(t, secret, d)] = true
	}
	for i := 0; ; i++ {
		if c := fmt.Sprintf("%06d", i); !valid[c] {
			return c
		}
	}
}

// totp is RFC 6238 (SHA-1, 30 s, 6 digits) for step offset steps from now.
func totp(t *testing.T, secret string, offset int64) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		t.Fatal(err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(time.Now().Unix()/30+offset)) //nolint:gosec // positive
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	o := sum[len(sum)-1] & 0xf
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[o:])&0x7fffffff)%1_000_000)
}

// do sends one request to the admin app and decodes the body into out (if set).
func (w *wired) do(method, path, body, bearer string, wantStatus int, out any) string {
	w.t.Helper()
	req := httptest.NewRequest(method, adminOrigin+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := serve(w.admin, req)
	if rec.Code != wantStatus {
		w.t.Fatalf("%s %s: %d %s, want %d", method, path, rec.Code, rec.Body, wantStatus)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			w.t.Fatalf("%s %s: %v: %s", method, path, err, rec.Body)
		}
	}
	return rec.Body.String()
}

// inviteAdmin runs the CLI path (bootstrap + invite as the operator), then
// sets the password from the emailed link (it opens the UI's /set-password
// page) through the audited auth endpoint the opt-in API mounts. It returns
// the admin's bearer token.
func (w *wired) inviteAdmin(email, password string) string {
	w.t.Helper()
	ctx := context.Background()
	in := validate.CreateAdminInvite{Email: email}
	if err := in.Validate(); err != nil {
		w.t.Fatal(err)
	}
	if _, created, err := w.adminSvc.Bootstrap(ctx, services.SystemActor("bootstrap"), in); err != nil || !created {
		w.t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	if _, err := w.adminSvc.InviteAdmin(ctx, services.SystemActor("cli"), in); err != nil {
		w.t.Fatal(err)
	}
	if err := w.admins.Shutdown(ctx); err != nil { // flush the queued invite email
		w.t.Fatal(err)
	}
	link, err := url.Parse(w.mail.link(w.t, email))
	if err != nil {
		w.t.Fatal(err)
	}
	if link.Path != config.AppRoutes.SetPassword || link.Query().Get("token") == "" {
		w.t.Fatalf("invite link %s does not open the set-password page", link)
	}
	var res struct{ Token string }
	w.do("POST", "/api/auth/set-password", `{"token":"`+link.Query().Get("token")+`","newPassword":"`+password+`"}`, "", 200, &res)
	if res.Token == "" {
		w.t.Fatal("set-password returned no session token")
	}
	return res.Token
}

// TestBanFlow is the dashboard's core promise, over real packages/auth on two
// databases: a banned user's sessions die and their sign-in is refused in the
// app; unbanning restores it; every step is audited.
func TestBanFlow(t *testing.T) {
	t.Parallel()
	w := newWired(t)
	ctx := context.Background()

	ada, adaSession, err := w.app.SignUpEmail(ctx, auth.SignUpInput{Name: "Ada", Email: "ada@example.com", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	root := w.inviteAdmin("root@example.com", "admin-password-1")

	var list struct{ Data services.UserPage }
	w.do("GET", config.APIRoutes.Users+"?search=ada", "", root, 200, &list)
	if list.Data.Total != 1 || list.Data.Users[0].ID != ada.ID {
		t.Fatalf("list = %+v", list.Data)
	}
	var detail struct{ Data services.UserDetail }
	w.do("GET", config.UserPath(ada.ID, ""), "", root, 200, &detail)
	if len(detail.Data.Sessions) != 1 || detail.Data.User.Banned {
		t.Fatalf("detail = %+v", detail.Data)
	}

	if !w.appSignedIn(adaSession.Token) {
		t.Fatal("app session not valid before the ban")
	}
	w.do("POST", config.UserPath(ada.ID, "/ban"), `{"banReason":"spam","banExpiresIn":3600}`, root, 200, nil)
	if w.appSignedIn(adaSession.Token) {
		t.Fatal("banned user's session still valid in the app")
	}
	if _, _, err := w.app.SignInEmail(ctx, auth.SignInInput{Email: "ada@example.com", Password: "password123"}); !errors.Is(err, auth.ErrBannedUser) {
		t.Fatalf("banned sign-in: err = %v, want ErrBannedUser", err)
	}
	w.do("GET", config.UserPath(ada.ID, ""), "", root, 200, &detail)
	if !detail.Data.User.Banned || detail.Data.User.BanReason == nil || *detail.Data.User.BanReason != "spam" || detail.Data.User.BanExpires == nil {
		t.Fatalf("after ban = %+v", detail.Data.User)
	}

	w.do("POST", config.UserPath(ada.ID, "/unban"), "", root, 200, nil)
	if _, _, err := w.app.SignInEmail(ctx, auth.SignInInput{Email: "ada@example.com", Password: "password123"}); err != nil {
		t.Fatalf("sign-in after unban: %v", err)
	}
	w.do("PUT", config.UserPath(ada.ID, "/role"), `{"role":"admin"}`, root, 200, nil)
	w.do("DELETE", config.UserPath(ada.ID, "/sessions"), "", root, 200, nil)
	w.do("DELETE", config.UserSessionPath(ada.ID, "missing"), "", root, 404, nil)
	w.do("POST", config.UserPath("missing", "/ban"), `{"banReason":"x"}`, root, 404, nil)

	var audit struct{ Data services.AuditPage }
	w.do("GET", config.APIRoutes.Audit+"?targetId="+ada.ID, "", root, 200, &audit)
	var actions []string
	for _, e := range audit.Data.Entries {
		actions = append(actions, e.Action)
		if e.ActorEmail != "root@example.com" || e.TargetLabel != "ada@example.com" {
			t.Errorf("entry = %+v", e)
		}
	}
	if got := strings.Join(actions, ","); got != "user.revoke_sessions,user.set_role,user.unban,user.ban" {
		t.Fatalf("audit actions = %s", got)
	}

	var overview struct{ Data services.Overview }
	w.do("GET", config.APIRoutes.Analytics, "", root, 200, &overview)
	if overview.Data.TotalUsers != 1 {
		t.Fatalf("overview = %+v", overview.Data)
	}
}

// TestAdminIsolation: admins and app users never share a table, a cookie or
// a token, and nobody can sign up as an admin.
func TestAdminIsolation(t *testing.T) {
	t.Parallel()
	w := newWired(t)
	ctx := context.Background()

	_, adaSession, err := w.app.SignUpEmail(ctx, auth.SignUpInput{Name: "Ada", Email: "ada@example.com", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	root := w.inviteAdmin("root@example.com", "admin-password-1")

	w.do("GET", config.APIRoutes.Users, "", adaSession.Token, 401, nil) // an app session is not an admin session
	if w.appSignedIn(root) {
		t.Fatal("admin token accepted by the app")
	}
	w.do("POST", "/api/auth/sign-up/email", `{"name":"Eve","email":"eve@example.com","password":"password123"}`, "", 404, nil)

	var admins struct{ Data []services.Admin }
	w.do("GET", config.APIRoutes.Admins, "", root, 200, &admins)
	if len(admins.Data) != 1 || admins.Data[0].Email != "root@example.com" || !admins.Data[0].HasPassword {
		t.Fatalf("admins = %+v", admins.Data)
	}
	var list struct{ Data services.UserPage }
	w.do("GET", config.APIRoutes.Users, "", root, 200, &list)
	if list.Data.Total != 1 || list.Data.Users[0].Email != "ada@example.com" {
		t.Fatalf("app users = %+v (admins must not appear)", list.Data)
	}

	// Inviting through the API: a pending invite, then a conflict once active.
	var invited struct{ Data services.Admin }
	w.do("POST", config.APIRoutes.AdminInvites, `{"email":"Eve@Example.com"}`, root, 200, &invited)
	if invited.Data.Email != "eve@example.com" || invited.Data.HasPassword {
		t.Fatalf("invited = %+v", invited.Data)
	}
	w.do("POST", config.APIRoutes.AdminInvites, `{"email":"root@example.com"}`, root, 409, nil)
}

// TestAdminManagement: disabling an admin kills their sessions and refuses
// their sign-in until they are enabled; removing deletes them; nobody acts on
// themselves; every step is audited. The bootstrap admin's first invite is
// not reported as a resend.
func TestAdminManagement(t *testing.T) {
	t.Parallel()
	w := newWired(t)
	root := w.inviteAdmin("root@example.com", "admin-password-1")

	var admins struct{ Data []services.Admin }
	w.do("GET", config.APIRoutes.Admins, "", root, 200, &admins)
	rootID := admins.Data[0].ID
	if got := w.auditActions(root, rootID); !slices.Contains(got, "admin.invite") || slices.Contains(got, "admin.invite resent=true") {
		t.Fatalf("bootstrap admin's first invite: %v", got)
	}

	var eve struct{ Data services.Admin }
	w.do("POST", config.APIRoutes.AdminInvites, `{"email":"eve@example.com"}`, root, 200, &eve)
	_ = w.admins.Shutdown(context.Background()) // flush the invite email
	link, _ := url.Parse(w.mail.link(t, "eve@example.com"))
	var set struct{ Token string }
	w.do("POST", "/api/auth/set-password", `{"token":"`+link.Query().Get("token")+`","newPassword":"eve-password-1"}`, "", 200, &set)
	eveToken := set.Token

	w.do("POST", config.AdminPath(rootID, "/disable"), "", root, 400, nil) // yourself
	w.do("DELETE", config.AdminPath(rootID, ""), "", root, 400, nil)
	w.do("POST", config.AdminPath("missing", "/disable"), "", root, 404, nil)

	w.do("POST", config.AdminPath(eve.Data.ID, "/disable"), "", root, 200, nil)
	w.do("GET", config.APIRoutes.Users, "", eveToken, 401, nil) // sessions revoked
	if rec := w.raw("POST", "/api/auth/sign-in/email", `{"email":"eve@example.com","password":"eve-password-1"}`); rec.Code == 200 {
		t.Fatalf("disabled admin signed in: %s", rec.Body)
	}
	w.do("POST", config.AdminPath(eve.Data.ID, "/disable"), "", root, 400, nil) // already disabled
	w.do("POST", config.AdminPath(eve.Data.ID, "/enable"), "", root, 200, nil)
	if rec := w.raw("POST", "/api/auth/sign-in/email", `{"email":"eve@example.com","password":"eve-password-1"}`); rec.Code != 200 {
		t.Fatalf("enabled admin sign-in: %d %s", rec.Code, rec.Body)
	}
	w.do("DELETE", config.AdminPath(eve.Data.ID, ""), "", root, 200, nil)
	w.do("GET", config.APIRoutes.Admins, "", root, 200, &admins)
	if len(admins.Data) != 1 {
		t.Fatalf("admins after remove = %+v", admins.Data)
	}

	want := []string{"admin.remove", "admin.sign_in method=password", "admin.enable", "admin.disable", "admin.sign_in method=set_password", "admin.invite"}
	if got := w.auditActions(root, eve.Data.ID); !slices.Equal(got, want) {
		t.Fatalf("eve's audit trail = %v\nwant %v", got, want)
	}
}

// TestAuthEvents: JSON sign-in, failed and successful 2FA and sign-out are
// audited for the admin they belong to.
func TestAuthEvents(t *testing.T) {
	t.Parallel()
	w := newWired(t)
	root := w.inviteAdmin("root@example.com", "admin-password-1")
	var admins struct{ Data []services.Admin }
	w.do("GET", config.APIRoutes.Admins, "", root, 200, &admins)
	rootID := admins.Data[0].ID

	ctx := context.Background()
	setup, err := w.admins.EnableTwoFactor(ctx, rootID, "admin-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.admins.ConfirmTwoFactor(ctx, rootID, totp(t, setup.Secret, 0)); err != nil {
		t.Fatal(err)
	}

	rec := w.raw("POST", "/api/auth/sign-in/email", `{"email":"root@example.com","password":"admin-password-1"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "twoFactorRedirect") {
		t.Fatalf("sign-in with 2FA: %d %s", rec.Code, rec.Body)
	}
	var challenge string
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, "two_factor") {
			challenge = c.Name + "=" + c.Value
		}
	}
	if rec := w.raw("POST", "/api/auth/two-factor/verify-totp", `{"code":"`+wrongTOTP(t, setup.Secret)+`"}`, "Cookie", challenge, "Expo-Origin", adminOrigin); rec.Code == 200 {
		t.Fatalf("wrong code accepted: %s", rec.Body)
	}
	rec = w.raw("POST", "/api/auth/two-factor/verify-totp", `{"code":"`+totp(t, setup.Secret, 1)+`"}`, "Cookie", challenge, "Expo-Origin", adminOrigin)
	var signedIn struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &signedIn); rec.Code != 200 || err != nil || signedIn.Token == "" {
		t.Fatalf("verify-totp: %d %s", rec.Code, rec.Body)
	}
	if rec := w.raw("POST", "/api/auth/sign-out", "", "Authorization", "Bearer "+signedIn.Token, "Expo-Origin", adminOrigin); rec.Code != 200 {
		t.Fatalf("sign-out: %d %s", rec.Code, rec.Body)
	}

	got := w.auditActions(root, rootID)
	want := []string{"admin.sign_out", "admin.sign_in method=two_factor", "admin.two_factor_failed reason=INVALID_CODE", "admin.sign_in method=set_password"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("root's audit trail = %v\nwant it to start with %v", got, want)
	}
}

// TestAuthEventsExactAcrossInstances: two admin instances (two replicas of
// the admin app) on one admin database each audit through their own events.
// Concurrent wrong 2FA codes from two admins on different instances are each
// attributed to their own admin, which counter-based attribution could not do.
func TestAuthEventsExactAcrossInstances(t *testing.T) {
	t.Parallel()
	w := newWired(t)
	ctx := context.Background()
	root := w.inviteAdmin("root@example.com", "admin-password-1")
	var invited struct{ Data services.Admin }
	w.do("POST", config.APIRoutes.AdminInvites, `{"email":"eve@example.com"}`, root, 200, &invited)
	_ = w.admins.Shutdown(ctx) // flush the invite email
	link, _ := url.Parse(w.mail.link(t, "eve@example.com"))
	w.do("POST", "/api/auth/set-password", `{"token":"`+link.Query().Get("token")+`","newPassword":"eve-password-1"}`, "", 200, nil)
	var admins struct{ Data []services.Admin }
	w.do("GET", config.APIRoutes.Admins, "", root, 200, &admins)
	ids := map[string]string{}
	for _, a := range admins.Data {
		ids[a.Email] = a.ID
	}

	// A second replica: its own auth instance and handler, same database.
	replica := newAdminInstance(t, w.adminPool, w.auditLog, w.mail, w.log)
	// Each admin signs in on one instance and fails 2FA on the other: the
	// challenge lives in the shared database, the audit follows the admin.
	signInOn := map[string]*auth.Auth{"root@example.com": w.admins, "eve@example.com": replica}
	failOn := map[string]*auth.Auth{"root@example.com": replica, "eve@example.com": w.admins}
	const failures = 3 // under the per-IP 2FA rate limit of each instance
	var wg sync.WaitGroup
	for email, password := range map[string]string{"root@example.com": "admin-password-1", "eve@example.com": "eve-password-1"} {
		setup, err := w.admins.EnableTwoFactor(ctx, ids[email], password)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.admins.ConfirmTwoFactor(ctx, ids[email], totp(t, setup.Secret, 0)); err != nil {
			t.Fatal(err)
		}
		post := func(a *auth.Auth, path, body string, headers ...string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", adminOrigin+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			for i := 0; i+1 < len(headers); i += 2 {
				req.Header.Set(headers[i], headers[i+1])
			}
			return serve(a.Handler(), req)
		}
		rec := post(signInOn[email], "/api/auth/sign-in/email", `{"email":"`+email+`","password":"`+password+`"}`)
		var challenge string
		for _, c := range rec.Result().Cookies() {
			if strings.HasSuffix(c.Name, "two_factor") {
				challenge = c.Name + "=" + c.Value
			}
		}
		if challenge == "" {
			t.Fatalf("%s: no 2FA challenge: %d %s", email, rec.Code, rec.Body)
		}
		wrong := wrongTOTP(t, setup.Secret)
		for range failures {
			wg.Go(func() {
				if rec := post(failOn[email], "/api/auth/two-factor/verify-totp", `{"code":"`+wrong+`"}`, "Cookie", challenge, "Origin", adminOrigin); rec.Code != http.StatusUnauthorized {
					t.Errorf("%s: wrong code answered %d %s", email, rec.Code, rec.Body)
				}
			})
		}
	}
	wg.Wait()
	if err := replica.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	for email, id := range ids {
		n := 0
		for _, a := range w.auditActions(root, id) {
			if a == "admin.two_factor_failed reason=INVALID_CODE" {
				n++
			}
		}
		if n != failures {
			t.Errorf("%s has %d failed-2FA audit entries, want exactly %d", email, n, failures)
		}
	}
}
