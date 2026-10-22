package ui

import (
	"context"
	"errors"
	"image/color"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	templruntime "github.com/a-h/templ/runtime"
	"github.com/boombuler/barcode/qr"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/view"
	"github.com/jayleaton/go-fast-stack/packages/uikit"
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
	pager := Pager{From: 1, To: 1, Total: 3, NextURL: "/users?offset=1"}

	return []renderCase{
		{"login", false, LoginPage(AuthForm{}), "pages.login.title"},
		{"login error", false, LoginPage(AuthForm{Error: "x"}), "pages.login.loginButton"},
		{"set password", false, SetPasswordPage("tok", AuthForm{}), "pages.setPassword.newPassword"},
		{"set password no token", false, SetPasswordPage("", AuthForm{}), "pages.setPassword.invalidLink"},
		{"two factor", false, TwoFactorPage(TwoFactorForm{}), "pages.twoFactor.codeLabel"},
		{"two factor backup", false, TwoFactorPage(TwoFactorForm{Backup: true, Trust: true, Error: "x"}), "pages.twoFactor.backupCodeLabel"},
		{"two factor expired", false, TwoFactorPage(TwoFactorForm{Expired: true, Error: "x"}), "common.backToLogin"},
		{"setup challenge", false, TwoFactorSetupPage(SetupState{Required: true, Setup: setup}), "pages.twoFactorSetup.backupCodesHint"},
		{"setup no qr", false, TwoFactorSetupPage(SetupState{Required: true, Setup: &TOTPSetup{Secret: "ABCDEFGH", URI: "not-otpauth"}}), "pages.twoFactorSetup.manualEntry"},
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
		{"admins actions", true, AdminsPage(AdminsData{SelfID: "a1", Admins: []services.Admin{{ID: "a1", Email: "a@b.c", HasPassword: true}, {ID: "a2", Email: "d@b.c", HasPassword: true, Disabled: true}, {ID: "a3", Email: "e@b.c", HasPassword: true}}}), "pages.admins.disabled"},
		{"admins self", true, AdminsPage(AdminsData{SelfID: "a1", Admins: []services.Admin{{ID: "a1", Email: "a@b.c", HasPassword: true}}}), "pages.admins.you"},
		{"admins status", true, AdminsPage(AdminsData{Status: Status{Message: "ok"}}), "pages.admins.actions"},
		{"admins status error", true, AdminsPage(AdminsData{Status: Status{Error: true, Message: "x"}}), "pages.admins.title"},
		{"admins sent", true, AdminsPage(AdminsData{Invite: InviteState{Sent: "sent"}}), "pages.admins.invite.title"},
		{"admins error", true, AdminsPage(AdminsData{Error: "x"}), "pages.admins.invite.submit"},
		{"audit", true, AuditPage(AuditData{Entries: []services.AuditEntry{{Action: services.ActionAdminInvite, TargetType: services.TargetAdmin, TargetLabel: "a@b.c", CreatedAt: at}}, Pager: pager}), "pages.audit.actions.adminInvite"},
		{"audit admin actions", true, AuditPage(AuditData{Entries: []services.AuditEntry{
			{Action: services.ActionAdminSignIn, TargetType: services.TargetAdmin, Details: map[string]string{"method": services.SignInTwoFactor}, CreatedAt: at},
			{Action: services.ActionAdminTwoFactorFailed, TargetType: services.TargetAdmin, Details: map[string]string{"reason": "INVALID_BACKUP_CODE"}, CreatedAt: at},
			{Action: services.ActionAdminRemove, TargetType: services.TargetAdmin, CreatedAt: at},
		}}), "pages.audit.methods.twoFactor"},
		{"audit statuses", true, AuditPage(AuditData{Entries: []services.AuditEntry{
			{Action: services.ActionUserBan, TargetType: services.TargetUser, TargetID: "u1", CreatedAt: at, Status: services.StatusSucceeded},
			{Action: services.ActionUserBan, TargetType: services.TargetUser, TargetID: "u1", CreatedAt: at, Status: services.StatusFailed, Reason: services.ErrUserNotFound.Message},
			{Action: services.ActionAdminRemove, TargetType: services.TargetAdmin, CreatedAt: at, Status: services.StatusFailed, Reason: services.ErrAuditUnavailable.Message},
			{Action: services.ActionUserUnban, TargetType: services.TargetUser, TargetID: "u1", CreatedAt: at, Status: services.StatusPending},
			{Action: services.ActionAdminInvite, TargetType: services.TargetAdmin, CreatedAt: at, Status: services.StatusUnknown},
			{Action: services.ActionUserBan, TargetType: services.TargetUser, TargetID: "u1", CreatedAt: at, Status: services.StatusSucceeded, Reason: "Reconciled: x", Reconciled: true},
			{Action: services.ActionUserBan, TargetType: services.TargetUser, TargetID: "u1", CreatedAt: at, Status: services.StatusFailed, Reason: "Reconciled: x", Reconciled: true},
			{Action: services.ActionAdminInvite, TargetType: services.TargetAdmin, CreatedAt: at, Status: services.StatusUnknown, Reason: "Reconciled: x", Reconciled: true},
		}}), "pages.audit.statuses.pendingHint"},
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
	t.Parallel()
	pages := renderCases()
	for _, l := range i18n.Locales {
		tr := i18n.Translator{Locale: l}
		for _, p := range pages {
			t.Run(string(l)+"/"+p.name, func(t *testing.T) {
				t.Parallel()
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
	t.Parallel()
	at := time.Now()
	detail := services.UserDetail{User: services.User{ID: "u1", Banned: true}, Sessions: []services.Session{{ID: "s1", ExpiresAt: at}}}
	for _, c := range []templ.Component{
		LoginPage(AuthForm{}), SetPasswordPage("t", AuthForm{}), TwoFactorPage(TwoFactorForm{}),
		TwoFactorSetupPage(SetupState{SignedIn: true}), TwoFactorSetupPage(SetupState{Setup: &TOTPSetup{}}),
		UserPage(UserData{Detail: detail}), AdminsPage(AdminsData{}),
		AdminsPage(AdminsData{SelfID: "a1", Admins: []services.Admin{{ID: "a1"}, {ID: "a2", HasPassword: true}, {ID: "a3", Disabled: true}, {ID: "a4"}}}),
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

// oneByteBuffers are templ buffers that pass each write straight to the
// connection, one per render case. A Buffer takes its size from the global
// templruntime.DefaultBufferSize when it is first Reset and keeps it, so they
// are made here, during package initialisation, before any test runs:
// setting the global inside a test would race with the parallel tests'
// renders.
var oneByteBuffers = func() []*templruntime.Buffer {
	size := templruntime.DefaultBufferSize
	templruntime.DefaultBufferSize = 1
	defer func() { templruntime.DefaultBufferSize = size }()
	bufs := make([]*templruntime.Buffer, len(renderCases()))
	for i := range bufs {
		bufs[i] = &templruntime.Buffer{}
		bufs[i].Reset(io.Discard)
	}
	return bufs
}()

// TestRenderStopsWhenClientHangsUp fails the connection at every write of
// every page: Render must stop and return the write error (ui.render logs
// it), never panic or swallow it. A 1-byte templ buffer makes each write
// reach the connection.
func TestRenderStopsWhenClientHangsUp(t *testing.T) {
	t.Parallel()
	ctx := view.WithState(i18n.WithTranslator(context.Background(), i18n.Translator{Locale: i18n.En}), view.State{Authenticated: true})
	for i, p := range renderCases() {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			b := oneByteBuffers[i]
			if b.Size() != 1 {
				t.Fatalf("templ buffer of %d bytes, want 1", b.Size())
			}
			for n := 0; ; n++ {
				b.Reset(&hangUpAfter{n: n})
				err := p.c.Render(ctx, b)
				if err == nil {
					break
				}
				if !errors.Is(err, errHangUp) {
					t.Fatalf("write %d: %v", n, err)
				}
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err := p.c.Render(canceled, io.Discard); !errors.Is(err, context.Canceled) {
				t.Errorf("rendered for a canceled request: %v", err)
			}
		})
	}
}

// TestAuditStatus pins the badge of every status in every locale: a known
// tone and a translated label, the pending hint, and reconciled reasons
// chosen by status rather than shown as recorded.
func TestAuditStatus(t *testing.T) {
	t.Parallel()
	tones := map[string]uikit.Tone{
		services.StatusSucceeded: uikit.Success, services.StatusFailed: uikit.Error,
		services.StatusPending: uikit.Warning, services.StatusUnknown: uikit.Neutral,
	}
	for _, l := range i18n.Locales {
		ctx := i18n.WithTranslator(context.Background(), i18n.Translator{Locale: l})
		for status, tone := range tones {
			s := auditStatus(ctx, services.AuditEntry{Status: status, Reason: "Reconciled: raw", Reconciled: true})
			if s.Tone != tone || s.Label == "" || s.Label == status || rawKey.MatchString(s.Label) {
				t.Errorf("%s %s: %+v", l, status, s)
			}
			if (s.Hint != "") != (status == services.StatusPending) {
				t.Errorf("%s %s: hint %q", l, status, s.Hint)
			}
			if _, reconciles := reconciledReasonKeys[status]; reconciles == (s.Reason == "Reconciled: raw") || !s.Reconciled {
				t.Errorf("%s %s: reconciled reason %q", l, status, s.Reason)
			}
		}
		if s := auditStatus(ctx, services.AuditEntry{}); s.Label != "—" || s.Tone != uikit.Neutral {
			t.Errorf("%s: no status: %+v", l, s)
		}
		if got := auditReason(ctx, services.ErrAuditUnavailable.Message); got != i18n.T(ctx, "errors.auditUnavailable") {
			t.Errorf("%s: audit unavailable reason %q", l, got)
		}
		if got := auditReason(ctx, "disk full"); got != "disk full" {
			t.Errorf("%s: unknown reason %q", l, got)
		}
	}
}

func TestSection(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/users": view.SectionUsers, "/users/u1": view.SectionUsers, "/admins": view.SectionAdmins,
		"/audit": view.SectionAudit, "/analytics": view.SectionAnalytics, "/login": "", "/usersx": "",
	} {
		if got := section(path); got != want {
			t.Errorf("section(%q) = %q, want %q", path, got, want)
		}
	}
}

// qrRun is one rectangle of a QR path: "M{x} {y}h{w}v1h-{w}z".
var qrRun = regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`)

// TestTOTPQR redraws the SVG path module by module and compares it with the
// encoder's matrix, quiet zone included.
func TestTOTPQR(t *testing.T) {
	t.Parallel()
	uri := "otpauth://totp/Acme:root%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=Acme"
	q, ok := totpQR(uri)
	if !ok {
		t.Fatal("no QR code for a valid otpauth URI")
	}
	want, err := qr.Encode(uri, qr.M, qr.Auto)
	if err != nil {
		t.Fatal(err)
	}
	n := want.Bounds().Dx()
	if q.Size != n+2*qrQuietZone || q.ViewBox() != "0 0 "+strconv.Itoa(q.Size)+" "+strconv.Itoa(q.Size) {
		t.Fatalf("size %d, viewBox %q, want %d modules + quiet zone", q.Size, q.ViewBox(), n)
	}
	// Only runs, so nothing of the URI (the secret) is in the page twice.
	if strings.Join(qrRun.FindAllString(q.Path, -1), "") != q.Path {
		t.Fatalf("path has segments that are not runs: %.80q", q.Path)
	}
	got := map[[2]int]bool{}
	runs := qrRun.FindAllStringSubmatch(q.Path, -1)
	for _, m := range runs {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		if m[3] != m[4] || w < 1 {
			t.Fatalf("bad run %q", m[0])
		}
		for i := range w {
			got[[2]int{x + i, y}] = true
		}
	}
	dark := 0
	for y := range q.Size {
		for x := range q.Size {
			mx, my := x-qrQuietZone, y-qrQuietZone
			inside := mx >= 0 && my >= 0 && mx < n && my < n
			isDark := inside && color.GrayModel.Convert(want.At(mx, my)).(color.Gray).Y < 128
			if isDark {
				dark++
			}
			if got[[2]int{x, y}] != isDark {
				t.Fatalf("module (%d,%d): path %v, encoder %v", x, y, got[[2]int{x, y}], isDark)
			}
		}
	}
	if dark == 0 || len(got) != dark {
		t.Fatalf("%d dark modules drawn, %d expected", len(got), dark)
	}

	for _, bad := range []string{"", "javascript:alert(1)", "https://example.com", "otpauth://totp/x?secret=" + strings.Repeat("A", 5000)} {
		if q, ok := totpQR(bad); ok || q.Path != "" {
			t.Errorf("totpQR(%.40q) = ok", bad)
		}
	}
}

// Remove asks in a dialog that names the admin (escaped), while the form
// itself posts straight to the remove action, so it works without JS.
func TestRemoveAdminAsksInADialog(t *testing.T) {
	t.Parallel()
	email := `o"brien's </script>&co@example.com`
	html := renderIn(t, i18n.En, view.State{Authenticated: true}, AdminsPage(AdminsData{SelfID: "a1", Admins: []services.Admin{{ID: "a2", Email: email, HasPassword: true}}}))
	for _, want := range []string{
		`action="/admins/a2/remove" data-on:submit="@post(&#39;/admins/a2/remove&#39;, {contentType: &#39;form&#39;})" data-indicator:_removing`,
		`<dialog id="remove-a2-dialog"`, `aria-haspopup="dialog"`,
		templ.EscapeString(i18n.Tf(i18n.WithTranslator(context.Background(), i18n.Translator{Locale: i18n.En}), "pages.admins.confirmRemove", email)),
	} {
		if !strings.Contains(html, want) {
			t.Errorf("admins page lacks %s", want)
		}
	}
	if strings.Contains(html, email) || strings.Contains(html, "confirm(") {
		t.Error("the email is unescaped, or Remove still uses a browser confirm")
	}
}

// A failure recorded without a service error is translated, never shown raw.
func TestAuditReasonInternal(t *testing.T) {
	t.Parallel()
	for _, l := range i18n.Locales {
		ctx := i18n.WithTranslator(context.Background(), i18n.Translator{Locale: l})
		if got, want := auditReason(ctx, services.ReasonInternal), i18n.T(ctx, "pages.audit.internalReason"); got != want || got == services.ReasonInternal && l != i18n.En {
			t.Errorf("%s: auditReason = %q, want %q", l, got, want)
		}
	}
}

// The audit page says how its two kinds of entries are recorded: admin
// changes fail closed, authentication events are best-effort.
func TestAuditPageExplainsRecording(t *testing.T) {
	t.Parallel()
	for _, l := range i18n.Locales {
		html := renderIn(t, l, view.State{Authenticated: true}, AuditPage(AuditData{}))
		if !strings.Contains(html, templ.EscapeString(i18n.Translator{Locale: l}.T("pages.audit.howRecorded"))) {
			t.Errorf("%s: audit page lacks the recording note", l)
		}
	}
}
