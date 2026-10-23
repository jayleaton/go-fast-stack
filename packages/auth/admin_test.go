package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBanAndUnban(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)
	cookie := hdr{"Cookie": cookieHeader(test, harness.signUp(email, "password123"))}
	bearer, _ := harness.session(email, "password123")
	id := harness.userID(cookie)
	admin := harness.auth.Users()

	if err := admin.BanUser(ctx, id, "spam", 0); err != nil {
		test.Fatal(err)
	}
	// Existing sessions die and sign-in is refused, but only after the password matched.
	if harness.getSession(cookie) != nil || harness.getSession(bearer) != nil {
		test.Fatal("session survived the ban")
	}
	wantCode(test, harness.signIn(email, "password123"), http.StatusForbidden, "BANNED_USER")
	wantCode(test, harness.signIn(email, "wrong-password"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")
	detail, err := admin.GetUserDetail(ctx, id)
	if err != nil || !detail.User.Banned || detail.User.BanReason == nil || *detail.User.BanReason != "spam" || detail.User.BanExpires != nil || len(detail.Sessions) != 0 {
		test.Fatalf("detail = %+v %v", detail, err)
	}
	// A session that appears anyway (e.g. created by another process) is refused.
	if _, err := testPool.Exec(ctx, `INSERT INTO "session" ("id","expiresAt","token","userId") VALUES ($1,$2,$3,$4)`,
		generateID(32), harness.clock.Now().Add(time.Hour), "smuggled"+generateID(24), id); err != nil {
		test.Fatal(err)
	}
	var token string
	_ = testPool.QueryRow(ctx, `SELECT "token" FROM "session" WHERE "userId" = $1`, id).Scan(&token)
	if harness.getSession(hdr{"Authorization": "Bearer " + token}) != nil {
		test.Fatal("banned user's session accepted")
	}

	// Unban restores sign-in.
	if err := admin.UnbanUser(ctx, id); err != nil {
		test.Fatal(err)
	}
	wantStatus(test, harness.signIn(email, "password123"), http.StatusOK)

	// A timed ban lifts itself once it expires.
	if err := admin.BanUser(ctx, id, "", time.Hour); err != nil {
		test.Fatal(err)
	}
	wantCode(test, harness.signIn(email, "password123"), http.StatusForbidden, "BANNED_USER")
	harness.clock.Advance(time.Hour + time.Second)
	wantStatus(test, harness.signIn(email, "password123"), http.StatusOK)
	if detail, _ := admin.GetUserDetail(ctx, id); detail.User.Banned || detail.User.BanExpires != nil {
		test.Fatalf("expired ban not lifted: %+v", detail.User)
	}

	for name, err := range map[string]error{
		"ban unknown":    admin.BanUser(ctx, generateID(32), "", 0),
		"unban unknown":  admin.UnbanUser(ctx, generateID(32)),
		"role unknown":   admin.SetRole(ctx, generateID(32), "admin"),
		"detail unknown": func() error { _, err := admin.GetUserDetail(ctx, generateID(32)); return err }(),
	} {
		if !errors.Is(err, ErrUserNotFound) {
			test.Errorf("%s: %v", name, err)
		}
	}
	if err := admin.BanUser(ctx, id, "", -time.Second); err == nil {
		test.Error("negative ban duration accepted")
	}
}

func TestAdminResetTwoFactorRecoversAccess(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(true))
	ctx := context.Background()
	email := uniqueEmail(test)
	rec := harness.signUp(email, "password123")
	pending := hdr{"Cookie": cookies(rec), "Origin": testBase}
	secret := jsonBody(test, harness.do("POST", BasePath+"/two-factor/enable", map[string]any{}, pending))["secret"].(string)
	rec = harness.do("POST", BasePath+"/two-factor/verify-totp", map[string]any{"code": harness.code(secret)}, pending)
	id := harness.userID(hdr{"Cookie": cookieHeader(test, rec)})

	// Lost authenticator: the admin resets 2FA; sign-in asks to enrol again.
	if err := harness.auth.Users().ResetTwoFactor(ctx, id); err != nil {
		test.Fatal(err)
	}
	rec = harness.signIn(email, "password123")
	if body := jsonBody(test, rec); body["twoFactorSetupRequired"] != true {
		test.Fatalf("after reset sign-in = %v", body)
	}
	if err := harness.auth.Users().ResetTwoFactor(ctx, generateID(32)); !errors.Is(err, ErrUserNotFound) {
		test.Fatalf("unknown user err = %v", err)
	}
}

func TestDisableSignUpKeepsInvites(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, func(config *Config) { config.DisableSignUp = true })
	ctx := context.Background()
	email := uniqueEmail(test)
	wantCode(test, harness.signUp(email, "password123"), http.StatusBadRequest, "EMAIL_PASSWORD_SIGN_UP_DISABLED")
	if _, _, err := harness.auth.SignUpEmail(ctx, SignUpInput{Email: email, Password: "password123"}); !errors.Is(err, ErrSignUpDisabled) {
		test.Fatalf("SignUpEmail err = %v", err)
	}
	if count := len(harness.mails()); count != 0 {
		test.Fatalf("disabled sign-up sent %d mails", count)
	}
	// Invites still create accounts that can sign in.
	user, err := harness.auth.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email, Role: "admin"})
	if err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		test.Fatal(err)
	}
	token := mustQuery(test, harness.mails()[0].url, "token")
	wantStatus(test, harness.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "admin-password"}, nil), http.StatusOK)
	wantStatus(test, harness.signIn(email, "admin-password"), http.StatusOK)
}

func TestBannedUserGetsNoTwoFactorChallenge(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(false))
	email := uniqueEmail(test)
	_, signedIn := harness.enrol(email)
	if err := harness.auth.Users().BanUser(context.Background(), harness.userID(signedIn), "", 0); err != nil {
		test.Fatal(err)
	}
	wantCode(test, harness.signIn(email, "password123"), http.StatusForbidden, "BANNED_USER")
}

func TestListUsersAndAdminOperations(test *testing.T) {
	test.Parallel()
	ctx := context.Background()
	pool := newDatabase(test) // lists and counts every user: not the shared database
	harness := newHarness(test, nil)
	auth, err := New(harness.auth.cfg, pool, harness.mail)
	if err != nil {
		test.Fatal(err)
	}
	auth.now = harness.clock.Now
	admin := NewUserAdmin(pool) // no secret, cookies or mailer needed
	admin.now = harness.clock.Now

	names := []string{"Ada Lovelace", "Grace Hopper", "Alan Turing", "100% Real", "under_score"}
	ids := map[string]string{}
	for index, name := range names {
		user, _, err := auth.SignUpEmail(ctx, SignUpInput{Name: name, Email: strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, " ", "."), "%", "pct")) + "@example.com", Password: "password123"})
		if err != nil {
			test.Fatal(err)
		}
		ids[name] = user.ID
		harness.clock.Advance(time.Duration(index+1) * time.Second)
	}
	if err := admin.SetRole(ctx, ids["Grace Hopper"], "admin"); err != nil {
		test.Fatal(err)
	}
	if err := admin.BanUser(ctx, ids["Alan Turing"], "", 0); err != nil {
		test.Fatal(err)
	}

	list := func(query ListUsersQuery) []string {
		test.Helper()
		page, err := admin.ListUsers(ctx, query)
		if err != nil {
			test.Fatal(err)
		}
		var out []string
		for _, user := range page.Users {
			out = append(out, user.Name)
		}
		return out
	}
	yes, no := true, false
	for name, testCase := range map[string]struct {
		query ListUsersQuery
		want  string
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
		if got := strings.Join(list(testCase.query), ","); got != testCase.want {
			test.Errorf("%s: got %q, want %q", name, got, testCase.want)
		}
	}
	page, _ := admin.ListUsers(ctx, ListUsersQuery{Limit: 2, Offset: 2})
	if page.Total != 5 || page.Limit != 2 || page.Offset != 2 || len(page.Users) != 2 {
		test.Fatalf("page = %+v", page)
	}
	past, _ := admin.ListUsers(ctx, ListUsersQuery{Limit: 1000, Offset: 50})
	if past.Total != 5 || len(past.Users) != 0 || past.Limit != 100 {
		test.Fatalf("past the end = %+v", past)
	}

	detail, err := admin.GetUserDetail(ctx, ids["Grace Hopper"])
	if err != nil || detail.User.Role != "admin" || !detail.User.HasPassword || detail.User.TwoFactorEnabled || len(detail.Sessions) != 1 || detail.Sessions[0].Token != "" {
		test.Fatalf("detail = %+v %v", detail, err)
	}
	if err := admin.SetRole(ctx, ids["Ada Lovelace"], "admin; DROP"); !errors.Is(err, ErrInvalidRole) {
		test.Fatalf("bad role err = %v", err)
	}
	if err := admin.SetRole(ctx, ids["Ada Lovelace"], "admin,support"); err != nil {
		test.Fatal(err)
	}
	if err := admin.RevokeSession(ctx, ids["Ada Lovelace"], detail.Sessions[0].ID); !errors.Is(err, ErrSessionNotFound) {
		test.Fatalf("revoking another user's session: %v", err)
	}
	if err := admin.RevokeSession(ctx, ids["Grace Hopper"], detail.Sessions[0].ID); err != nil {
		test.Fatal(err)
	}
	if err := admin.RevokeSession(ctx, ids["Grace Hopper"], ""); !errors.Is(err, ErrSessionNotFound) {
		test.Fatalf("empty session id: %v", err)
	}
	if err := admin.RevokeUserSessions(ctx, ids["Ada Lovelace"]); err != nil {
		test.Fatal(err)
	}
	if detail, _ := admin.GetUserDetail(ctx, ids["Ada Lovelace"]); len(detail.Sessions) != 0 {
		test.Fatalf("sessions left: %+v", detail.Sessions)
	}
}

func TestInviteSetPasswordFlow(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, nil)
	ctx := context.Background()
	email := uniqueEmail(test)

	user, err := harness.auth.CreateUserWithoutPassword(ctx, CreateUserInput{Name: " New Admin ", Email: strings.ToUpper(email), Role: "admin"})
	if err != nil || user.Email != email || user.Name != "New Admin" || user.EmailVerified {
		test.Fatalf("created = %+v %v", user, err)
	}
	if _, err := harness.auth.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email}); !errors.Is(err, ErrEmailTaken) {
		test.Fatalf("duplicate err = %v", err)
	}
	if _, err := harness.auth.CreateUserWithoutPassword(ctx, CreateUserInput{Email: uniqueEmail(test), Role: "x y"}); !errors.Is(err, ErrInvalidRole) {
		test.Fatalf("bad role err = %v", err)
	}
	// No password yet: sign-in fails like any wrong password.
	wantCode(test, harness.signIn(email, "anything-at-all"), http.StatusUnauthorized, "INVALID_EMAIL_OR_PASSWORD")

	for _, bad := range []string{"", "https://evil.test/set"} {
		if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, bad); !errors.Is(err, ErrInvalidRedirectURL) {
			test.Fatalf("redirect %q err = %v", bad, err)
		}
	}
	if err := harness.auth.IssueSetPasswordLink(ctx, generateID(32), "/set"); !errors.Is(err, ErrUserNotFound) {
		test.Fatalf("unknown user err = %v", err)
	}
	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		test.Fatal(err)
	}
	first := harness.mails()[0]
	if first.kind != "set-password" || first.to != email || !strings.HasPrefix(first.url, testBase+"/set?token=") {
		test.Fatalf("mail = %+v", first)
	}
	// A newer invite replaces the first.
	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		test.Fatal(err)
	}
	if ok, err := harness.auth.resetTokenValid(ctx, mustQuery(test, first.url, "token")); ok || err != nil {
		test.Fatalf("superseded invite still valid (%v)", err)
	}
	token := mustQuery(test, harness.mails()[1].url, "token")

	wantCode(test, harness.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "short"}, nil), http.StatusBadRequest, "PASSWORD_TOO_SHORT")
	rec := harness.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "admin-password"}, nil)
	wantStatus(test, rec, http.StatusOK)
	session := harness.getSession(hdr{"Cookie": cookieHeader(test, rec)})
	if session == nil || session["user"].(map[string]any)["emailVerified"] != true {
		test.Fatalf("set-password session = %v", session)
	}
	// Single use.
	wantCode(test, harness.do("POST", BasePath+"/set-password", map[string]any{"token": token, "newPassword": "other-password"}, nil), http.StatusBadRequest, "INVALID_TOKEN")
	wantStatus(test, harness.signIn(email, "admin-password"), http.StatusOK)
	if detail, _ := harness.auth.Users().GetUserDetail(ctx, user.ID); detail.User.Role != "admin" || !detail.User.HasPassword {
		test.Fatalf("detail = %+v", detail.User)
	}

	// Invite links expire after InviteTTL.
	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		test.Fatal(err)
	}
	link := harness.mails()[2].url
	harness.clock.Advance(DefaultInviteTTL + time.Minute)
	if loc := harness.do("GET", BasePath+"/reset-password/"+mustQuery(test, link, "token")+"?callbackURL=/set", nil, nil).Header().Get("Location"); loc != "/set?error=INVALID_TOKEN" {
		test.Fatalf("expired invite redirect = %q", loc)
	}
}

func TestInviteContinuesIntoRequiredTwoFactor(test *testing.T) {
	test.Parallel()
	harness := newHarness(test, with2FA(true))
	ctx := context.Background()
	email := uniqueEmail(test)
	user, err := harness.auth.CreateUserWithoutPassword(ctx, CreateUserInput{Email: email, Role: "admin"})
	if err != nil {
		test.Fatal(err)
	}
	if err := harness.auth.IssueSetPasswordLink(ctx, user.ID, "/set"); err != nil {
		test.Fatal(err)
	}
	token := mustQuery(test, harness.mails()[0].url, "token")

	// Service form, as an HTML handler uses it.
	_, _, err = harness.auth.CompleteSetPassword(ctx, token, "admin-password")
	challenge, ok := errors.AsType[*TwoFactorChallenge](err)
	if !ok || !challenge.SetupRequired || challenge.Token == "" {
		test.Fatalf("CompleteSetPassword err = %v", err)
	}
	setup, err := harness.auth.StartTwoFactorSetup(ctx, challenge.Token)
	if err != nil || len(setup.BackupCodes) != backupCodeCount {
		test.Fatalf("setup = %+v %v", setup, err)
	}
	res, err := harness.auth.VerifyTwoFactor(ctx, VerifyTwoFactorInput{ChallengeToken: challenge.Token, Code: harness.code(setup.Secret)})
	if err != nil || res.Session.Token == "" || res.User.ID != user.ID {
		test.Fatalf("verify = %+v %v", res, err)
	}
	if on, _ := harness.auth.TwoFactorEnabled(ctx, user.ID); !on {
		test.Fatal("2FA not enabled by enrolment")
	}
	// The enrolment challenge is spent; a fresh sign-in needs a code.
	if _, err := harness.auth.StartTwoFactorSetup(ctx, challenge.Token); !errors.Is(err, ErrInvalidTwoFactorCookie) {
		test.Fatalf("spent challenge err = %v", err)
	}
	harness.tick()
	harness.challenge(email, "admin-password")
}

func TestSeparateInstancesDoNotCollide(test *testing.T) {
	test.Parallel()
	ctx := context.Background()
	appPool, adminPool := newDatabase(test), newEmptyDatabase(test)
	if err := MigrateTable(ctx, adminPool, "goose_admin_auth"); err != nil {
		test.Fatal(err)
	}
	if err := MigrateTable(ctx, adminPool, "goose_admin_auth"); err != nil {
		test.Fatalf("second run: %v", err)
	}
	var exists bool
	_ = adminPool.QueryRow(ctx, `SELECT to_regclass('goose_admin_auth') IS NOT NULL AND to_regclass('goose_auth') IS NULL`).Scan(&exists)
	if !exists {
		test.Fatal("admin migrations did not use their own version table")
	}

	fixture := newHarness(test, nil)
	app, err := New(Config{Secret: testSecret, BaseURL: testBase, RateLimiter: allowAll{}}, appPool, fixture.mail)
	if err != nil {
		test.Fatal(err)
	}
	admins, err := New(Config{Secret: strings.Repeat("a", 40), BaseURL: "https://admin.test", CookiePrefix: "admin", RateLimiter: allowAll{}}, adminPool, fixture.mail)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := New(Config{Secret: testSecret, BaseURL: testBase, CookiePrefix: "bad;name"}, appPool, fixture.mail); err == nil {
		test.Fatal("invalid cookie prefix accepted")
	}

	signUp := func(auth *Auth) *http.Cookie {
		h2 := &harness{test: test, auth: auth, handler: auth.Handler(), mail: fixture.mail, clock: fixture.clock}
		rec := h2.do("POST", BasePath+"/sign-up/email", map[string]any{"name": "x", "email": "same@example.com", "password": "password123"}, nil)
		wantStatus(test, rec, http.StatusOK)
		return sessionCookie(rec)
	}
	appCookie, adminCookie := signUp(app), signUp(admins)
	if appCookie.Name != "better-auth.session_token" || adminCookie.Name != "__Secure-admin.session_token" {
		test.Fatalf("cookie names %q / %q", appCookie.Name, adminCookie.Name)
	}
	// Each instance ignores the other's cookie, even under its own name.
	get := func(auth *Auth, cookie string) bool {
		h2 := &harness{test: test, auth: auth, handler: auth.Handler(), mail: fixture.mail, clock: fixture.clock}
		return h2.getSession(hdr{"Cookie": cookie}) != nil
	}
	if !get(app, appCookie.Name+"="+appCookie.Value) || !get(admins, adminCookie.Name+"="+adminCookie.Value) {
		test.Fatal("own cookie rejected")
	}
	if get(admins, adminCookie.Name+"="+appCookie.Value) || get(app, appCookie.Name+"="+adminCookie.Value) {
		test.Fatal("a cookie signed by the other instance was accepted")
	}

	// Nested (admin handler mounted inside the app's Middleware): each instance
	// still only sees its own session.
	nested := &harness{test: test, auth: admins, handler: app.Middleware(admins.Handler()), mail: fixture.mail, clock: fixture.clock}
	both := appCookie.Name + "=" + appCookie.Value
	if nested.getSession(hdr{"Cookie": both}) != nil {
		test.Fatal("admin instance accepted the app session through nested Middleware")
	}
	if nested.getSession(hdr{"Cookie": both + "; " + adminCookie.Name + "=" + adminCookie.Value}) == nil {
		test.Fatal("admin session lost under nested Middleware")
	}
	protected := app.Middleware(admins.RequireUser(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })))
	if rec := nested.serve(protected, "GET", "/admin", nil, hdr{"Cookie": both}); rec.Code != http.StatusUnauthorized {
		test.Fatalf("admin RequireUser with only an app session: %d", rec.Code)
	}
}
