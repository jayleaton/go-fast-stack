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
func (h *harness) lastMail(kind, to string) string {
	h.t.Helper()
	mails := h.mails()
	for i := len(mails) - 1; i >= 0; i-- {
		if mails[i].kind == kind && mails[i].to == to {
			return mails[i].url
		}
	}
	h.t.Fatalf("no %s mail to %s in %+v", kind, to, mails)
	return ""
}

// wantAPILink checks link is GET BasePath+path with the given callbackURL.
func wantAPILink(t *testing.T, link, pathPrefix, callbackURL string) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host != testBase || !strings.HasPrefix(u.Path, BasePath+pathPrefix) || u.Query().Get("callbackURL") != callbackURL {
		t.Fatalf("link = %s, want %s%s...?callbackURL=%s", link, BasePath, pathPrefix, callbackURL)
	}
}

func TestResetLinkShapes(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "old-password"), http.StatusOK)

	// Relative: straight to the app page, existing query kept.
	if err := h.a.RequestPasswordReset(ctx, email, "/auth/reset-password?step=1"); err != nil {
		t.Fatal(err)
	}
	link := h.lastMail("reset", email)
	token := mustQuery(t, link, "token")
	if token == "" || link != testBase+"/auth/reset-password?step=1&token="+token {
		t.Fatalf("relative reset link = %s", link)
	}
	if err := h.a.ResetPassword(ctx, token, "new-password"); err != nil {
		t.Fatalf("direct link token: %v", err)
	}
	wantStatus(t, h.signIn(email, "new-password"), http.StatusOK)

	// Absolute and deep-link targets go through the API callback.
	for _, target := range []string{testBase + "/reset", "betterstack://reset-password"} {
		if err := h.a.RequestPasswordReset(ctx, email, target); err != nil {
			t.Fatal(err)
		}
		link := h.lastMail("reset", email)
		wantAPILink(t, link, "/reset-password/", target)
		rec := h.do("GET", pathOf(t, link), nil, nil)
		wantStatus(t, rec, http.StatusFound)
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, target+"?token=") {
			t.Fatalf("%s: callback redirect = %q", target, loc)
		}
	}
}

func TestSetPasswordLinkShapes(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)
	user, err := h.a.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "betterstack://set-password"); err != nil {
		t.Fatal(err)
	}
	wantAPILink(t, h.lastMail("set-password", email), "/reset-password/", "betterstack://set-password")
	if err := h.a.IssueSetPasswordLink(ctx, user.ID, testBase+"/set"); err != nil {
		t.Fatal(err)
	}
	wantAPILink(t, h.lastMail("set-password", email), "/reset-password/", testBase+"/set")

	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "/set-password"); err != nil {
		t.Fatal(err)
	}
	link := h.lastMail("set-password", email)
	token := mustQuery(t, link, "token")
	if token == "" || link != testBase+"/set-password?token="+token {
		t.Fatalf("relative set-password link = %s", link)
	}
	if _, sess, err := h.a.CompleteSetPassword(ctx, token, "admin-password"); err != nil || sess.Token == "" {
		t.Fatalf("direct link token: %+v %v", sess, err)
	}
}

func TestVerificationLinkShapes(t *testing.T) {
	ctx := context.Background()
	signUp := func(h *harness, callbackURL string) (string, string) {
		h.t.Helper()
		email := uniqueEmail(h.t)
		if _, _, err := h.a.SignUpEmail(ctx, SignUpInput{Name: "V", Email: email, Password: "password123", CallbackURL: callbackURL}); err != nil {
			h.t.Fatal(err)
		}
		return email, h.lastMail("verification", email)
	}

	// Without VerifyEmailPath every link is the API link.
	h := newHarness(t, func(c *Config) { c.RequireEmailVerification = true })
	_, link := signUp(h, "/done")
	wantAPILink(t, link, "/verify-email", "/done")

	h = newHarness(t, func(c *Config) {
		c.RequireEmailVerification = true
		c.VerifyEmailPath = verifyPage
	})
	for callback, want := range map[string]string{"/done?x=1": "/done?x=1", "": "/"} {
		email, link := signUp(h, callback)
		token := mustQuery(t, link, "token")
		if token == "" || link != testBase+verifyPage+"?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(want) {
			t.Fatalf("callback %q: link = %s", callback, link)
		}
		// The page spends the token in-process: verified and signed in.
		user, sess, err := h.a.VerifyEmail(ctx, token)
		if err != nil || user.Email != email || !user.EmailVerified || sess.Token == "" {
			t.Fatalf("direct link token: %+v %+v %v", user, sess, err)
		}
	}

	// Absolute and deep-link callbacks open the page too, which hands them
	// back through CheckCallbackURL.
	for _, callback := range []string{testBase + "/done", "betterstack://verified"} {
		_, link := signUp(h, callback)
		token := mustQuery(t, link, "token")
		if link != testBase+verifyPage+"?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(callback) {
			t.Fatalf("%s: link = %s", callback, link)
		}
		if err := h.a.CheckCallbackURL(callback); err != nil {
			t.Fatalf("%s not trusted: %v", callback, err)
		}

		// An old API link forwards to the page without spending the token.
		rec := h.do("GET", "/api/auth/verify-email?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(callback), nil, nil)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != link || sessionCookie(rec) != nil {
			t.Fatalf("%s: GET %d %q", callback, rec.Code, loc)
		}
		if _, sess, err := h.a.VerifyEmail(ctx, token); err != nil || sess.Token == "" {
			t.Fatalf("%s: token spent by GET: %v", callback, err)
		}
	}
	for _, bad := range []string{"", "https://evil.example/x", "evil://x", "//evil.example"} {
		if err := h.a.CheckCallbackURL(bad); !errors.Is(err, ErrInvalidCallbackURL) {
			t.Errorf("CheckCallbackURL(%q) = %v", bad, err)
		}
	}
}

func TestChangeEmailLinkShapes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.VerifyEmailPath = verifyPage })
	email := uniqueEmail(t)
	wantStatus(t, h.signUp(email, "password123"), http.StatusOK)
	cookie := h.loginCookie(email)
	id := h.userID(hdr{"Cookie": cookie})

	// Absolute and deep-link callbacks open the page too.
	for _, callback := range []string{testBase + "/account", "betterstack://account"} {
		next := uniqueEmail(t)
		if err := h.a.ChangeEmail(ctx, id, next, callback); err != nil {
			t.Fatal(err)
		}
		if link := h.lastMail("verification", next); !strings.HasPrefix(link, testBase+verifyPage+"?token=") || mustQuery(t, link, "callbackURL") != callback {
			t.Fatalf("%s: change-email link = %s", callback, link)
		}
	}

	// Relative: the app page, which spends the token in-process.
	const callback = "/dashboard/account?email-changed=1"
	next := uniqueEmail(t)
	if err := h.a.ChangeEmail(ctx, id, next, callback); err != nil {
		t.Fatal(err)
	}
	link := h.lastMail("verification", next)
	token := mustQuery(t, link, "token")
	if token == "" || link != testBase+verifyPage+"?token="+url.QueryEscape(token)+"&callbackURL="+url.QueryEscape(callback) {
		t.Fatalf("change-email link = %s", link)
	}
	user, sess, err := h.a.VerifyEmail(ctx, token)
	if err != nil || user.Email != next || !user.EmailVerified || sess.Token == "" || sess.UserID != id {
		t.Fatalf("direct link token: %+v %+v %v", user, sess, err)
	}
	if got := h.getSession(hdr{"Cookie": cookie})["user"].(map[string]any)["email"]; got != next {
		t.Fatalf("existing session sees %v", got)
	}
}
