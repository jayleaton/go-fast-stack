package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is an OIDC provider on httptest: a JWKS with one RSA key and a
// token endpoint that checks the client, redirect URI and PKCE verifier,
// spends each code once and returns an ID token signed with that key.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string
	now    func() time.Time

	mu         sync.Mutex
	grants     map[string]fakeGrant
	lastSecret string // client_secret of the last token request
}

type fakeGrant struct {
	clientID, challenge, redirectURI string
	claims                           map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeIdP{t: t, key: key, grants: map[string]fakeGrant{}, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks", func(writer http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding
		writeJSON(writer, http.StatusOK, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64.EncodeToString(key.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", fake.token)
	fake.srv = httptest.NewServer(mux)
	t.Cleanup(fake.srv.Close)
	fake.issuer = fake.srv.URL
	return fake
}

func (fake *fakeIdP) endpoints() *oauthEndpoints {
	return &oauthEndpoints{authorize: fake.srv.URL + "/authorize", token: fake.srv.URL + "/token", jwks: fake.srv.URL + "/jwks", issuers: []string{fake.issuer}}
}

func (fake *fakeIdP) token(writer http.ResponseWriter, request *http.Request) {
	_ = request.ParseForm()
	fake.mu.Lock()
	grant, ok := fake.grants[request.PostForm.Get("code")]
	delete(fake.grants, request.PostForm.Get("code"))
	fake.lastSecret = request.PostForm.Get("client_secret")
	fake.mu.Unlock()
	switch {
	case !ok, grant.clientID != request.PostForm.Get("client_id"), grant.redirectURI != request.PostForm.Get("redirect_uri"),
		grant.challenge != "" && pkceChallenge(request.PostForm.Get("code_verifier")) != grant.challenge:
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"access_token": "at-" + generateID(8), "id_token": fake.sign(grant.claims), "expires_in": 3600, "scope": "openid email"})
}

func (fake *fakeIdP) sign(claims map[string]any) string {
	b64 := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, fake.key, crypto.SHA256, digest[:])
	if err != nil {
		fake.t.Fatal(err)
	}
	return unsigned + "." + b64.EncodeToString(sig)
}

// authorize plays the person consenting at the provider: it reads the
// authorization URL and returns the callback query the provider would send
// (code and state). claims override the ID token's (nonce, email...).
func (fake *fakeIdP) authorize(authURL string, claims map[string]any) url.Values {
	fake.t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, fake.srv.URL+"/authorize?") {
		fake.t.Fatalf("authorization URL %q", authURL)
	}
	query := parsed.Query()
	now := fake.now()
	idClaims := map[string]any{
		"iss": fake.issuer, "aud": query.Get("client_id"), "sub": "sub-" + generateID(10),
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": query.Get("nonce"),
		"email_verified": true, "name": "Ada Social",
	}
	for name, value := range claims {
		idClaims[name] = value
	}
	code := "code-" + generateID(16)
	fake.mu.Lock()
	fake.grants[code] = fakeGrant{clientID: query.Get("client_id"), challenge: query.Get("code_challenge"), redirectURI: query.Get("redirect_uri"), claims: idClaims}
	fake.mu.Unlock()
	return url.Values{"code": {code}, "state": {query.Get("state")}}
}

func p256PEM(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), key
}

// withSocial configures Google and Apple against idp.
func withSocial(t *testing.T, idp *fakeIdP, more ...func(*Config)) func(*Config) {
	pemKey, _ := p256PEM(t)
	return func(claims *Config) {
		claims.Google = &GoogleConfig{ClientID: "google-client", ClientSecret: "google-secret", endpoints: idp.endpoints()}
		claims.Apple = &AppleConfig{ClientID: "com.example.web", TeamID: "TEAM123", KeyID: "KEY123", PrivateKey: pemKey, endpoints: idp.endpoints()}
		claims.HTTPClient = idp.srv.Client()
		claims.SocialErrorPath = "/login"
		for _, mutate := range more {
			mutate(claims)
		}
	}
}

func newSocialHarness(t *testing.T, more ...func(*Config)) (*harness, *fakeIdP) {
	t.Helper()
	needDB(t)
	idp := newFakeIdP(t)
	fixture := newHarness(t, withSocial(t, idp, more...))
	idp.now = fixture.clock.Now
	return fixture, idp
}

// startBrowser starts the browser flow and returns the provider URL and the
// Cookie header carrying the state cookie.
func (fixture *harness) startBrowser(provider, callbackURL string) (string, string) {
	fixture.t.Helper()
	rec := httptest.NewRecorder()
	authURL, err := fixture.a.StartSocialSignIn(context.Background(), rec, provider, callbackURL)
	if err != nil {
		fixture.t.Fatal(err)
	}
	for _, claims := range rec.Result().Cookies() {
		if strings.HasSuffix(claims.Name, "."+oauthStateCookie) {
			return authURL, claims.Name + "=" + claims.Value
		}
	}
	fixture.t.Fatal("no state cookie")
	return "", ""
}

// callback delivers the provider's redirect and returns the response.
func (fixture *harness) callback(provider string, query url.Values, cookie string) *httptest.ResponseRecorder {
	fixture.t.Helper()
	headers := hdr{}
	if cookie != "" {
		headers["Cookie"] = cookie
	}
	return fixture.do("GET", BasePath+"/callback/"+provider+"?"+query.Encode(), nil, headers)
}

// landed returns the redirect target of rec and its query.
func landed(t *testing.T, rec *httptest.ResponseRecorder) (string, url.Values) {
	t.Helper()
	wantStatus(t, rec, http.StatusFound)
	loc := rec.Header().Get("Location")
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return loc, parsed.Query()
}

func TestSocialBrowserSignIn(t *testing.T) {
	t.Parallel()
	log := &eventLog{}
	fixture, idp := newSocialHarness(t, withEvents(log))
	email := uniqueEmail(t)

	if got := strings.Join(fixture.a.SocialProviders(), ","); got != "google,apple" {
		t.Fatalf("providers = %s", got)
	}
	rec := fixture.do("GET", BasePath+"/social-providers", nil, nil)
	if raw := jsonBody(t, rec); len(raw["providers"].([]any)) != 2 {
		t.Fatalf("social-providers = %v", raw)
	}

	authURL, cookie := fixture.startBrowser(ProviderGoogle, "/dashboard")
	parsed, _ := url.Parse(authURL)
	if got := parsed.Query().Get("redirect_uri"); got != testBase+"/api/auth/callback/google" {
		t.Fatalf("redirect_uri = %q, want the callback on BaseURL", got)
	}
	if parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("nonce") == "" {
		t.Fatalf("authorization URL lacks PKCE or nonce: %s", authURL)
	}
	sub := "sub-" + generateID(10)
	query := idp.authorize(authURL, map[string]any{"sub": sub, "email": strings.ToUpper(email), "picture": "https://example.com/a.png"})
	rec = fixture.callback(ProviderGoogle, query, cookie)
	if loc, _ := landed(t, rec); loc != "/dashboard" {
		t.Fatalf("landed on %q", loc)
	}
	if idp.lastSecret != "google-secret" {
		t.Fatalf("client secret sent = %q", idp.lastSecret)
	}
	session := fixture.getSession(hdr{"Cookie": cookieHeader(t, rec)})
	user := session["user"].(map[string]any)
	if user["email"] != email || user["emailVerified"] != true || user["name"] != "Ada Social" || user["image"] != "https://example.com/a.png" {
		t.Fatalf("user = %v", user)
	}
	ev := log.take(t, fixture.a)
	if kinds(ev) != "sign_in" || ev[0].Method != SignInMethodSocial || ev[0].Details["provider"] != "google" {
		t.Fatalf("events = %+v", ev)
	}

	// Signing in again finds the linked account: same user, no new one.
	authURL, cookie = fixture.startBrowser(ProviderGoogle, "")
	rec = fixture.callback(ProviderGoogle, idp.authorize(authURL, map[string]any{"email": email, "sub": sub}), cookie)
	if loc, _ := landed(t, rec); loc != "/" {
		t.Fatalf("default landing = %q", loc)
	}
	if again := fixture.getSession(hdr{"Cookie": cookieHeader(t, rec)}); again["user"].(map[string]any)["id"] != user["id"] {
		t.Fatal("second sign-in made another user")
	}

	// The state is spent: replaying the callback restarts the flow.
	rec = fixture.callback(ProviderGoogle, query, cookie)
	if _, got := landed(t, rec); got.Get("error") != "please_restart_the_process" || sessionCookie(rec) != nil {
		t.Fatalf("replayed callback: %v", rec.Header())
	}
}

// Every way a callback can fail lands on the error page with a code and no
// session, and creates no user.
func TestSocialCallbackRefusals(t *testing.T) {
	t.Parallel()
	fixture, idp := newSocialHarness(t)
	ctx := context.Background()

	_, otherCookie := fixture.startBrowser(ProviderGoogle, "/")
	forged := strings.Replace(otherCookie, "=", "=x", 1)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		mutate func(query url.Values, cookie string) (url.Values, string)
		want   string
	}{
		{name: "no state", mutate: func(query url.Values, claims string) (url.Values, string) { query.Del("state"); return query, claims }, want: "state_not_found"},
		{name: "unknown state", mutate: func(query url.Values, claims string) (url.Values, string) {
			query.Set("state", "nope")
			return query, claims
		}, want: "please_restart_the_process"},
		{name: "no state cookie", mutate: func(query url.Values, _ string) (url.Values, string) { return query, "" }, want: "state_mismatch"},
		{name: "another flow's cookie", mutate: func(query url.Values, _ string) (url.Values, string) { return query, otherCookie }, want: "state_mismatch"},
		{name: "forged cookie", mutate: func(query url.Values, _ string) (url.Values, string) { return query, forged }, want: "state_mismatch"},
		{name: "provider error", mutate: func(query url.Values, claims string) (url.Values, string) {
			query.Set("error", "access_denied")
			return query, claims
		}, want: "access_denied"},
		{name: "odd provider error", mutate: func(query url.Values, claims string) (url.Values, string) {
			query.Set("error", "<b>x</b>")
			return query, claims
		}, want: "oauth_error"},
		{name: "no code", mutate: func(query url.Values, claims string) (url.Values, string) { query.Del("code"); return query, claims }, want: "no_code"},
		{name: "bad code", mutate: func(query url.Values, claims string) (url.Values, string) {
			query.Set("code", "forged")
			return query, claims
		}, want: "invalid_code"},
		{name: "nonce mismatch", claims: map[string]any{"nonce": "attacker"}, want: "unable_to_get_user_info"},
		{name: "wrong audience", claims: map[string]any{"aud": "someone-else"}, want: "unable_to_get_user_info"},
		{name: "wrong issuer", claims: map[string]any{"iss": "https://evil.example"}, want: "unable_to_get_user_info"},
		{name: "expired ID token", claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, want: "unable_to_get_user_info"},
		{name: "no email", claims: map[string]any{"email": ""}, want: "email_not_found"},
	} {
		authURL, cookie := fixture.startBrowser(ProviderGoogle, "/dashboard")
		claims := map[string]any{"email": uniqueEmail(t)}
		for name, value := range tc.claims {
			claims[name] = value
		}
		query := idp.authorize(authURL, claims)
		if tc.mutate != nil {
			query, cookie = tc.mutate(query, cookie)
		}
		rec := fixture.callback(ProviderGoogle, query, cookie)
		loc, got := landed(t, rec)
		if !strings.HasPrefix(loc, testBase+"/login?") || got.Get("error") != tc.want || sessionCookie(rec) != nil {
			t.Errorf("%s: landed on %q (cookie %v), want error=%s", tc.name, loc, sessionCookie(rec), tc.want)
		}
		if email, _ := claims["email"].(string); email != "" {
			if _, err := fixture.a.store.userByEmail(ctx, email); err == nil {
				t.Errorf("%s: a user was created", tc.name)
			}
		}
	}

	// PKCE: a code issued for another verifier's challenge is refused by
	// the provider, so a stolen code is useless with this flow's state.
	authURL, cookie := fixture.startBrowser(ProviderGoogle, "/")
	query := idp.authorize(authURL, map[string]any{"email": uniqueEmail(t)})
	idp.mu.Lock()
	grant := idp.grants[query.Get("code")]
	grant.challenge = pkceChallenge("another-verifier")
	idp.grants[query.Get("code")] = grant
	idp.mu.Unlock()
	if _, got := landed(t, fixture.callback(ProviderGoogle, query, cookie)); got.Get("error") != "invalid_code" {
		t.Errorf("PKCE mismatch: error=%s", got.Get("error"))
	}

	// The provider in the path must be the one the flow started with.
	authURL, cookie = fixture.startBrowser(ProviderGoogle, "/")
	query = idp.authorize(authURL, map[string]any{"email": uniqueEmail(t)})
	if _, got := landed(t, fixture.callback(ProviderApple, query, cookie)); got.Get("error") != "state_mismatch" {
		t.Errorf("provider swap: error=%s", got.Get("error"))
	}
}

func TestSocialStartRejects(t *testing.T) {
	t.Parallel()
	fixture, _ := newSocialHarness(t)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	for _, cb := range []string{"https://evil.example/x", "//evil.example", "betterstack://home"} {
		if _, err := fixture.a.StartSocialSignIn(ctx, rec, ProviderGoogle, cb); err == nil {
			t.Errorf("browser flow accepted callback %q", cb)
		}
	}
	if _, err := fixture.a.StartSocialSignIn(ctx, rec, "github", "/"); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("unknown provider: %v", err)
	}
	for _, cb := range []string{"", "/dashboard", testBase + "/x", "evilapp://steal"} {
		rec := fixture.do("POST", BasePath+"/sign-in/social", map[string]any{"provider": "google", "callbackURL": cb}, nil)
		wantCode(t, rec, http.StatusForbidden, ErrInvalidCallbackURL.Code)
	}
	wantCode(t, fixture.do("POST", BasePath+"/sign-in/social", map[string]any{"provider": "github", "callbackURL": "betterstack://auth"}, nil), http.StatusNotFound, ErrProviderNotFound.Code)

	// Without providers configured there is nothing to show or start.
	plain := newHarness(t, nil)
	if got := plain.a.SocialProviders(); len(got) != 0 {
		t.Errorf("providers = %v", got)
	}
	if raw := jsonBody(t, plain.do("GET", BasePath+"/social-providers", nil, nil)); len(raw["providers"].([]any)) != 0 {
		t.Errorf("social-providers = %v", raw)
	}
}

// Linking by email needs both sides to have verified it; an address the
// provider has not verified gets no account when verification is required.
func TestSocialAccountLinking(t *testing.T) {
	t.Parallel()
	log := &eventLog{}
	fixture, idp := newSocialHarness(t, withEvents(log))
	ctx := context.Background()
	signIn := func(email string, verified bool) url.Values {
		authURL, cookie := fixture.startBrowser(ProviderGoogle, "/")
		_, query := landed(t, fixture.callback(ProviderGoogle, idp.authorize(authURL, map[string]any{"email": email, "email_verified": verified}), cookie))
		return query
	}

	// A local user who has not verified their address is not linked:
	// whoever controls the provider account might not own the inbox.
	unverified := uniqueEmail(t)
	wantStatus(t, fixture.signUp(unverified, "password123"), http.StatusOK)
	if query := signIn(unverified, true); query.Get("error") != "account_not_linked" {
		t.Errorf("unverified local user: %v", query)
	}

	verified := uniqueEmail(t)
	wantStatus(t, fixture.signUp(verified, "password123"), http.StatusOK)
	if _, err := fixture.a.store.pool.Exec(ctx, `UPDATE "user" SET "emailVerified" = true WHERE "email" = $1`, verified); err != nil {
		t.Fatal(err)
	}
	if query := signIn(verified, false); query.Get("error") != "account_not_linked" {
		t.Errorf("provider-unverified email linked: %v", query)
	}
	_ = log.take(t, fixture.a)
	if query := signIn(verified, true); query.Has("error") {
		t.Errorf("verified on both sides: %v", query)
	}
	if ev := log.take(t, fixture.a); kinds(ev) != "account_linked,sign_in" || ev[0].Details["provider"] != "google" {
		t.Errorf("events = %+v", ev)
	}
	var length int
	_ = fixture.a.store.pool.QueryRow(ctx, `SELECT count(*) FROM "account" a JOIN "user" u ON u."id" = a."userId" WHERE u."email" = $1`, verified).Scan(&length)
	if length != 2 {
		t.Errorf("accounts = %d, want credential + google", length)
	}

	// Development (no verification required): a new, unverified user.
	email := uniqueEmail(t)
	if query := signIn(email, false); query.Has("error") {
		t.Errorf("dev sign-up: %v", query)
	}
	if user, err := fixture.a.store.userByEmail(ctx, email); err != nil || user.EmailVerified {
		t.Errorf("dev user = %+v, %v", user, err)
	}

	// Production: no account and no session for an unproven address.
	prod, prodIdP := newSocialHarness(t, func(claims *Config) { claims.RequireEmailVerification = true })
	email = uniqueEmail(t)
	authURL, cookie := prod.startBrowser(ProviderGoogle, "/")
	_, query := landed(t, prod.callback(ProviderGoogle, prodIdP.authorize(authURL, map[string]any{"email": email, "email_verified": false}), cookie))
	if query.Get("error") != "email_not_verified" {
		t.Errorf("prod unverified: %v", query)
	}
	if _, err := prod.a.store.userByEmail(ctx, email); err == nil {
		t.Error("prod created an unverified user")
	}

	// Sign-up disabled, and 2FA never skipped.
	closed, closedIdP := newSocialHarness(t, func(claims *Config) { claims.DisableSignUp = true })
	authURL, cookie = closed.startBrowser(ProviderGoogle, "/")
	if _, query := landed(t, closed.callback(ProviderGoogle, closedIdP.authorize(authURL, map[string]any{"email": uniqueEmail(t)}), cookie)); query.Get("error") != "signup_disabled" {
		t.Errorf("sign-up disabled: %v", query)
	}
	tf, tfIdP := newSocialHarness(t, with2FA(true))
	authURL, cookie = tf.startBrowser(ProviderGoogle, "/")
	if _, query := landed(t, tf.callback(ProviderGoogle, tfIdP.authorize(authURL, map[string]any{"email": uniqueEmail(t)}), cookie)); query.Get("error") != "two_factor_required" {
		t.Errorf("2FA required: %v", query)
	}
}

// The app handoff: no session in any URL, a one-time code that only the
// holder of the flow's secret can spend, once, within a minute.
func TestSocialAppHandoff(t *testing.T) {
	t.Parallel()
	log := &eventLog{}
	fixture, idp := newSocialHarness(t, withEvents(log))
	email := uniqueEmail(t)
	start := func() (string, string) {
		rec := fixture.do("POST", BasePath+"/sign-in/social", map[string]any{"provider": "google", "callbackURL": "betterstack://auth/callback"}, nil)
		wantStatus(t, rec, http.StatusOK)
		raw := jsonBody(t, rec)
		if raw["redirect"] != false || raw["handoffSecret"] == "" || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("start = %v, cookies %v", raw, rec.Result().Cookies())
		}
		return raw["url"].(string), raw["handoffSecret"].(string)
	}
	finish := func(authURL string) (string, url.Values) {
		// The in-app browser has no state cookie: the secret binds the flow.
		rec := fixture.callback(ProviderGoogle, idp.authorize(authURL, map[string]any{"email": email}), "")
		if sessionCookie(rec) != nil {
			t.Fatal("handoff callback set a session cookie")
		}
		return landed(t, rec)
	}
	exchange := func(code, secret string) *httptest.ResponseRecorder {
		return fixture.do("POST", BasePath+"/sign-in/handoff", map[string]any{"code": code, "handoffSecret": secret}, nil)
	}

	authURL, secret := start()
	loc, query := finish(authURL)
	if !strings.HasPrefix(loc, "betterstack://auth/callback?code=") || strings.Contains(loc, "token") {
		t.Fatalf("deep link = %q", loc)
	}
	_ = log.take(t, fixture.a)
	rec := exchange(query.Get("code"), secret)
	wantStatus(t, rec, http.StatusOK)
	raw := jsonBody(t, rec)
	if raw["token"] == "" || raw["user"].(map[string]any)["email"] != email || rec.Header().Get("Set-Auth-Token") == "" {
		t.Fatalf("exchange = %v", raw)
	}
	if got := fixture.getSession(hdr{"Authorization": "Bearer " + rec.Header().Get("Set-Auth-Token")}); got == nil {
		t.Fatal("bearer session does not work")
	}
	if ev := log.take(t, fixture.a); kinds(ev) != "sign_in" || ev[0].Method != SignInMethodSocial {
		t.Fatalf("events = %+v", ev)
	}
	// Replayed code.
	wantCode(t, exchange(query.Get("code"), secret), http.StatusBadRequest, ErrInvalidToken.Code)

	// A wrong secret (an app that intercepted the deep link) spends the code.
	authURL, secret = start()
	_, query = finish(authURL)
	wantCode(t, exchange(query.Get("code"), "intercepted"), http.StatusBadRequest, ErrInvalidToken.Code)
	wantCode(t, exchange(query.Get("code"), secret), http.StatusBadRequest, ErrInvalidToken.Code)

	// Expired code.
	authURL, secret = start()
	_, query = finish(authURL)
	fixture.clock.Advance(handoffCodeTTL + time.Second)
	wantCode(t, exchange(query.Get("code"), secret), http.StatusBadRequest, ErrInvalidToken.Code)
	wantCode(t, exchange("", ""), http.StatusBadRequest, ErrInvalidToken.Code)

	// Failures go back to the app's deep link, not the web error page.
	authURL, _ = start()
	query = idp.authorize(authURL, nil)
	query.Set("error", "access_denied")
	if loc, _ := landed(t, fixture.callback(ProviderGoogle, query, "")); loc != "betterstack://auth/callback?error=access_denied" {
		t.Fatalf("app failure landed on %q", loc)
	}
}

// Apple: form_post to the callback is bounced to a GET without the
// front-channel id_token, the client secret is an ES256 JWT signed with the
// configured key, and the name Apple posts once becomes the user's name.
func TestSocialApple(t *testing.T) {
	t.Parallel()
	fixture, idp := newSocialHarness(t)
	email := uniqueEmail(t)
	authURL, cookie := fixture.startBrowser(ProviderApple, "/")
	parsed, _ := url.Parse(authURL)
	if parsed.Query().Get("response_mode") != "form_post" || parsed.Query().Has("code_challenge") {
		t.Fatalf("Apple authorization URL: %s", authURL)
	}
	query := idp.authorize(authURL, map[string]any{"email": email, "email_verified": "true", "name": ""})
	form := url.Values{"code": {query.Get("code")}, "state": {query.Get("state")}, "id_token": {"front-channel"}, "user": {`{"name":{"firstName":"Ada","lastName":"Apple"}}`}}
	req := httptest.NewRequestWithContext(context.Background(), "POST", BasePath+"/callback/apple", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	fixture.h.ServeHTTP(rec, req)
	loc, bounced := landed(t, rec)
	if !strings.HasPrefix(loc, testBase+"/api/auth/callback/apple?") || bounced.Has("id_token") {
		t.Fatalf("form_post bounce = %q", loc)
	}
	rec = fixture.callback(ProviderApple, bounced, cookie)
	if loc, _ := landed(t, rec); loc != "/" {
		t.Fatalf("landed on %q", loc)
	}
	user := fixture.getSession(hdr{"Cookie": cookieHeader(t, rec)})["user"].(map[string]any)
	if user["name"] != "Ada Apple" || user["emailVerified"] != true {
		t.Fatalf("user = %v", user)
	}
	parts := strings.Split(idp.lastSecret, ".")
	var header struct{ Alg, Kid string }
	if len(parts) != 3 || decodeSegment(parts[0], &header) != nil || header.Alg != "ES256" || header.Kid != "KEY123" {
		t.Fatalf("Apple client secret = %q", idp.lastSecret)
	}

	// Rotation: a key and key id that do not parse are refused and the old
	// ones stay; a good pair replaces them.
	if err := fixture.a.SetSocialSecrets(SocialSecrets{GoogleClientSecret: "g2", ApplePrivateKey: "not pem", AppleKeyID: "K2"}); err == nil {
		t.Error("bad Apple key accepted")
	}
	if err := fixture.a.SetSocialSecrets(SocialSecrets{ApplePrivateKey: "x", AppleKeyID: "K2"}); err == nil {
		t.Error("missing Google secret accepted")
	}
	pemKey, _ := p256PEM(t)
	if err := fixture.a.SetSocialSecrets(SocialSecrets{GoogleClientSecret: "g2", ApplePrivateKey: pemKey, AppleKeyID: "K2"}); err != nil {
		t.Fatal(err)
	}
	authURL, cookie = fixture.startBrowser(ProviderApple, "/")
	landed(t, fixture.callback(ProviderApple, idp.authorize(authURL, map[string]any{"email": email}), cookie))
	_ = decodeSegment(strings.Split(idp.lastSecret, ".")[0], &header)
	if header.Kid != "K2" {
		t.Fatalf("rotated key id = %q", header.Kid)
	}
}

func TestSocialConfigValidation(t *testing.T) {
	t.Parallel()
	needDB(t)
	for name, mutate := range map[string]func(*Config){
		"google without secret": func(claims *Config) { claims.Google = &GoogleConfig{ClientID: "x"} },
		"apple without key":     func(claims *Config) { claims.Apple = &AppleConfig{ClientID: "x", TeamID: "t", KeyID: "k"} },
		"apple bad key": func(claims *Config) {
			claims.Apple = &AppleConfig{ClientID: "x", TeamID: "t", KeyID: "k", PrivateKey: "nope"}
		},
		"error path absolute": func(claims *Config) { claims.SocialErrorPath = "https://evil.example/" },
	} {
		cfg := Config{Secret: testSecret, BaseURL: testBase}
		mutate(&cfg)
		if _, err := New(cfg, testPool, &captureMailer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
