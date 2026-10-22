package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

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
	t        *testing.T
	admin    http.Handler // the admin app
	app      *auth.Auth   // the web app's auth instance on the app database
	admins   *auth.Auth
	mail     *outbox
	adminSvc *services.AdminService
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
	admins, err := auth.New(auth.Config{
		Secret: strings.Repeat("b", 32), BaseURL: adminOrigin, CookiePrefix: "admin",
		TwoFactor: &auth.TwoFactorConfig{}, Logger: log,
	}, adminPool, mail)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admins.Shutdown(context.Background()); _ = app.Shutdown(context.Background()) })

	auditLog := adminrepo.NewAuditLog(adminPool)
	var p services.Policy
	adminSvc := services.NewAdminService(NewAdminAccounts(admins), auditLog, p)
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
	return &wired{t: t, admin: h, app: app, admins: admins, mail: mail, adminSvc: adminSvc}
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
// sets the password from the emailed link through the admin auth endpoints.
// It returns the admin's bearer token.
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
	token := link.Path[strings.LastIndex(link.Path, "/")+1:]
	var res struct{ Token string }
	w.do("POST", "/api/auth/set-password", `{"token":"`+token+`","newPassword":"`+password+`"}`, "", 200, &res)
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
