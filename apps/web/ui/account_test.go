package ui_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/routes"
)

var (
	ua           = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	ip           = "192.0.2.10"
	demoSessions = []services.Session{
		{ID: "sess-1", Current: true, UserAgent: &ua, IPAddress: &ip, CreatedAt: time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 1, 9, 9, 0, 0, 0, time.UTC)},
		{ID: "sess-2", CreatedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 1, 8, 9, 0, 0, 0, time.UTC)},
	}
	demoSummary = services.UserDataSummary{Sessions: 2}
)

// accountActions are the Account page's posts with a valid form each.
var accountActions = []struct {
	name, path string
	form       url.Values
}{
	{"profile", routes.AccountProfile, url.Values{"name": {"Ada"}}},
	{"email", routes.AccountEmail, url.Values{"newEmail": {"ada@example.com"}}},
	{"password", routes.AccountPassword, url.Values{"currentPassword": {"old-password"}, "newPassword": {"new-password"}, "confirmPassword": {"new-password"}}},
	{"revoke session", routes.AccountRevokeSession, url.Values{"sessionId": {"sess-2"}}},
	{"revoke others", routes.AccountRevokeOthers, url.Values{}},
	{"delete", routes.AccountDelete, url.Values{}},
}

func TestAccountAnonymous(t *testing.T) {
	e := newEnv(t, config.Features{})
	assertSeeOther(t, e.do(http.MethodGet, config.AppRoutes.Account, nil), config.AppRoutes.Login)
	for _, a := range accountActions {
		t.Run(a.name, func(t *testing.T) {
			assertRedirectScript(t, e.do(http.MethodPost, a.path, a.form, datastar), config.AppRoutes.Login)
			assertSeeOther(t, e.do(http.MethodPost, a.path, a.form), config.AppRoutes.Login)
		})
	}
}

func TestAccountPage(t *testing.T) {
	e := newEnv(t, config.Features{})
	e.signedIn("sess-1")
	e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(demoSessions, nil)
	e.users.EXPECT().DataSummary(gomock.Any(), demoActor, "u1").Return(demoSummary, nil)
	rec := e.do(http.MethodGet, config.AppRoutes.EmailChanged, nil, lang("ja"))
	body := rec.Body.String()
	for _, want := range []string{
		ja("pages.account.title"), ja("pages.account.emailChanged", "email", "demo@example.com"),
		ja("pages.account.sessions.thisDevice"), ja("pages.account.sessions.device", "browser", "Chrome", "os", "macOS"),
		`data-session-id="sess-2"`, `name="sessionId" value="sess-2"`, ja("pages.account.danger.summary", "count", "2"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="sessionId" value="sess-1"`) {
		t.Error("the current session must not offer a revoke button")
	}

	// A change-email link that failed lands with auth's error code instead.
	e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(demoSessions, nil)
	e.users.EXPECT().DataSummary(gomock.Any(), demoActor, "u1").Return(demoSummary, nil)
	body = e.do(http.MethodGet, config.AppRoutes.EmailChanged+"&error=INVALID_USER", nil).Body.String()
	if !strings.Contains(body, en("pages.verifyEmail.invalidUser")) || strings.Contains(body, en("pages.account.emailChanged", "email", "demo@example.com")) {
		t.Error("failed link must show the auth error, not the changed banner")
	}

	// A failing load still renders the page, with the reason.
	e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(nil, errors.New("db down"))
	e.users.EXPECT().DataSummary(gomock.Any(), demoActor, "u1").Return(demoSummary, nil)
	if rec := e.do(http.MethodGet, config.AppRoutes.Account, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), en("errors.somethingWentWrong")) {
		t.Errorf("load error: %d", rec.Code)
	}
}

func TestAccountHappyPaths(t *testing.T) {
	img := "https://example.com/a.png"

	t.Run("profile", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.signedIn("sess-1")
		e.users.EXPECT().UpdateProfile(gomock.Any(), demoActor, "u1", gomock.Cond(func(in validate.UpdateUserSettings) bool {
			return *in.Name == "Ada" && in.Image.Set && *in.Image.Value == img
		})).Return(services.User{ID: "u1", Name: "Ada", Email: "demo@example.com", Image: &img}, nil)
		rec := e.do(http.MethodPost, routes.AccountProfile, url.Values{"name": {"  Ada "}, "image": {img}}, datastar)
		assertSSE(t, rec, `id="profile-card"`, en("pages.account.profile.saved"), `value="Ada"`, `src="`+img+`"`)

		// An empty image clears it.
		e.users.EXPECT().UpdateProfile(gomock.Any(), demoActor, "u1", gomock.Cond(func(in validate.UpdateUserSettings) bool {
			return in.Image.Set && in.Image.Value == nil
		})).Return(services.User{ID: "u1", Name: "Ada"}, nil)
		assertSSE(t, e.do(http.MethodPost, routes.AccountProfile, url.Values{"name": {"Ada"}, "image": {""}}, datastar), en("pages.account.profile.saved"))
	})

	t.Run("email", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.signedIn("sess-1")
		e.users.EXPECT().RequestEmailChange(gomock.Any(), demoActor, "u1", validate.UpdateUserEmail{NewEmail: "ada@example.com", CallbackURL: config.AppRoutes.EmailChanged}).Return(nil)
		assertSSE(t, e.do(http.MethodPost, routes.AccountEmail, url.Values{"newEmail": {" Ada@Example.com "}}, datastar, lang("ja")),
			`id="email-card"`, ja("pages.account.email.checkInbox"), ja("pages.account.email.sentTo", "email", "ada@example.com"))
	})

	t.Run("password", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.signedIn("sess-1")
		e.users.EXPECT().ChangePassword(gomock.Any(), demoActor, "u1", validate.UpdateUserPassword{CurrentPassword: "old-password", NewPassword: "new-password", RevokeOtherSessions: true}).Return(nil)
		e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(demoSessions[:1], nil)
		form := url.Values{"currentPassword": {"old-password"}, "newPassword": {"new-password"}, "confirmPassword": {"new-password"}, "revokeOtherSessions": {"on"}}
		// Replaced, not morphed, so the typed passwords are cleared.
		assertSSE(t, e.do(http.MethodPost, routes.AccountPassword, form, datastar), "data: mode replace", `id="password-card"`, en("pages.account.password.changedRevoked"), `id="sessions-card"`, en("pages.account.sessions.none"))

		// A mismatch never reaches the service.
		form.Set("confirmPassword", "typo-password")
		assertSSE(t, e.do(http.MethodPost, routes.AccountPassword, form, datastar), `id="password-status"`, en("pages.account.password.mismatch"))
	})

	t.Run("sessions", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.signedIn("sess-1")
		e.users.EXPECT().RevokeSession(gomock.Any(), demoActor, "u1", "sess-2").Return(nil)
		e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(demoSessions[:1], nil).Times(2)
		assertSSE(t, e.do(http.MethodPost, routes.AccountRevokeSession, url.Values{"sessionId": {"sess-2"}}, datastar), `id="sessions-card"`, en("pages.account.sessions.revoked"))
		e.users.EXPECT().RevokeOtherSessions(gomock.Any(), demoActor, "u1").Return(nil)
		assertSSE(t, e.do(http.MethodPost, routes.AccountRevokeOthers, url.Values{}, datastar), en("pages.account.sessions.othersRevoked"))

		// The revoke worked but the list cannot be reloaded: say why instead.
		e.users.EXPECT().RevokeSession(gomock.Any(), demoActor, "u1", "sess-2").Return(nil)
		e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(nil, errors.New("db down"))
		assertSSE(t, e.do(http.MethodPost, routes.AccountRevokeSession, url.Values{"sessionId": {"sess-2"}}, datastar), `id="sessions-status"`, en("errors.somethingWentWrong"))
	})

	t.Run("delete", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.signedIn("sess-1")
		e.users.EXPECT().DeleteAccount(gomock.Any(), demoActor, "u1").Return(nil)
		e.auth.EXPECT().SignOut(gomock.Any(), demoSession.Token).Return(nil)
		e.auth.EXPECT().ClearSessionCookie(gomock.Any())
		assertRedirectScript(t, e.do(http.MethodPost, routes.AccountDelete, url.Values{}, datastar), config.AppRoutes.Home)

		// A failed sign-out after the delete still clears the cookie and leaves.
		e.users.EXPECT().DeleteAccount(gomock.Any(), demoActor, "u1").Return(nil)
		e.auth.EXPECT().SignOut(gomock.Any(), demoSession.Token).Return(errors.New("gone"))
		e.auth.EXPECT().ClearSessionCookie(gomock.Any())
		assertRedirectScript(t, e.do(http.MethodPost, routes.AccountDelete, url.Values{}, datastar), config.AppRoutes.Home)
	})

	t.Run("without JS", func(t *testing.T) {
		e := newEnv(t, config.Features{})
		e.signedIn("sess-1")
		e.users.EXPECT().UpdateProfile(gomock.Any(), demoActor, "u1", gomock.Any()).Return(services.User{ID: "u1", Name: "Ada"}, nil)
		assertSeeOther(t, e.do(http.MethodPost, routes.AccountProfile, url.Values{"name": {"Ada"}}), config.AppRoutes.Account)
	})
}

// TestAccountErrors renders every named service error and kind translated,
// through the revoke-others action.
func TestAccountErrors(t *testing.T) {
	cases := []struct {
		err error
		key string
	}{
		{services.ErrInvalidPassword, "pages.account.errors.wrongPassword"},
		{services.ErrPasswordPolicy, "pages.account.errors.passwordPolicy"},
		{services.ErrSamePassword, "pages.account.errors.samePassword"},
		{services.ErrNoPassword, "pages.account.errors.noPassword"},
		{services.ErrSameEmail, "pages.account.errors.sameEmail"},
		{services.ErrEmailTaken, "errors.emailTaken"},
		{services.ErrSessionNotFresh, "pages.account.errors.sessionNotFresh"},
		{services.ErrSessionNotFound, "pages.account.errors.sessionNotFound"},
		{services.ErrCurrentSession, "pages.account.errors.currentSession"},
		{services.ErrRateLimited, "errors.tooManyRequests"},
		{services.ErrUserNotFound, "errors.notFound"},
		{services.ErrInvalidCallbackURL, "errors.invalidInput"},
		{fmt.Errorf("wrapped: %w", services.ErrForbidden), "errors.forbidden"},
		{&services.Error{Kind: services.KindInvalid, Message: "a rule added later"}, "errors.invalidInput"},
		{&services.Error{Kind: services.KindConflict, Message: "a conflict added later"}, "errors.conflict"},
		{&services.Error{Kind: services.KindInternal, Message: "boom"}, "errors.somethingWentWrong"},
		{errors.New("db down"), "errors.somethingWentWrong"},
	}
	for _, c := range cases {
		t.Run(c.err.Error(), func(t *testing.T) {
			e := newEnv(t, config.Features{})
			e.signedIn("sess-1")
			e.users.EXPECT().RevokeOtherSessions(gomock.Any(), demoActor, "u1").Return(c.err)
			e.users.EXPECT().ListSessions(gomock.Any(), demoActor, "u1").Return(demoSessions, nil)
			assertSSE(t, e.do(http.MethodPost, routes.AccountRevokeOthers, url.Values{}, datastar, lang("ja")), `id="sessions-card"`, ja(c.key))
		})
	}

	// Each form patches its own status slot with the error.
	forms := []struct {
		name, path, slot string
		form             url.Values
		expect           func(*env)
	}{
		{"profile", routes.AccountProfile, "profile-status", url.Values{"name": {"Ada"}}, func(e *env) {
			e.users.EXPECT().UpdateProfile(gomock.Any(), demoActor, "u1", gomock.Any()).Return(services.User{}, services.ErrUserNotFound)
		}},
		{"email", routes.AccountEmail, "email-status", url.Values{"newEmail": {"taken@example.com"}}, func(e *env) {
			e.users.EXPECT().RequestEmailChange(gomock.Any(), demoActor, "u1", gomock.Any()).Return(services.ErrEmailTaken)
		}},
		{"password", routes.AccountPassword, "password-status", url.Values{"currentPassword": {"wrong-pass"}, "newPassword": {"new-password"}, "confirmPassword": {"new-password"}}, func(e *env) {
			e.users.EXPECT().ChangePassword(gomock.Any(), demoActor, "u1", gomock.Any()).Return(services.ErrInvalidPassword)
		}},
		{"delete", routes.AccountDelete, "delete-error", url.Values{}, func(e *env) {
			e.users.EXPECT().DeleteAccount(gomock.Any(), demoActor, "u1").Return(services.ErrForbidden)
		}},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t, config.Features{})
			e.signedIn("sess-1")
			f.expect(e)
			rec := e.do(http.MethodPost, f.path, f.form, datastar)
			assertSSE(t, rec, `id="`+f.slot+`"`, `role="alert"`)
		})
	}

	// Validator failures never reach the service.
	validation := []struct {
		name, path string
		form       url.Values
		key        string
	}{
		{"name", routes.AccountProfile, url.Values{"name": {"   "}}, "pages.account.errors.name"},
		{"image", routes.AccountProfile, url.Values{"name": {"Ada"}, "image": {"ftp://x"}}, "pages.account.errors.image"},
		{"email", routes.AccountEmail, url.Values{"newEmail": {"not-an-email"}}, "errors.emailInvalid"},
		{"password", routes.AccountPassword, url.Values{"currentPassword": {""}, "newPassword": {"x"}, "confirmPassword": {"x"}}, "errors.passwordRequired"},
	}
	for _, v := range validation {
		t.Run("invalid "+v.name, func(t *testing.T) {
			e := newEnv(t, config.Features{})
			e.signedIn("sess-1")
			assertSSE(t, e.do(http.MethodPost, v.path, v.form, datastar, lang("ja")), ja(v.key))
		})
	}
}

// sseReader reads Datastar events from a live stream.
type sseReader struct {
	t  *testing.T
	sc *bufio.Scanner
}

// next returns the data of the next event.
func (s *sseReader) next() string {
	s.t.Helper()
	var data strings.Builder
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if data.Len() > 0 {
				return data.String()
			}
			continue
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data.WriteString(d + "\n")
		}
	}
	s.t.Fatalf("stream ended: %v", s.sc.Err())
	return ""
}

// TestLiveStream covers the server-push example over a real connection: the
// first patch, a profile save from the Account page (account card + notice),
// and the stream signing its tab out when another device revokes it.
func TestLiveStream(t *testing.T) {
	e := newEnv(t, config.Features{})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	e.signedIn("sess-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+routes.DashboardLive+"?tab=A", nil)
	req.Header.Set("Datastar-Request", "true")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	s := &sseReader{t: t, sc: bufio.NewScanner(res.Body)}
	if ev := s.next(); !strings.Contains(ev, `id="live-body"`) || !strings.Contains(ev, en("pages.dashboard.live.serverTime")) {
		t.Fatalf("first event: %s", ev)
	}

	e.users.EXPECT().UpdateProfile(gomock.Any(), demoActor, "u1", gomock.Any()).Return(services.User{ID: "u1", Name: "From Account", Email: "demo@example.com"}, nil)
	assertSSE(t, e.do(http.MethodPost, routes.AccountProfile, url.Values{"name": {"From Account"}}, datastar), en("pages.account.profile.saved"))
	if ev := s.next(); !strings.Contains(ev, `id="account-card"`) || !strings.Contains(ev, "From Account") {
		t.Fatalf("expected account card patch, got: %s", ev)
	}
	if ev := s.next(); !strings.Contains(ev, `id="live-notice"`) {
		t.Fatalf("expected notice for a change made elsewhere, got: %s", ev)
	}
	_ = s.next() // live body after the event

	// Another device signs out every session but its own.
	e.signedIn("sess-9")
	other := demoActor
	other.SessionID = "sess-9"
	e.users.EXPECT().RevokeOtherSessions(gomock.Any(), other, "u1").Return(nil)
	e.users.EXPECT().ListSessions(gomock.Any(), other, "u1").Return(nil, nil)
	assertSSE(t, e.do(http.MethodPost, routes.AccountRevokeOthers, url.Values{}, datastar), en("pages.account.sessions.othersRevoked"))
	if ev := s.next(); !strings.Contains(ev, `window.location.href = "`+config.AppRoutes.Login+`"`) {
		t.Fatalf("expected the revoked tab to go to login, got: %s", ev)
	}
}

// TestLiveStreamAccountDeleted: deleting the account on another device sends
// every open dashboard home.
func TestLiveStreamAccountDeleted(t *testing.T) {
	e := newEnv(t, config.Features{})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	e.signedIn("sess-1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+routes.DashboardLive+"?tab=A", nil)
	req.Header.Set("Datastar-Request", "true")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	s := &sseReader{t: t, sc: bufio.NewScanner(res.Body)}
	_ = s.next() // first live body

	e.signedIn("sess-9")
	other := demoActor
	other.SessionID = "sess-9"
	e.users.EXPECT().DeleteAccount(gomock.Any(), other, "u1").Return(nil)
	e.auth.EXPECT().SignOut(gomock.Any(), gomock.Any()).Return(nil)
	e.auth.EXPECT().ClearSessionCookie(gomock.Any())
	assertRedirectScript(t, e.do(http.MethodPost, routes.AccountDelete, url.Values{}, datastar), config.AppRoutes.Home)
	if ev := s.next(); !strings.Contains(ev, `window.location.href = "`+config.AppRoutes.Home+`"`) {
		t.Fatalf("expected the deleted account's tab to go home, got: %s", ev)
	}
}
