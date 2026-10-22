package ui

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/view"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/welcome"
)

// rawKey catches a message key that rendered as itself (missing translation).
var rawKey = regexp.MustCompile(`\b(common|components|pages|errors)\.[a-z][A-Za-z]*\.?[A-Za-z.]*`)

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

// TestPagesRenderInEveryLocale renders each parity page (and its states) in
// every locale and checks the heading text and that no key leaked through.
func TestPagesRenderInEveryLocale(t *testing.T) {
	user := User{ID: "u1", Name: "Ada", Email: "ada@example.com", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	pages := []struct {
		name    string
		authed  bool
		c       templ.Component
		heading string // message key expected in the output
	}{
		{"home (welcome off)", false, HomePage(), "pages.home.title"},
		{"login", false, LoginPage(AuthForm{}), "pages.login.title"},
		{"login error", false, LoginPage(AuthForm{Error: "x"}), "pages.login.loginButton"},
		{"signup", false, SignupPage(AuthForm{}), "pages.signup.title"},
		{"forgot password", false, ForgotPasswordPage(AuthForm{}, false), "pages.forgotPassword.sendResetLink"},
		{"forgot password sent", false, ForgotPasswordPage(AuthForm{}, true), "pages.forgotPassword.emailSent"},
		{"reset password", false, ResetPasswordPage("tok", AuthForm{}, false), "pages.resetPassword.newPassword"},
		{"reset password no token", false, ResetPasswordPage("", AuthForm{}, false), "pages.resetPassword.invalidResetLink"},
		{"reset password done", false, ResetPasswordPage("tok", AuthForm{}, true), "pages.resetPassword.passwordResetSuccess"},
		{"verify email", false, VerifyEmailPage("a@b.c", ResendState{}), "pages.verifyEmail.checkEmailTitle"},
		{"verify email resent", false, VerifyEmailPage("a@b.c", ResendState{Message: "sent", CooldownSeconds: 300}), "pages.verifyEmail.resendVerificationEmail"},
		{"verify success loading", false, VerifyEmailSuccessPage("tok", nil), "pages.verifyEmail.verifyingEmail"},
		{"verify success done", true, VerifyEmailSuccessPage("", VerifyEmailSucceeded()), "pages.verifyEmail.emailVerifiedSuccessfully"},
		{"verify success failed", false, VerifyEmailSuccessPage("", VerifyEmailFailed("x")), "pages.verifyEmail.verificationFailed"},
		{"dashboard", true, DashboardPage(DashboardData{User: user, Tab: "t"}), "pages.dashboard.title"},
		{"dashboard saved", true, DashboardPage(DashboardData{User: user, Settings: SettingsState{Saved: true}}), "pages.dashboard.settingsForm.saved"},
		{"live body", true, LiveBody(time.Now(), Session{ExpiresAt: time.Now().Add(time.Hour)}), "pages.dashboard.live.serverTime"},
		{"welcome landing", false, welcome.LandingPage(), ""},
		{"welcome why", false, welcome.WhyPage(), ""},
	}
	for _, l := range i18n.Locales {
		tr := i18n.Translator{Locale: l}
		for _, p := range pages {
			t.Run(string(l)+"/"+p.name, func(t *testing.T) {
				html := renderIn(t, l, view.State{Authenticated: p.authed}, p.c)
				if m := rawKey.FindString(html); m != "" {
					t.Errorf("untranslated key %q in output", m)
				}
				if p.heading != "" && !strings.Contains(html, templ.EscapeString(tr.T(p.heading))) {
					t.Errorf("missing %s text %q", p.heading, tr.T(p.heading))
				}
				full := strings.HasPrefix(strings.ToLower(html), "<!doctype html>")
				if full && !strings.Contains(html, `<html lang="`+string(l)+`"`) {
					t.Errorf("html lang is not %s", l)
				}
				if full && p.authed != strings.Contains(html, templ.EscapeString(tr.T("components.authStatus.logout"))) {
					t.Errorf("logout button shown=%v, want %v", !p.authed, p.authed)
				}
			})
		}
	}
}

// TestWelcomeCopyMatches keeps the welcome copy complete in every locale.
func TestWelcomeCopyMatches(t *testing.T) {
	en := renderIn(t, i18n.En, view.State{}, welcome.LandingPage())
	ja := renderIn(t, i18n.Ja, view.State{}, welcome.LandingPage())
	if !strings.Contains(en, "A good first house for your project") || !strings.Contains(ja, "プロジェクトの、最初の家") {
		t.Fatal("landing hero not localized")
	}
	if strings.Count(en, "<li") != strings.Count(ja, "<li") {
		t.Fatalf("landing list items differ: en %d, ja %d", strings.Count(en, "<li"), strings.Count(ja, "<li"))
	}
}
