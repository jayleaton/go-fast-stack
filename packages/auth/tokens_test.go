package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

// unitAuth is an Auth without a database, for pure functions.
func unitAuth() *Auth {
	auth := &Auth{
		cfg:        Config{Secret: testSecret, MinPasswordLength: 8, MaxPasswordLength: 128, SecretGrace: DefaultSessionTTL},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        time.Now,
		baseOrigin: testBase,
		trusted:    append([]string{testBase}, MobileTrustedOrigins(false)...),
	}
	auth.ring.Store(&keyring{current: signingKey{secret: []byte(testSecret), kid: kidOf([]byte(testSecret))}})
	auth.data.Store(&dataKeys{totp: legacyDataKey([]byte(testSecret), purposeTOTP), backup: legacyDataKey([]byte(testSecret), purposeBackup)})
	return auth
}

// unitAuthWith is unitAuth signing with secret.
func unitAuthWith(secret string) *Auth {
	auth := unitAuth()
	auth.ring.Store(&keyring{current: signingKey{secret: []byte(secret), kid: kidOf([]byte(secret))}})
	auth.data.Store(&dataKeys{totp: legacyDataKey([]byte(secret), purposeTOTP), backup: legacyDataKey([]byte(secret), purposeBackup)})
	return auth
}

func TestGenerateID(test *testing.T) {
	test.Parallel()
	seen := map[string]bool{}
	for range 1000 {
		id := generateID(32)
		if len(id) != 32 || !isTokenString(id) || seen[id] {
			test.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
	if got := generateID(24); len(got) != 24 {
		test.Fatalf("len = %d", len(got))
	}
}

func TestSignedValueFormat(test *testing.T) {
	test.Parallel()
	auth := unitAuth()
	signed := auth.signValue("abc")
	value, sig, _ := strings.Cut(signed, ".")
	if value != "abc" || len(sig) != 44 || !strings.HasSuffix(sig, "=") {
		test.Fatalf("signed = %q", signed)
	}
	// Known answer from Node: encodeURIComponent(v + "." + HMAC-SHA256 base64),
	// which is what better-call puts in the cookie.
	cookie := encodeCookieValue(signed)
	if want := "abc.QCXFSY%2FZZJ78xtzA78HXwLV3XtDD3%2BreXEtIvoOEcvA%3D"; cookie != want {
		test.Fatalf("cookie = %q, want %q", cookie, want)
	}
	if got, ok := auth.verifySigned(cookie); !ok || got != "abc" {
		test.Fatalf("verify(encoded) = %q %v", got, ok)
	}
}

func TestVerifySigned(test *testing.T) {
	test.Parallel()
	auth := unitAuth()
	good := auth.signValue("tok123")
	other := unitAuthWith(strings.Repeat("o", 40)).signValue("tok123")
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{good, "tok123", true},
		{url.QueryEscape(good), "tok123", true},
		{other, "", false},
		{"tok123", "", false},
		{".sig", "", false},
		{"tok123.", "", false},
		{"tok124" + good[6:], "", false},
		{good[:len(good)-1], "", false},
		{"tok123.%zz", "", false},
		{"a.b.c" + good[6:], "", false},
		{"tok123." + strings.Repeat("A", 43) + "=", "", false},
		{"tok123." + strings.Repeat("!", 43) + "=", "", false},
	}
	for _, testCase := range cases {
		got, ok := auth.verifySigned(testCase.in)
		if got != testCase.want || ok != testCase.ok {
			test.Errorf("verifySigned(%q) = %q %v, want %q %v", testCase.in, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestParseBearer(test *testing.T) {
	test.Parallel()
	auth := unitAuth()
	tok := generateID(32)
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer " + tok, tok, true},
		{"BEARER " + tok, tok, true},
		{"Bearer " + auth.signValue(tok), tok, true},
		{"Bearer " + url.QueryEscape(auth.signValue(tok)), tok, true},
		{"Bearer  " + tok + "  ", tok, true},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic " + tok, "", false},
		{"Bearer" + tok, "", false},
		{"Bearer " + tok + ".forged", "", false},
		{"Bearer " + tok + "!", "", false},
		{"Bearer " + strings.Repeat("a", 300), "", false},
		{"Bearer tok' OR 1=1 --", "", false},
	}
	for _, testCase := range cases {
		got, ok, _ := auth.parseBearer(testCase.header)
		if got != testCase.want || ok != testCase.ok {
			test.Errorf("parseBearer(%q) = %q %v, want %q %v", testCase.header, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestVerificationToken(test *testing.T) {
	test.Parallel()
	auth := unitAuth()
	now := time.Unix(1_800_000_000, 0)
	auth.now = func() time.Time { return now }
	tok := auth.signVerificationToken("User@Example.com", "fp", 24*time.Hour)
	if claims, err := auth.parseVerificationToken(tok); err != nil || claims.Email != "user@example.com" || claims.Cred != "fp" || claims.UpdateTo != "" {
		test.Fatalf("parse = %+v %v", claims, err)
	}
	change := auth.signChangeEmailToken("uid1", "Old@Example.com", "New@Example.com", "nonce1", time.Hour)
	if claims, err := auth.parseVerificationToken(change); err != nil || claims.Email != "old@example.com" || claims.UpdateTo != "new@example.com" || claims.RequestType != changeEmailRequest || claims.UserID != "uid1" || claims.Nonce != "nonce1" {
		test.Fatalf("parse change-email = %+v %v", claims, err)
	}

	enc := base64.RawURLEncoding.EncodeToString
	sign := func(header, claims string) string {
		unsigned := enc([]byte(header)) + "." + enc([]byte(claims))
		return unsigned + "." + enc(auth.mac(unsigned))
	}
	parts := strings.Split(tok, ".")
	cases := map[string]string{
		"two parts":       parts[0] + "." + parts[1],
		"bad signature":   parts[0] + "." + parts[1] + "." + enc([]byte("nope")),
		"bad sig base64":  parts[0] + "." + parts[1] + ".***",
		"alg none":        sign(`{"alg":"none"}`, `{"email":"a@b.co","exp":1900000000}`),
		"alg HS512":       sign(`{"alg":"HS512"}`, `{"email":"a@b.co","exp":1900000000}`),
		"bad header json": sign(`{`, `{"email":"a@b.co","exp":1900000000}`),
		"bad claims json": sign(`{"alg":"HS256"}`, `[`),
		"no email":        sign(`{"alg":"HS256"}`, `{"exp":1900000000}`),
		"no exp":          sign(`{"alg":"HS256"}`, `{"email":"a@b.co"}`),
		"change email":    sign(`{"alg":"HS256"}`, `{"email":"a@b.co","updateTo":"c@d.co","exp":1900000000}`),
		"confirmation":    sign(`{"alg":"HS256"}`, `{"email":"a@b.co","updateTo":"c@d.co","requestType":"change-email-confirmation","exp":1900000000}`),
		"type no target":  sign(`{"alg":"HS256"}`, `{"email":"a@b.co","requestType":"change-email-verification","exp":1900000000}`),
		"header base64":   "***." + parts[1] + "." + parts[2],
		"claims base64":   parts[0] + ".***." + parts[2],
		"too long":        strings.Repeat("a", 2049) + ".b.c",
		"empty":           "",
	}
	for name, in := range cases {
		if _, err := auth.parseVerificationToken(in); !errors.Is(err, ErrInvalidToken) {
			test.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}

	now = now.Add(24 * time.Hour)
	if _, err := auth.parseVerificationToken(tok); !errors.Is(err, errTokenExpired) {
		test.Fatalf("expired err = %v", err)
	}
}

func FuzzVerifySigned(fuzz *testing.F) {
	auth := unitAuth()
	fuzz.Add(auth.signValue("abc"))
	fuzz.Add(url.QueryEscape(auth.signValue(generateID(32))))
	fuzz.Add("x.y")
	fuzz.Add("%")
	fuzz.Fuzz(func(test *testing.T, in string) {
		value, ok := auth.verifySigned(in)
		if ok && auth.signValue(value) != mustUnescape(in) {
			test.Fatalf("accepted %q as %q without a matching signature", in, value)
		}
		// Round trip: whatever we sign verifies.
		if strings.Contains(in, "%") || in == "" {
			return // verifySigned URL-decodes, so only plain values round-trip
		}
		if got, ok := auth.verifySigned(auth.signValue(in)); !ok || got != in {
			test.Fatalf("round trip of %q = %q %v", in, got, ok)
		}
	})
}

func mustUnescape(text string) string {
	if strings.Contains(text, "%") {
		if unescaped, err := url.PathUnescape(text); err == nil {
			return unescaped
		}
	}
	return text
}

func FuzzParseBearer(fuzz *testing.F) {
	auth := unitAuth()
	fuzz.Add("Bearer " + generateID(32))
	fuzz.Add("Bearer " + auth.signValue(generateID(32)))
	fuzz.Add("bearer %41.%")
	fuzz.Fuzz(func(test *testing.T, header string) {
		token, ok, _ := auth.parseBearer(header)
		if ok && !isTokenString(token) && !strings.Contains(header, ".") {
			test.Fatalf("parseBearer(%q) returned non-token %q", header, token)
		}
	})
}

func FuzzParseVerificationToken(fuzz *testing.F) {
	auth := unitAuth()
	fuzz.Add(auth.signVerificationToken("a@example.com", "", time.Hour))
	fuzz.Add("a.b.c")
	fuzz.Add("")
	fuzz.Fuzz(func(test *testing.T, in string) {
		claims, err := auth.parseVerificationToken(in)
		if err == nil && (claims.Email == "" || claims.Email != strings.ToLower(claims.Email) || (claims.UpdateTo != "") != (claims.RequestType == changeEmailRequest)) {
			test.Fatalf("parse(%q) = %+v", in, claims)
		}
	})
}

func TestMatchesOrigin(test *testing.T) {
	test.Parallel()
	cases := []struct {
		url, pattern string
		relative     bool
		want         bool
	}{
		{"http://app.test", "http://app.test", false, true},
		{"http://app.test/path?q=1", "http://app.test", false, true},
		{"http://APP.TEST:80/", "http://app.test", false, true},
		{"https://app.test", "http://app.test", false, false},
		{"http://app.test:3000", "http://app.test", false, false},
		{"http://app.test.evil", "http://app.test", false, false},
		{"http://user@evil.test", "http://app.test", false, false},
		{"https://a.example.com", "https://*.example.com", false, true},
		{"https://a.b.example.com", "https://*.example.com", false, true},
		{"https://example.com.evil.test", "https://*.example.com", false, false},
		{"https://a.example.com", "*.example.com", false, true},
		{"not a url", "*.example.com", false, false},
		{"javascript://x.example.com/%0aalert(1)", "*.example.com", true, false},
		{"exp://192.168.1.2:8081", "exp://**", false, true},
		{"exp://192.168.1.2:8081/--/path", "exp://**", false, true},
		{"exps://x", "exp://**", false, false},
		{"betterstack://", "betterstack://", false, true},
		{"betterstack://home/x", "betterstack://", false, true},
		{"BetterStack://Home", "betterstack://home", false, true},
		{"betterstack://other", "betterstack://home", false, false},
		{"myapp://host/cb/x", "myapp://host/cb", false, true},
		{"myapp://host/cb/../evil", "myapp://host/cb", false, false},
		{"myapp://host/cb%2F..%2Fevil", "myapp://host/cb", false, false},
		{"myapp://host/cbx", "myapp://host/cb", false, false},
		{"myapp://host\x00", "myapp://host", false, false},
		{"/dashboard", "http://app.test", true, true},
		{"/dashboard", "http://app.test", false, false},
		{"//evil.test", "http://app.test", true, false},
		{"/\\evil.test", "http://app.test", true, false},
		{"/%2F%2Fevil.test", "http://app.test", true, false},
		{"/%5cevil.test", "http://app.test", true, false},
		{"/ok?next=//evil", "http://app.test", true, true},
		{"/a\tb", "http://app.test", true, false},
		{"javascript:alert(1)", "http://app.test", true, false},
		{"", "http://app.test", true, false},
	}
	for _, testCase := range cases {
		if got := matchesOrigin(testCase.url, testCase.pattern, testCase.relative); got != testCase.want {
			test.Errorf("matchesOrigin(%q, %q, %v) = %v, want %v", testCase.url, testCase.pattern, testCase.relative, got, testCase.want)
		}
	}
}

func TestNormalizeIP(test *testing.T) {
	test.Parallel()
	cases := map[string]string{
		"203.0.113.9":               "203.0.113.9",
		"::ffff:203.0.113.9":        "203.0.113.9",
		"2001:db8::1":               "2001:db8::/64",
		"2001:DB8:0:0:ffff::2":      "2001:db8::/64",
		"2001:db8:0:1::1":           "2001:db8:0:1::/64",
		"fe80::1%eth0":              "fe80::/64",
		"not-an-ip":                 "",
		"203.0.113.9, 198.51.100.1": "",
	}
	for in, want := range cases {
		if got := normalizeIP(in); got != want {
			test.Errorf("normalizeIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMobileTrustedOrigins(test *testing.T) {
	test.Parallel()
	if got := MobileTrustedOrigins(true); len(got) != 1 || got[0] != "betterstack://" {
		test.Fatalf("production = %v", got)
	}
	if got := MobileTrustedOrigins(false); len(got) != 3 {
		test.Fatalf("development = %v", got)
	}
}

func TestPasswordHashing(test *testing.T) {
	test.Parallel()
	params := argonParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	h1, err := hashPassword("hunter2hunter2", params)
	if err != nil {
		test.Fatal(err)
	}
	h2, _ := hashPassword("hunter2hunter2", params)
	if h1 == h2 || !strings.HasPrefix(h1, "$argon2id$v=19$m=64,t=1,p=1$") {
		test.Fatalf("hashes %q %q", h1, h2)
	}
	if ok, err := verifyPassword("hunter2hunter2", h1); !ok || err != nil {
		test.Fatalf("verify = %v %v", ok, err)
	}
	if ok, _ := verifyPassword("hunter2hunter3", h1); ok {
		test.Fatal("wrong password verified")
	}
	parts := strings.Split(h1, "$")
	for name, bad := range map[string]string{
		"empty":        "",
		"bcrypt":       "$2a$10$abcdefghijklmnopqrstuv",
		"argon2i":      strings.Replace(h1, "argon2id", "argon2i", 1),
		"version":      strings.Replace(h1, "v=19", "v=16", 1),
		"params":       strings.Replace(h1, "m=64,t=1,p=1", "m=x", 1),
		"zero time":    strings.Replace(h1, "t=1", "t=0", 1),
		"huge memory":  strings.Replace(h1, "m=64", "m=99999999", 1),
		"salt base64":  strings.Join([]string{"", parts[1], parts[2], parts[3], "***", parts[5]}, "$"),
		"key base64":   strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], "***"}, "$"),
		"missing part": strings.Join(parts[:5], "$"),
	} {
		if ok, err := verifyPassword("hunter2hunter2", bad); ok || err == nil {
			test.Errorf("%s: verify = %v %v, want malformed", name, ok, err)
		}
	}
}

func TestBetterAuthScryptHash(test *testing.T) {
	test.Parallel()
	if ok, err := verifyPassword("hunter2hunter2", betterAuthHash); !ok || err != nil {
		test.Fatalf("verify = %v %v", ok, err)
	}
	if ok, _ := verifyPassword("hunter2hunter3", betterAuthHash); ok {
		test.Fatal("wrong password verified")
	}
	for _, bad := range []string{":abcd", "salt:zz", "salt:abcd", "a:b:c"} {
		if ok, _ := verifyPassword("x", bad); ok {
			test.Errorf("%q verified", bad)
		}
	}
}

func TestPasswordHashDefaults(test *testing.T) {
	test.Parallel()
	if testing.Short() {
		test.Skip("slow with -race")
	}
	// New hashes with these outside test binaries (argonParamsFor).
	if got := argonParamsFor(false); got != defaultArgonParams {
		test.Fatalf("production params = %+v", got)
	}
	password, err := hashPassword("pw", defaultArgonParams)
	if err != nil || !strings.HasPrefix(password, "$argon2id$v=19$m=65536,t=3,p=4$") {
		test.Fatalf("hash = %q %v", password, err)
	}
	if ok, _ := verifyPassword("pw", password); !ok {
		test.Fatal("verify failed")
	}
}

func TestPasswordLengthCountsCharacters(test *testing.T) {
	test.Parallel()
	auth := unitAuth()
	if err := auth.checkPasswordLength("ééééééé"); !errors.Is(err, ErrPasswordTooShort) { // 7 chars, 14 bytes
		test.Fatalf("err = %v", err)
	}
	if err := auth.checkPasswordLength("éééééééé"); err != nil {
		test.Fatalf("err = %v", err)
	}
}

func TestMemoryRateLimiter(test *testing.T) {
	test.Parallel()
	limiter := NewMemoryRateLimiter()
	now := time.Unix(0, 0)
	limiter.now = func() time.Time { return now }
	rule := RateRule{Window: 10 * time.Second, Max: 3}
	ctx := context.Background()
	for index := range 3 {
		if retry, _ := limiter.Allow(ctx, "k", rule); retry != 0 {
			test.Fatalf("request %d limited", index)
		}
	}
	now = now.Add(4 * time.Second)
	if retry, _ := limiter.Allow(ctx, "k", rule); retry != 6*time.Second {
		test.Fatalf("retry = %v, want 6s", retry)
	}
	if retry, _ := limiter.Allow(ctx, "other", rule); retry != 0 {
		test.Fatal("keys share a budget")
	}
	now = now.Add(6 * time.Second)
	if retry, _ := limiter.Allow(ctx, "k", rule); retry != 0 {
		test.Fatal("window did not reset")
	}
	// Stale windows are swept.
	now = now.Add(2 * time.Minute)
	_, _ = limiter.Allow(ctx, "fresh", rule)
	if _, ok := limiter.windows["other"]; ok {
		test.Fatal("stale window kept")
	}
}

type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, string, RateRule) (time.Duration, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestRateLimiterFailureFailsOpen(test *testing.T) {
	test.Parallel()
	auth := unitAuth()
	auth.limiter = failingLimiter{}
	ctx := context.WithValue(context.Background(), ctxKey{}, &requestMeta{ip: "1.2.3.4"})
	if err := auth.rateLimit(ctx, "sign-in", RuleSignIn); err != nil {
		test.Fatalf("err = %v", err)
	}
}

func TestErrorMapping(test *testing.T) {
	test.Parallel()
	if asError(&RateLimitError{RetryAfter: time.Second}) != ErrTooManyRequests {
		test.Fatal("rate limit error not mapped")
	}
	if asError(io.EOF) != nil {
		test.Fatal("foreign error mapped")
	}
	if ErrInvalidToken.Error() != "Invalid token" {
		test.Fatal("message")
	}
}
