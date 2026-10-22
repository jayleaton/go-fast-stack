package ui

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	templruntime "github.com/a-h/templ/runtime"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/view"
)

// rawKey catches a message key that rendered as itself (missing translation).
var rawKey = regexp.MustCompile(`\b(common|components|pages|errors)\.[a-z][A-Za-z]*\.?[A-Za-z.]*`)

// placeholder catches a Tf placeholder that was not filled.
var placeholder = regexp.MustCompile(`\{[0-9]\}`)

func renderIn(t *testing.T, l i18n.Locale, st view.State, c templ.Component) string {
	t.Helper()
	ctx := i18n.WithTranslator(context.Background(), i18n.Translator{Locale: l})
	ctx = view.WithState(ctx, st)
	var b strings.Builder
	if err := c.Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

type renderCase struct {
	name    string
	authed  bool
	c       templ.Component
	heading string // message key expected in the output
}

// renderCases are every page in each of its states.
func renderCases() []renderCase {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	usr := services.User{ID: "u1", Name: "Ada", Email: "ada@example.com", Role: "user", CreatedAt: at, UpdatedAt: at}
	banned := usr
	banned.Banned, banned.BanReason = true, new(string)
	*banned.BanReason = "spam"
	timed := banned
	timed.BanExpires = &at
	detail := services.UserDetail{User: banned, Sessions: []services.Session{{ID: "s1", CreatedAt: at, ExpiresAt: at}}}
	setup := &TOTPSetup{Secret: "ABCDEFGH", URI: "otpauth://totp/x", BackupCodes: []string{"c1"}}
	pager := view.Pager{From: 1, To: 1, Total: 3, NextURL: "/users?offset=1"}

	return []renderCase{
		{"login", false, LoginPage(AuthForm{}), "pages.login.title"},
		{"login error", false, LoginPage(AuthForm{Error: "x"}), "pages.login.loginButton"},
		{"set password", false, SetPasswordPage("tok", AuthForm{}), "pages.setPassword.newPassword"},
		{"set password no token", false, SetPasswordPage("", AuthForm{}), "pages.setPassword.invalidLink"},
		{"two factor", false, TwoFactorPage(TwoFactorForm{}), "pages.twoFactor.codeLabel"},
		{"two factor backup", false, TwoFactorPage(TwoFactorForm{Backup: true, Trust: true, Error: "x"}), "pages.twoFactor.backupCodeLabel"},
		{"two factor expired", false, TwoFactorPage(TwoFactorForm{Expired: true, Error: "x"}), "common.backToLogin"},
		{"setup challenge", false, TwoFactorSetupPage(SetupState{Required: true, Setup: setup}), "pages.twoFactorSetup.backupCodesHint"},
		{"setup expired", false, TwoFactorSetupPage(SetupState{Expired: true}), "errors.challengeExpired"},
		{"setup password", true, TwoFactorSetupPage(SetupState{SignedIn: true}), "pages.twoFactorSetup.start"},
		{"setup password required", true, TwoFactorSetupPage(SetupState{SignedIn: true, Required: true}), "pages.twoFactorSetup.required"},
		{"setup signed in", true, TwoFactorSetupPage(SetupState{SignedIn: true, Setup: setup}), "pages.twoFactorSetup.confirm"},
		{"setup done", true, TwoFactorSetupPage(SetupState{SignedIn: true, Done: true}), "pages.twoFactorSetup.done"},
		{"setup already on", true, TwoFactorSetupPage(SetupState{SignedIn: true, Enabled: true}), "pages.twoFactorSetup.alreadyOn"},
		{"users", true, UsersPage(UsersData{Users: []services.User{usr, banned, timed}, Pager: pager, Sort: UserFilters{SortBy: "email"}.sortLinks()}), "pages.users.title"},
		{"users empty", true, UsersPage(UsersData{}), "pages.users.empty"},
		{"users error", true, UsersPage(UsersData{Error: "x"}), "pages.users.filters.apply"},
		{"user", true, UserPage(UserData{Detail: detail}), "pages.user.ban.unban"},
		{"user not banned", true, UserPage(UserData{Detail: services.UserDetail{User: usr}, Status: Status{Message: "ok"}}), "pages.user.ban.notBanned"},
		{"user missing", true, UserPage(UserData{Missing: true, Status: Status{Error: true, Message: "x"}}), "pages.user.back"},
		{"admins", true, AdminsPage(AdminsData{Admins: []services.Admin{{Email: "a@b.c", HasPassword: true, TwoFactorEnabled: true}, {Email: "p@b.c"}}}), "pages.admins.pending"},
		{"admins sent", true, AdminsPage(AdminsData{Invite: InviteState{Sent: "sent"}}), "pages.admins.invite.title"},
		{"admins error", true, AdminsPage(AdminsData{Error: "x"}), "pages.admins.invite.submit"},
		{"audit", true, AuditPage(AuditData{Entries: []services.AuditEntry{{Action: services.ActionAdminInvite, TargetType: services.TargetAdmin, TargetLabel: "a@b.c", CreatedAt: at}}, Pager: pager}), "pages.audit.actions.adminInvite"},
		{"audit empty", true, AuditPage(AuditData{Filters: AuditFilters{TargetID: "u1"}}), "pages.audit.empty"},
		{"audit error", true, AuditPage(AuditData{Error: "x"}), "pages.audit.title"},
		{"analytics", true, AnalyticsPage(AnalyticsData{Overview: services.Overview{TotalUsers: 3, GeneratedAt: at}}), "pages.analytics.placeholder"},
		{"analytics error", true, AnalyticsPage(AnalyticsData{Error: "x"}), "pages.analytics.title"},
	}
}

// TestPagesRenderInEveryLocale renders each page (and its states) in every
// locale and checks the heading text, that no key or placeholder leaked
// through, the html lang, and the signed-in header.
func TestPagesRenderInEveryLocale(t *testing.T) {
	pages := renderCases()
	for _, l := range i18n.Locales {
		tr := i18n.Translator{Locale: l}
		for _, p := range pages {
			t.Run(string(l)+"/"+p.name, func(t *testing.T) {
				html := renderIn(t, l, view.State{Authenticated: p.authed, Email: "root@example.com", Section: view.SectionUsers}, p.c)
				if m := rawKey.FindString(html); m != "" {
					t.Errorf("untranslated key %q in output", m)
				}
				if m := placeholder.FindString(html); m != "" {
					t.Errorf("unfilled placeholder %q in output", m)
				}
				if !strings.Contains(html, templ.EscapeString(tr.T(p.heading))) {
					t.Errorf("missing %s text %q", p.heading, tr.T(p.heading))
				}
				if !strings.Contains(html, `<html lang="`+string(l)+`"`) {
					t.Errorf("html lang is not %s", l)
				}
				if !strings.Contains(html, "<title>") || !strings.Contains(html, view.DefaultAppName+"</title>") {
					t.Error("title lacks the app name")
				}
				if p.authed != strings.Contains(html, templ.EscapeString(tr.T("components.authStatus.logout"))) {
					t.Errorf("logout button shown=%v, want %v", !p.authed, p.authed)
				}
				if p.authed != strings.Contains(html, templ.EscapeString(tr.T("components.nav.audit"))) {
					t.Errorf("nav shown=%v, want %v", !p.authed, p.authed)
				}
			})
		}
	}
}

// TestFormsHaveLoadingIndicators keeps a Datastar indicator on every action
// form (GET filter forms flip a signal on submit instead).
func TestFormsHaveLoadingIndicators(t *testing.T) {
	at := time.Now()
	detail := services.UserDetail{User: services.User{ID: "u1", Banned: true}, Sessions: []services.Session{{ID: "s1", ExpiresAt: at}}}
	for _, c := range []templ.Component{
		LoginPage(AuthForm{}), SetPasswordPage("t", AuthForm{}), TwoFactorPage(TwoFactorForm{}),
		TwoFactorSetupPage(SetupState{SignedIn: true}), TwoFactorSetupPage(SetupState{Setup: &TOTPSetup{}}),
		UserPage(UserData{Detail: detail}), AdminsPage(AdminsData{}),
	} {
		html := renderIn(t, i18n.En, view.State{Authenticated: true}, c)
		forms := strings.Count(html, `method="post"`) - 2 // the logout and locale forms in the header
		if n := strings.Count(html, "data-indicator:"); n < forms {
			t.Errorf("%d post forms, %d indicators:\n%s", forms, n, html)
		}
	}
	for _, c := range []templ.Component{UsersPage(UsersData{}), AuditPage(AuditData{})} {
		if html := renderIn(t, i18n.En, view.State{}, c); !strings.Contains(html, `data-on:submit="$_`) {
			t.Error("filter form has no loading state")
		}
	}
}

var errHangUp = errors.New("client hung up")

// hangUpAfter accepts n writes, then fails like a closed connection.
type hangUpAfter struct{ n int }

func (w *hangUpAfter) Write(p []byte) (int, error) {
	return w.WriteString(string(p))
}

// WriteString lets bufio pass each templ write through whole.
func (w *hangUpAfter) WriteString(s string) (int, error) {
	if w.n <= 0 {
		return 0, errHangUp
	}
	w.n--
	return len(s), nil
}

// TestRenderStopsWhenClientHangsUp fails the connection at every write of
// every page: Render must stop and return the write error (ui.render logs
// it), never panic or swallow it. A 1-byte templ buffer makes each write
// reach the connection.
func TestRenderStopsWhenClientHangsUp(t *testing.T) {
	size := templruntime.DefaultBufferSize
	templruntime.DefaultBufferSize = 1
	defer func() { templruntime.DefaultBufferSize = size }()
	ctx := view.WithState(i18n.WithTranslator(context.Background(), i18n.Translator{Locale: i18n.En}), view.State{Authenticated: true})
	for _, p := range renderCases() {
		for n := 0; ; n++ {
			b := &templruntime.Buffer{}
			b.Reset(&hangUpAfter{n: n})
			err := p.c.Render(ctx, b)
			if err == nil {
				break
			}
			if !errors.Is(err, errHangUp) {
				t.Fatalf("%s: write %d: %v", p.name, n, err)
			}
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, p := range renderCases() {
		if err := p.c.Render(canceled, io.Discard); !errors.Is(err, context.Canceled) {
			t.Errorf("%s rendered for a canceled request: %v", p.name, err)
		}
	}
}

func TestSection(t *testing.T) {
	for path, want := range map[string]string{
		"/users": view.SectionUsers, "/users/u1": view.SectionUsers, "/admins": view.SectionAdmins,
		"/audit": view.SectionAudit, "/analytics": view.SectionAnalytics, "/login": "", "/usersx": "",
	} {
		if got := section(path); got != want {
			t.Errorf("section(%q) = %q, want %q", path, got, want)
		}
	}
}
