package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBanAndUnban(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)
	cookie := hdr{"Cookie": cookieHeader(t, h.signUp(email, "password123"))}
	bearer, _ := h.session(email, "password123")
	id := h.userID(cookie)
	admin := h.a.Users()

	if err := admin.BanUser(ctx, id, "spam", 0); err != nil {
		t.Fatal(err)
	}
	// Existing sessions die and sign-in is refused, but only after the password matched.
	if h.getSession(cookie) != nil || h.getSession(bearer) != nil {
		t.Fatal("session survived the ban")
	}
	wantCode(t, h.signIn(email, "password123"), http.StatusForbidden, "BANNED_USER")
	wantCode(t, h.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	detail, err := admin.GetUserDetail(ctx, id)
	if err != nil || !detail.User.Banned || detail.User.BanReason == nil || *detail.User.BanReason != "spam" || detail.User.BanExpires != nil || len(detail.Sessions) != 0 {
		t.Fatalf("detail = %+v %v", detail, err)
	}
	// A session that appears anyway (e.g. created by another process) is refused.
	if _, err := testPool.Exec(ctx, `INSERT INTO "session" ("id","expiresAt","token","userId") VALUES ($1,$2,$3,$4)`,
		generateID(32), h.clock.Now().Add(time.Hour), "smuggled"+generateID(24), id); err != nil {
		t.Fatal(err)
	}
	var token string
	_ = testPool.QueryRow(ctx, `SELECT "token" FROM "session" WHERE "userId" = $1`, id).Scan(&token)
	if h.getSession(hdr{"Authorization": "Bearer " + token}) != nil {
		t.Fatal("banned user's session accepted")
	}

	// Unban restores sign-in.
	if err := admin.UnbanUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.signIn(email, "password123"), http.StatusOK)

	// A timed ban lifts itself once it expires.
	if err := admin.BanUser(ctx, id, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	wantCode(t, h.signIn(email, "password123"), http.StatusForbidden, "BANNED_USER")
	h.clock.Advance(time.Hour + time.Second)
	wantStatus(t, h.signIn(email, "password123"), http.StatusOK)
	if d, _ := admin.GetUserDetail(ctx, id); d.User.Banned || d.User.BanExpires != nil {
		t.Fatalf("expired ban not lifted: %+v", d.User)
	}

	for name, err := range map[string]error{
		"ban unknown":    admin.BanUser(ctx, generateID(32), "", 0),
		"unban unknown":  admin.UnbanUser(ctx, generateID(32)),
		"role unknown":   admin.SetRole(ctx, generateID(32), "admin"),
		"detail unknown": func() error { _, err := admin.GetUserDetail(ctx, generateID(32)); return err }(),
	} {
		if !errors.Is(err, ErrUserNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := admin.BanUser(ctx, id, "", -time.Second); err == nil {
		t.Error("negative ban duration accepted")
	}
}

func TestAdminResetTwoFactorRecoversAccess(t *testing.T) {
	h := newHarness(t, with2FA(true))
	ctx := context.Background()
	email := uniqueEmail(t)
	rec := h.signUp(email, "password123")
	pending := hdr{"Cookie": cookies(rec), "Origin": testBase}
	secret := jsonBody(t, h.do("POST", BasePath+"/two-factor/enable", map[string]any{}, pending))["secret"].(string)
	rec = h.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": h.code(secret)}, pending)
	id := h.userID(hdr{"Cookie": cookieHeader(t, rec)})

	// Lost authenticator: the admin resets 2FA; sign-in asks to enrol again.
	if err := h.a.Users().ResetTwoFactor(ctx, id); err != nil {
		t.Fatal(err)
	}
	rec = h.signIn(email, "password123")
	if body := jsonBody(t, rec); body["twoFactorSetupRequired"] != true {
		t.Fatalf("after reset sign-in = %v", body)
	}
	if err := h.a.Users().ResetTwoFactor(ctx, generateID(32)); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user err = %v", err)
	}
}

func TestDisableSignUpKeepsInvites(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DisableSignUp = true })
	ctx := context.Background()
	email := uniqueEmail(t)
	wantCode(t, h.signUp(email, "password123"), http.StatusBadRequest, "EMAIL_PASSWORD_SIGN_UP_DISABLED")
	if _, _, err := h.a.SignUpEmail(ctx, SignUpInput{Email: email, Password: "password123"}); !errors.Is(err, ErrSignUpDisabled) {
		t.Fatalf("SignUpEmail err = %v", err)
	}
	if n := len(h.mails()); n != 0 {
		t.Fatalf("disabled sign-up sent %d mails", n)
	}
	// Invites still create accounts that can sign in.
	user, err := h.a.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		t.Fatal(err)
	}
	token := mustQuery(t, h.do("GET", pathOf(t, h.mails()[0].url), nil, nil).Header().Get("Location"), "token")
	wantStatus(t, h.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "admin-password"}, nil), http.StatusOK)
	wantStatus(t, h.signIn(email, "admin-password"), http.StatusOK)
}

func TestBannedUserGetsNoTwoFactorChallenge(t *testing.T) {
	h := newHarness(t, with2FA(false))
	email := uniqueEmail(t)
	_, signedIn := h.enrol(email)
	if err := h.a.Users().BanUser(context.Background(), h.userID(signedIn), "", 0); err != nil {
		t.Fatal(err)
	}
	wantCode(t, h.signIn(email, "password123"), http.StatusForbidden, "BANNED_USER")
}

func TestListUsersAndAdminOperations(t *testing.T) {
	ctx := context.Background()
	pool := newDatabase(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil)
	a, err := New(h.a.cfg, pool, h.mail)
	if err != nil {
		t.Fatal(err)
	}
	a.now = h.clock.Now
	admin := NewUserAdmin(pool) // no secret, cookies or mailer needed
	admin.now = h.clock.Now

	names := []string{"Ada Lovelace", "Grace Hopper", "Alan Turing", "100% Real", "under_score"}
	ids := map[string]string{}
	for i, n := range names {
		user, _, err := a.SignUpEmail(ctx, SignUpInput{Name: n, Email: strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(n, " ", "."), "%", "pct")) + "@example.com", Password: "password123"})
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = user.ID
		h.clock.Advance(time.Duration(i+1) * time.Second)
	}
	if err := admin.SetRole(ctx, ids["Grace Hopper"], "admin"); err != nil {
		t.Fatal(err)
	}
	if err := admin.BanUser(ctx, ids["Alan Turing"], "", 0); err != nil {
		t.Fatal(err)
	}

	list := func(q ListUsersQuery) []string {
		t.Helper()
		page, err := admin.ListUsers(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range page.Users {
			out = append(out, u.Name)
		}
		return out
	}
	yes, no := true, false
	for name, c := range map[string]struct {
		q    ListUsersQuery
		want string
	}{
		"default newest last": {ListUsersQuery{}, "Ada Lovelace,Grace Hopper,Alan Turing,100% Real,under_score"},
		"sort desc":           {ListUsersQuery{SortDesc: true}, "under_score,100% Real,Alan Turing,Grace Hopper,Ada Lovelace"},
		"sort by name":        {ListUsersQuery{SortBy: "name"}, "100% Real,Ada Lovelace,Alan Turing,Grace Hopper,under_score"},
		"unknown sort":        {ListUsersQuery{SortBy: `"; DROP TABLE "user"; --`}, "Ada Lovelace,Grace Hopper,Alan Turing,100% Real,under_score"},
		"search email":        {ListUsersQuery{Search: "HOPPER"}, "Grace Hopper"},
		"search name":         {ListUsersQuery{Search: "al", SearchField: "name"}, "Alan Turing,100% Real"},
		"literal percent":     {ListUsersQuery{Search: "%", SearchField: "name"}, "100% Real"},
		"literal underscore":  {ListUsersQuery{Search: "_", SearchField: "name"}, "under_score"},
		"role filter":         {ListUsersQuery{Role: "admin"}, "Grace Hopper"},
		"banned filter":       {ListUsersQuery{Banned: &yes}, "Alan Turing"},
		"not banned page":     {ListUsersQuery{Banned: &no, Limit: 2, Offset: 1}, "Grace Hopper,100% Real"},
	} {
		if got := strings.Join(list(c.q), ","); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	page, _ := admin.ListUsers(ctx, ListUsersQuery{Limit: 2, Offset: 2})
	if page.Total != 5 || page.Limit != 2 || page.Offset != 2 || len(page.Users) != 2 {
		t.Fatalf("page = %+v", page)
	}
	past, _ := admin.ListUsers(ctx, ListUsersQuery{Limit: 1000, Offset: 50})
	if past.Total != 5 || len(past.Users) != 0 || past.Limit != 100 {
		t.Fatalf("past the end = %+v", past)
	}

	detail, err := admin.GetUserDetail(ctx, ids["Grace Hopper"])
	if err != nil || detail.User.Role != "admin" || !detail.User.HasPassword || detail.User.TwoFactorEnabled || len(detail.Sessions) != 1 || detail.Sessions[0].Token != "" {
		t.Fatalf("detail = %+v %v", detail, err)
	}
	if err := admin.SetRole(ctx, ids["Ada Lovelace"], "admin; DROP"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("bad role err = %v", err)
	}
	if err := admin.SetRole(ctx, ids["Ada Lovelace"], "admin,support"); err != nil {
		t.Fatal(err)
	}
	if err := admin.RevokeSession(ctx, ids["Ada Lovelace"], detail.Sessions[0].ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoking another user's session: %v", err)
	}
	if err := admin.RevokeSession(ctx, ids["Grace Hopper"], detail.Sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := admin.RevokeSession(ctx, ids["Grace Hopper"], ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("empty session id: %v", err)
	}
	if err := admin.RevokeUserSessions(ctx, ids["Ada Lovelace"]); err != nil {
		t.Fatal(err)
	}
	if d, _ := admin.GetUserDetail(ctx, ids["Ada Lovelace"]); len(d.Sessions) != 0 {
		t.Fatalf("sessions left: %+v", d.Sessions)
	}
}

func TestInviteSetPasswordFlow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := uniqueEmail(t)

	user, err := h.a.CreateUserWithoutPassword(ctx, CreateUserInput{Name: " New Admin ", Email: strings.ToUpper(email), Role: "admin"})
	if err != nil || user.Email != email || user.Name != "New Admin" || user.EmailVerified {
		t.Fatalf("created = %+v %v", user, err)
	}
	if _, err := h.a.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate err = %v", err)
	}
	if _, err := h.a.CreateUserWithoutPassword(ctx, CreateUserInput{Email: uniqueEmail(t), Role: "x y"}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("bad role err = %v", err)
	}
	// No password yet: sign-in fails like any wrong password.
	wantCode(t, h.signIn(email, "anything-at-all"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	for _, bad := range []string{"", "https://evil.test/set"} {
		if err := h.a.IssueSetPasswordLink(ctx, user.ID, bad); !errors.Is(err, ErrInvalidRedirectURL) {
			t.Fatalf("redirect %q err = %v", bad, err)
		}
	}
	if err := h.a.IssueSetPasswordLink(ctx, generateID(32), "/set"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown user err = %v", err)
	}
	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		t.Fatal(err)
	}
	first := h.mails()[0]
	if first.kind != "set-password" || first.to != email || !strings.HasPrefix(first.url, testBase+"/api/auth/reset-password/") {
		t.Fatalf("mail = %+v", first)
	}
	// A newer invite replaces the first.
	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		t.Fatal(err)
	}
	firstToken := mustQuery(t, h.do("GET", pathOf(t, first.url), nil, nil).Header().Get("Location"), "token")
	if firstToken != "" {
		t.Fatalf("superseded invite still valid")
	}
	rec := h.do("GET", pathOf(t, h.mails()[1].url), nil, nil)
	token := mustQuery(t, rec.Header().Get("Location"), "token")
	if !strings.HasPrefix(rec.Header().Get("Location"), "/set?token=") {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}

	wantCode(t, h.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "short"}, nil), http.StatusBadRequest, "PASSWORD_TOO_SHORT")
	rec = h.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "admin-password"}, nil)
	wantStatus(t, rec, http.StatusOK)
	session := h.getSession(hdr{"Cookie": cookieHeader(t, rec)})
	if session == nil || session["user"].(map[string]any)["emailVerified"] != true {
		t.Fatalf("set-password session = %v", session)
	}
	// Single use.
	wantCode(t, h.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "other-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
	wantStatus(t, h.signIn(email, "admin-password"), http.StatusOK)
	if d, _ := h.a.Users().GetUserDetail(ctx, user.ID); d.User.Role != "admin" || !d.User.HasPassword {
		t.Fatalf("detail = %+v", d.User)
	}

	// Invite links expire after InviteTTL.
	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		t.Fatal(err)
	}
	link := h.mails()[2].url
	h.clock.Advance(DefaultInviteTTL + time.Minute)
	if loc := h.do("GET", pathOf(t, link), nil, nil).Header().Get("Location"); loc != "/set?error=INVALID_TOKEN" {
		t.Fatalf("expired invite redirect = %q", loc)
	}
}

func TestInviteContinuesIntoRequiredTwoFactor(t *testing.T) {
	h := newHarness(t, with2FA(true))
	ctx := context.Background()
	email := uniqueEmail(t)
	user, err := h.a.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.a.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		t.Fatal(err)
	}
	token := mustQuery(t, h.do("GET", pathOf(t, h.mails()[0].url), nil, nil).Header().Get("Location"), "token")

	// Service form, as an HTML handler uses it.
	_, _, err = h.a.CompleteSetPassword(ctx, token, "admin-password")
	c, ok := errors.AsType[*TwoFactorChallenge](err)
	if !ok || !c.SetupRequired || c.Token == "" {
		t.Fatalf("CompleteSetPassword err = %v", err)
	}
	setup, err := h.a.StartTwoFactorSetup(ctx, c.Token)
	if err != nil || len(setup.BackupCodes) != backupCodeCount {
		t.Fatalf("setup = %+v %v", setup, err)
	}
	res, err := h.a.VerifyTwoFactor(ctx, VerifyTwoFactorInput{ChallengeToken: c.Token, Code: h.code(setup.Secret)})
	if err != nil || res.Session.Token == "" || res.User.ID != user.ID {
		t.Fatalf("verify = %+v %v", res, err)
	}
	if on, _ := h.a.TwoFactorEnabled(ctx, user.ID); !on {
		t.Fatal("2FA not enabled by enrolment")
	}
	// The enrolment challenge is spent; a fresh sign-in needs a code.
	if _, err := h.a.StartTwoFactorSetup(ctx, c.Token); !errors.Is(err, ErrInvalidTwoFactorCookie) {
		t.Fatalf("spent challenge err = %v", err)
	}
	h.tick()
	h.challenge(email, "admin-password")
}

func TestSeparateInstancesDoNotCollide(t *testing.T) {
	ctx := context.Background()
	appPool, adminPool := newDatabase(t), newDatabase(t)
	if err := Migrate(ctx, appPool); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTable(ctx, adminPool, "goose_admin_auth"); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTable(ctx, adminPool, "goose_admin_auth"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var exists bool
	_ = adminPool.QueryRow(ctx, `SELECT to_regclass('goose_admin_auth') IS NOT NULL AND to_regclass('goose_auth') IS NULL`).Scan(&exists)
	if !exists {
		t.Fatal("admin migrations did not use their own version table")
	}

	h := newHarness(t, nil)
	app, err := New(Config{Secret: testSecret, BaseURL: testBase, RateLimiter: allowAll{}}, appPool, h.mail)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := New(Config{Secret: strings.Repeat("a", 40), BaseURL: "https://admin.test", CookiePrefix: "admin", RateLimiter: allowAll{}}, adminPool, h.mail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Secret: testSecret, BaseURL: testBase, CookiePrefix: "bad;name"}, appPool, h.mail); err == nil {
		t.Fatal("invalid cookie prefix accepted")
	}

	signUp := func(a *Auth) *http.Cookie {
		h2 := &harness{t: t, a: a, h: a.Handler(), mail: h.mail, clock: h.clock}
		rec := h2.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "x", "email": "same@example.com", "password": "password123"}, nil)
		wantStatus(t, rec, http.StatusOK)
		return sessionCookie(rec)
	}
	appCookie, adminCookie := signUp(app), signUp(admins)
	if appCookie.Name != "better-auth.session_token" || adminCookie.Name != "__Secure-admin.session_token" {
		t.Fatalf("cookie names %q / %q", appCookie.Name, adminCookie.Name)
	}
	// Each instance ignores the other's cookie, even under its own name.
	get := func(a *Auth, cookie string) bool {
		h2 := &harness{t: t, a: a, h: a.Handler(), mail: h.mail, clock: h.clock}
		return h2.getSession(hdr{"Cookie": cookie}) != nil
	}
	if !get(app, appCookie.Name+"="+appCookie.Value) || !get(admins, adminCookie.Name+"="+adminCookie.Value) {
		t.Fatal("own cookie rejected")
	}
	if get(admins, adminCookie.Name+"="+appCookie.Value) || get(app, appCookie.Name+"="+adminCookie.Value) {
		t.Fatal("a cookie signed by the other instance was accepted")
	}

	// Nested (admin handler mounted inside the app's Middleware): each instance
	// still only sees its own session.
	nested := &harness{t: t, a: admins, h: app.Middleware(admins.Handler()), mail: h.mail, clock: h.clock}
	both := appCookie.Name + "=" + appCookie.Value
	if nested.getSession(hdr{"Cookie": both}) != nil {
		t.Fatal("admin instance accepted the app session through nested Middleware")
	}
	if nested.getSession(hdr{"Cookie": both + "; " + adminCookie.Name + "=" + adminCookie.Value}) == nil {
		t.Fatal("admin session lost under nested Middleware")
	}
	protected := app.Middleware(admins.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	if rec := nested.serve(protected, "GET", "/admin", nil, hdr{"Cookie": both}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin RequireUser with only an app session: %d", rec.Code)
	}
}
