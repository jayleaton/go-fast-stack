package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The emailed links send browsers to app pages for relative targets, so the
// browser never touches the JSON API; absolute and deep-link targets (mobile)
// keep the GET endpoints under BasePath.

const verifyPage = "/auth/verify-email/success"

// lastMail returns the URL of the newest email of kind sent to to.
func (harness *harness) lastMail(kind, to string) string {
	harness.test.Helper()
	mails := harness.mails()
	for index := len(mails) - 1; index >= 0; index-- {
		if mails[index].kind == kind && mails[index].to == to {
			return mails[index].url
		}
	}
	harness.test.Fatalf("no %s mail to %s in %+v", kind, to, mails)
	return ""
}

// wantAPILink checks link is GET BasePath+path with the given callbackURL.
func wantAPILink(test *testing.T, link, pathPrefix, callbackURL string) {
	test.Helper()
	address, err := url.Parse(link)
	if err != nil {
		test.Fatal(err)
	}
	if address.Scheme+"://"+address.Host != testBase || !strings.HasPrefix(address.Path, BasePath+pathPrefix) || address.Query().Get("callbackURL") != callbackURL {
		test.Fatalf("link = %s, want %s%s...?callbackURL=%s", link, BasePath, pathPrefix, callbackURL)
	}
}

func TestResetLinkShapes(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "old-password"), http.StatusOK)

	// Relative: straight to the app page, existing query kept.
	if err := harness.auth.RequestPasswordReset(ctx, email, "/auth/reset-password?step=1"); err != nil {
		test.Fatal(err)
	}
	link := harness.lastMail("reset", email)
	token := mustQuery(test, link, "token")
	if token == "" || link != testBase+"/auth/reset-password?step=1&token="+token {
		test.Fatalf("relative reset link = %s", link)
	}
	if err := harness.auth.ResetPassword(ctx, token, "new-password"); err != nil {
		test.Fatalf("direct link token: %v", err)
	}
	wantStatus(test, harness.signIn(email, "new-password"), http.StatusOK)

	// Absolute and deep-link targets go through the API callback.
	for _, target := range []string{testBase + "/reset", "betterstack://reset-password"} {
		if err := harness.auth.RequestPasswordReset(ctx, email, target); err != nil {
			test.Fatal(err)
		}
		link := harness.lastMail("reset", email)
		wantAPILink(test, link, "/reset-password/", target)
		rec := harness.do("GET", pathOf(test, link), nil, nil)
		wantStatus(test, rec, http.StatusFound)
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, target+"?token=") {
			test.Fatalf("%s: callback redirect = %q", target, loc)
		}
	}
}

func TestSetPasswordLinkShapes(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)
	user, err := harness.auth.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email})
	if err != nil {
		test.Fatal(err)
	}

	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "betterstack://set-password"); err != nil {
		test.Fatal(err)
	}
	wantAPILink(test, harness.lastMail("set-password", email), "/reset-password/", "betterstack://set-password")
	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, testBase+"/set"); err != nil {
		test.Fatal(err)
	}
	wantAPILink(test, harness.lastMail("set-password", email), "/reset-password/", testBase+"/set")

	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "/set-password"); err != nil {
		test.Fatal(err)
	}
	link := harness.lastMail("set-password", email)
	token := mustQuery(test, link, "token")
	if token == "" || link != testBase+"/set-password?token="+token {
		test.Fatalf("relative set-password link = %s", link)
	}
	if _, sess, err := harness.auth.CompleteSetPassword(ctx, token, "admin-password"); err != nil || sess.Token == "" {
		test.Fatalf("direct link token: %+v %v", sess, err)
	}
}

func TestVerificationLinkShapes(test *testing.T) {
	test.Parallel()
	ctx := context.Background()
	signUp := func(harness *harness, callbackURL string) (string, string) {
		harness.test.Helper()
		email := uniqueEmail(harness.test)
		if _, _, err := harness.auth.SignUpEmail(ctx, SignUpInput{Name: "V", Email: email, Password: "password123", CallbackURL: callbackURL}); err != nil {
			harness.test.Fatal(err)
		}
		return email, harness.lastMail("verification", email)
	}

	// Without VerifyEmailPath every link is the API link.
	fixture := newHarness(test, func(config *Config) { config.RequireEmailVerification = true })
	_, link := signUp(fixture, "/done")
	wantAPILink(test, link, "/verify-email", "/done")

	fixture = newHarness(test, func(config *Config) {
		config.RequireEmailVerification = true
		config.VerifyEmailPath = verifyPage
	})
	for callback, want := range map[string]string{"/done?x=1": "/done?x=1", "": "/"} {
		email, link := signUp(fixture, callback)
		token := mustQuery(test, link, "token")
		if token == "" || link != testBase+verifyPage+"?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(want) {
			test.Fatalf("callback %q: link = %s", callback, link)
		}
		// The page spends the token in-process: verified and signed in.
		user, sess, err := fixture.auth.VerifyEmail(ctx, token)
		if err != nil || user.Email != email || !user.EmailVerified || sess.Token == "" {
			test.Fatalf("direct link token: %+v %+v %v", user, sess, err)
		}
	}

	// Absolute and deep-link callbacks open the page too, which hands them
	// back through CheckCallbackURL.
	for _, callback := range []string{testBase + "/done", "betterstack://verified"} {
		_, link := signUp(fixture, callback)
		token := mustQuery(test, link, "token")
		if link != testBase+verifyPage+"?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(callback) {
			test.Fatalf("%s: link = %s", callback, link)
		}
		if err := fixture.auth.CheckCallbackURL(callback); err != nil {
			test.Fatalf("%s not trusted: %v", callback, err)
		}

		// An old API link forwards to the page without spending the token.
		rec := fixture.do("GET", "/api/auth/verify-email?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(callback), nil, nil)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != link || sessionCookie(rec) != nil {
			test.Fatalf("%s: GET %d %q", callback, rec.Code, loc)
		}
		if _, sess, err := fixture.auth.VerifyEmail(ctx, token); err != nil || sess.Token == "" {
			test.Fatalf("%s: token spent by GET: %v", callback, err)
		}
	}
	for _, bad := range []string{"", "https://evil.example/x", "evil://x", "//evil.example"} {
		if err := fixture.auth.CheckCallbackURL(bad); !errors.Is(err, ErrInvalidCallbackURL) {
			test.Errorf("CheckCallbackURL(%q) = %v", bad, err)
		}
	}
}

func TestChangeEmailLinkShapes(test *testing.T) {
	test.Parallel()
	ctx := context.Background()
	harness := newHarness(test, func(config *Config) { config.VerifyEmailPath = verifyPage })
	email := uniqueEmail(test)
	wantStatus(test, harness.signUp(email, "password123"), http.StatusOK)
	cookie := harness.loginCookie(email)
	id := harness.userID(hdr{"Cookie": cookie})

	// Absolute and deep-link callbacks open the page too.
	for _, callback := range []string{testBase + "/account", "betterstack://account"} {
		next := uniqueEmail(test)
		if err := harness.auth.ChangeEmail(ctx, id, next, callback); err != nil {
			test.Fatal(err)
		}
		if link := harness.lastMail("verification", next); !strings.HasPrefix(link, testBase+verifyPage+"?token=") || mustQuery(test, link, "callbackURL") != callback {
			test.Fatalf("%s: change-email link = %s", callback, link)
		}
	}

	// Relative: the app page, which spends the token in-process.
	const callback = "/dashboard/account?email-changed=1"
	next := uniqueEmail(test)
	if err := harness.auth.ChangeEmail(ctx, id, next, callback); err != nil {
		test.Fatal(err)
	}
	link := harness.lastMail("verification", next)
	token := mustQuery(test, link, "token")
	if token == "" || link != testBase+verifyPage+"?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(callback) {
		test.Fatalf("change-email link = %s", link)
	}
	user, sess, err := harness.auth.VerifyEmail(ctx, token)
	if err != nil || user.Email != next || !user.EmailVerified || sess.Token == "" || sess.UserID != id {
		test.Fatalf("direct link token: %+v %+v %v", user, sess, err)
	}
	if got := harness.getSession(hdr{"Cookie": cookie})["user"].(map[string]any)["email"]; got != next {
		test.Fatalf("existing session sees %v", got)
	}
}
