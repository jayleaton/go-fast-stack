package ui_test

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/routes"
)

func TestPages(t *testing.T) {
	e := newEnv(t, config.Features{TemplateWelcome: true})
	pages := []string{
		config.AppRoutes.Home, config.AppRoutes.Why, config.AppRoutes.Login, config.AppRoutes.Signup,
		config.AppRoutes.ForgotPassword, config.AppRoutes.ResetPassword + "?token=abc",
		config.AppRoutes.VerifyEmail + "?email=a%40b.c", config.AppRoutes.VerifyEmailSuccess,
	}
	for _, l := range []string{"en", "ja"} {
		for _, p := range pages {
			rec := e.do(http.MethodGet, p, nil, lang(l))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Language") != l {
				t.Errorf("%s %s: status %d, Content-Language %q", l, p, rec.Code, rec.Header().Get("Content-Language"))
			}
		}
	}
	assertSeeOther(t, e.do(http.MethodGet, config.AppRoutes.Dashboard, nil), config.AppRoutes.Login)
	assertSeeOther(t, e.do(http.MethodGet, config.AppRoutes.Account, nil), config.AppRoutes.Login)

	e.signedIn("sess-1")
	rec := e.do(http.MethodGet, config.AppRoutes.Dashboard, nil, lang("ja"))
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, ja("pages.dashboard.title")) || !strings.Contains(body, `href="`+config.AppRoutes.Account+`"`) {
		t.Errorf("dashboard signed in: %d", rec.Code)
	}
	assertSeeOther(t, e.do(http.MethodGet, config.AppRoutes.Home, nil), config.AppRoutes.Dashboard)

	off := newEnv(t, config.Features{})
	if rec := off.do(http.MethodGet, config.AppRoutes.Why, nil); rec.Code != http.StatusNotFound {
		t.Errorf("why with welcome off: %d", rec.Code)
	}
	if rec := off.do(http.MethodGet, config.AppRoutes.Home, nil); !strings.Contains(rec.Body.String(), en("pages.home.title")) {
		t.Error("home with welcome off is not the plain page")
	}
	if rec := e.do(http.MethodGet, "/favicon.ico", nil); rec.Code != http.StatusMovedPermanently {
		t.Errorf("favicon: %d", rec.Code)
	}
}

func TestLogin(t *testing.T) {
	form := url.Values{"email": {" demo@example.com "}, "password": {"password123"}}
	in := ui.SignInInput{Email: "demo@example.com", Password: "password123", CallbackURL: config.AppRoutes.VerifyEmailSuccess}

	t.Run("success", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.auth.EXPECT().SignInEmail(gomock.Any(), in).Return(demoUser, demoSession, nil)
		e.auth.EXPECT().SetSessionCookie(gomock.Any(), demoSession).Do(setCookie)
		rec := e.do(http.MethodPost, config.AppRoutes.Login, form, datastar)
		assertRedirectScript(t, rec, config.AppRoutes.Dashboard)
		if !hasCookie(rec, demoSession.Token) {
			t.Error("session cookie not set")
		}
	})

	t.Run("without JS", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.auth.EXPECT().SignInEmail(gomock.Any(), in).Return(demoUser, demoSession, nil)
		e.auth.EXPECT().SetSessionCookie(gomock.Any(), demoSession)
		assertSeeOther(t, e.do(http.MethodPost, config.AppRoutes.Login, form), config.AppRoutes.Dashboard)

		e.auth.EXPECT().SignInEmail(gomock.Any(), in).Return(ui.User{}, ui.Session{}, authErr(t, "errors.invalidCredentials"))
		rec := e.do(http.MethodPost, config.AppRoutes.Login, form)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), en("errors.invalidCredentials")) || !strings.Contains(rec.Body.String(), `value="demo@example.com"`) {
			t.Errorf("no-JS failed login: %d", rec.Code)
		}
	})

	// Every auth error the UI knows renders translated; unknown ones generically.
	for _, ae := range ui.AuthErrors {
		t.Run(ae.Key, func(t *testing.T) {
			e := newEnv(t, config.Features{})
			e.auth.EXPECT().SignInEmail(gomock.Any(), in).Return(ui.User{}, ui.Session{}, ae.Err)
			if ae.Key == "pages.verifyEmail.description" { // unverified: a fresh link goes out
				e.auth.EXPECT().SendVerificationEmail(gomock.Any(), "demo@example.com", config.AppRoutes.VerifyEmailSuccess).Return(nil)
			}
			assertSSE(t, e.do(http.MethodPost, config.AppRoutes.Login, form, datastar, lang("ja")), `id="form-error"`, ja(ae.Key))
		})
	}
	t.Run("unknown error", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.auth.EXPECT().SignInEmail(gomock.Any(), in).Return(ui.User{}, ui.Session{}, errors.New("db down"))
		assertSSE(t, e.do(http.MethodPost, config.AppRoutes.Login, form, datastar), en("errors.somethingWentWrong"))
	})
}

func TestSignup(t *testing.T) {
	form := url.Values{"name": {"Grace"}, "email": {"grace@example.com"}, "password": {"password123"}}
	in := ui.SignUpInput{Name: "Grace", Email: "grace@example.com", Password: "password123", CallbackURL: config.AppRoutes.VerifyEmailSuccess}

	e := newEnv(t, config.Features{})
	e.auth.EXPECT().SignUpEmail(gomock.Any(), in).Return(demoUser, demoSession, nil)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), demoSession)
	assertRedirectScript(t, e.do(http.MethodPost, config.AppRoutes.Signup, form, datastar), config.AppRoutes.Dashboard)

	e.auth.EXPECT().SignUpEmail(gomock.Any(), in).Return(ui.User{}, ui.Session{}, authErr(t, "errors.emailTaken"))
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.Signup, form, datastar), en("errors.emailTaken"))

	// With verification, auth returns no session and the user checks their inbox.
	v := newEnv(t, config.Features{EmailVerification: true})
	v.auth.EXPECT().SignUpEmail(gomock.Any(), in).Return(demoUser, ui.Session{}, nil)
	assertRedirectScript(t, v.do(http.MethodPost, config.AppRoutes.Signup, form, datastar), config.AppRoutes.VerifyEmail+"?email=grace%40example.com")
}

func TestForgotAndResetPassword(t *testing.T) {
	e := newEnv(t, config.Features{})
	e.auth.EXPECT().RequestPasswordReset(gomock.Any(), "demo@example.com", config.AppRoutes.ResetPassword).Return(nil)
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.ForgotPassword, url.Values{"email": {"demo@example.com"}}, datastar), `id="forgot-password"`, en("pages.forgotPassword.emailSent"))

	e.auth.EXPECT().RequestPasswordReset(gomock.Any(), "demo@example.com", config.AppRoutes.ResetPassword).Return(errors.New("smtp"))
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.ForgotPassword, url.Values{"email": {"demo@example.com"}}, datastar), en("common.error"))

	reset := func(pw, confirm string) url.Values {
		return url.Values{"token": {"tok"}, "password": {pw}, "confirmPassword": {confirm}}
	}
	// A mismatch never reaches auth.
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.ResetPassword, reset("newpassword1", "other12345"), datastar), en("pages.resetPassword.passwordMismatch"))

	e.auth.EXPECT().ResetPassword(gomock.Any(), "tok", "newpassword1").Return(nil)
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.ResetPassword, reset("newpassword1", "newpassword1"), datastar), `id="reset-password"`, en("pages.resetPassword.passwordResetSuccess"))

	e.auth.EXPECT().ResetPassword(gomock.Any(), "tok", "newpassword1").Return(authErr(t, "pages.verifyEmail.invalidToken"))
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.ResetPassword, reset("newpassword1", "newpassword1"), datastar), en("pages.resetPassword.invalidResetLink"))

	e.auth.EXPECT().ResetPassword(gomock.Any(), "tok", "short").Return(authErr(t, "errors.passwordTooShort"))
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.ResetPassword, reset("short", "short"), datastar), en("pages.resetPassword.passwordTooShort"))
}

func TestVerifyEmail(t *testing.T) {
	e := newEnv(t, config.Features{EmailVerification: true})
	e.auth.EXPECT().SendVerificationEmail(gomock.Any(), "new@example.com", config.AppRoutes.VerifyEmailSuccess).Return(nil)
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.VerifyEmail, url.Values{"email": {"new@example.com"}}, datastar, lang("ja")),
		`id="resend"`, "{_cooldown: 300}", ja("pages.verifyEmail.verificationEmailSent"))

	// Opening the link spends nothing: the page asks for a click, carrying
	// the token and callback only as hidden fields (no VerifyEmail call is
	// expected here).
	page := e.do(http.MethodGet, config.AppRoutes.VerifyEmailSuccess+"?token=abc&callbackURL=%2Fdashboard%2Faccount", nil)
	body := page.Body.String()
	if !strings.Contains(body, `<input type="hidden" name="token" value="abc">`) ||
		!strings.Contains(body, `<input type="hidden" name="callbackURL" value="/dashboard/account">`) ||
		!strings.Contains(body, en("pages.verifyEmail.confirmButton")) || strings.Count(body, "abc") != 1 || strings.Contains(body, `data-init="@post`) {
		t.Fatalf("confirm page:\n%s", body)
	}
	if body := e.do(http.MethodGet, config.AppRoutes.VerifyEmailSuccess, nil).Body.String(); !strings.Contains(body, en("pages.verifyEmail.invalidToken")) {
		t.Error("a link without a token is not shown as invalid")
	}

	// auth's own link handler redirects here with ?error=<code> on failure.
	for code, key := range map[string]string{"INVALID_TOKEN": "pages.verifyEmail.invalidToken", "TOKEN_EXPIRED": "pages.verifyEmail.invalidToken", "SOMETHING_NEW": "pages.verifyEmail.unexpectedErrorDuringVerification"} {
		body := e.do(http.MethodGet, config.AppRoutes.VerifyEmailSuccess+"?error="+code, nil).Body.String()
		if !strings.Contains(body, en("pages.verifyEmail.verificationFailed")) || !strings.Contains(body, en(key)) || strings.Contains(body, "data-init") {
			t.Errorf("?error=%s: not rendered as a failed verification", code)
		}
	}

	// The confirming POST spends the token and continues to the callback.
	e.auth.EXPECT().VerifyEmail(gomock.Any(), "abc").Return(demoUser, demoSession, nil)
	e.auth.EXPECT().SetSessionCookie(gomock.Any(), demoSession)
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.VerifyEmailSuccess, url.Values{"token": {"abc"}, "callbackURL": {"/dashboard/account"}}, datastar),
		`id="verify-result"`, en("pages.verifyEmail.emailVerifiedSuccessfully"), "window.location.assign(&#39;/dashboard/account&#39;)")

	// The mobile app's deep link is followed only when auth trusts it.
	for callback, want := range map[string]string{"betterstack://verified": "betterstack://verified", "evil://x": config.AppRoutes.Dashboard} {
		e.auth.EXPECT().VerifyEmail(gomock.Any(), "abc").Return(demoUser, ui.Session{}, nil)
		trusted := error(nil)
		if callback != want {
			trusted = errors.New("untrusted")
		}
		e.auth.EXPECT().CheckCallbackURL(callback).Return(trusted)
		assertSSE(t, e.do(http.MethodPost, config.AppRoutes.VerifyEmailSuccess, url.Values{"token": {"abc"}, "callbackURL": {callback}}, datastar),
			"window.location.assign(&#39;"+want+"&#39;)")
	}

	e.auth.EXPECT().VerifyEmail(gomock.Any(), "abc").Return(ui.User{}, ui.Session{}, authErr(t, "pages.verifyEmail.invalidToken"))
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.VerifyEmailSuccess, url.Values{"token": {"abc"}}, datastar), en("pages.verifyEmail.verificationFailed"), en("pages.verifyEmail.invalidToken"))
	assertSSE(t, e.do(http.MethodPost, config.AppRoutes.VerifyEmailSuccess, url.Values{}, datastar), en("pages.verifyEmail.invalidToken"))
}

func TestLogout(t *testing.T) {
	e := newEnv(t, config.Features{})
	e.signedIn("sess-1")
	e.auth.EXPECT().SignOut(gomock.Any(), demoSession.Token).Return(nil)
	e.auth.EXPECT().ClearSessionCookie(gomock.Any())
	assertSeeOther(t, e.do(http.MethodPost, routes.Logout, nil), config.AppRoutes.Login)
}

func TestLocaleSwitch(t *testing.T) {
	e := newEnv(t, config.Features{})
	rec := e.do(http.MethodPost, routes.Locale, url.Values{"locale": {"ja"}, "next": {"/login?x=1"}})
	assertSeeOther(t, rec, "/login?x=1")
	var lc *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "locale" {
			lc = c
		}
	}
	if lc == nil || lc.Value != "ja" {
		t.Fatalf("locale cookie: %+v", lc)
	}
	page := e.do(http.MethodGet, config.AppRoutes.Login, nil, func(r *http.Request) { r.AddCookie(lc) }, lang("en"))
	if !strings.Contains(page.Body.String(), ja("pages.login.title")) {
		t.Error("cookie locale not applied over Accept-Language")
	}
	for _, next := range []string{"//evil.example", "https://evil.example", `/\evil.example`} {
		assertSeeOther(t, e.do(http.MethodPost, routes.Locale, url.Values{"locale": {"en"}, "next": {next}}), config.AppRoutes.Home)
	}
}

func TestActionsRejectCrossOrigin(t *testing.T) {
	e := newEnv(t, config.Features{})
	rec := e.do(http.MethodPost, config.AppRoutes.Login, url.Values{"email": {"a@b.c"}}, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", rec.Code)
	}
}
