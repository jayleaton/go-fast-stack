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
	return &Auth{
		cfg:        Config{Secret: testSecret, MinPasswordLength: 8, MaxPasswordLength: 128},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        time.Now,
		baseOrigin: testBase,
		trusted:    append([]string{testBase}, MobileTrustedOrigins(false)...),
	}
}

func TestGenerateID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := generateID(32)
		if len(id) != 32 || !isTokenString(id) || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
	if got := generateID(24); len(got) != 24 {
		t.Fatalf("len = %d", len(got))
	}
}

func TestSignedValueFormat(t *testing.T) {
	a := unitAuth()
	signed := a.signValue("abc")
	value, sig, _ := strings.Cut(signed, ".")
	if value != "abc" || len(sig) != 44 || !strings.HasSuffix(sig, "=") {
		t.Fatalf("signed = %q", signed)
	}
	// Known answer from Node: encodeURIComponent(v + "." + HMAC-SHA256 base64),
	// which is what better-call puts in the cookie.
	cookie := encodeCookieValue(signed)
	if want := "abc.QCXFSY%2FZZJ78xtzA78HXwLV3XtDD3%2BreXEtIvoOEcvA%3D"; cookie != want {
		t.Fatalf("cookie = %q, want %q", cookie, want)
	}
	if got, ok := a.verifySigned(cookie); !ok || got != "abc" {
		t.Fatalf("verify(encoded) = %q %v", got, ok)
	}
}

func TestVerifySigned(t *testing.T) {
	a := unitAuth()
	good := a.signValue("tok123")
	other := (&Auth{cfg: Config{Secret: strings.Repeat("o", 40)}}).signValue("tok123")
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
	for _, c := range cases {
		got, ok := a.verifySigned(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("verifySigned(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseBearer(t *testing.T) {
	a := unitAuth()
	tok := generateID(32)
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer " + tok, tok, true},
		{"BEARER " + tok, tok, true},
		{"Bearer " + a.signValue(tok), tok, true},
		{"Bearer " + url.QueryEscape(a.signValue(tok)), tok, true},
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
	for _, c := range cases {
		got, ok := a.parseBearer(c.header)
		if got != c.want || ok != c.ok {
			t.Errorf("parseBearer(%q) = %q %v, want %q %v", c.header, got, ok, c.want, c.ok)
		}
	}
}

func TestVerificationToken(t *testing.T) {
	a := unitAuth()
	now := time.Unix(1_800_000_000, 0)
	a.now = func() time.Time { return now }
	tok := a.signVerificationToken("User@Example.com", "fp", 24*time.Hour)
	if got, cred, err := a.parseVerificationToken(tok); err != nil || got != "user@example.com" || cred != "fp" {
		t.Fatalf("parse = %q %q %v", got, cred, err)
	}

	enc := base64.RawURLEncoding.EncodeToString
	sign := func(header, claims string) string {
		unsigned := enc([]byte(header)) + "." + enc([]byte(claims))
		return unsigned + "." + enc(a.mac(unsigned))
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
		"header base64":   "***." + parts[1] + "." + parts[2],
		"claims base64":   parts[0] + ".***." + parts[2],
		"too long":        strings.Repeat("a", 2049) + ".b.c",
		"empty":           "",
	}
	for name, in := range cases {
		if _, _, err := a.parseVerificationToken(in); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}

	now = now.Add(24 * time.Hour)
	if _, _, err := a.parseVerificationToken(tok); !errors.Is(err, errTokenExpired) {
		t.Fatalf("expired err = %v", err)
	}
}

func FuzzVerifySigned(f *testing.F) {
	a := unitAuth()
	f.Add(a.signValue("abc"))
	f.Add(url.QueryEscape(a.signValue(generateID(32))))
	f.Add("x.y")
	f.Add("%")
	f.Fuzz(func(t *testing.T, in string) {
		value, ok := a.verifySigned(in)
		if ok && a.signValue(value) != mustUnescape(in) {
			t.Fatalf("accepted %q as %q without a matching signature", in, value)
		}
		// Round trip: whatever we sign verifies.
		if strings.Contains(in, "%") || in == "" {
			return // verifySigned URL-decodes, so only plain values round-trip
		}
		if got, ok := a.verifySigned(a.signValue(in)); !ok || got != in {
			t.Fatalf("round trip of %q = %q %v", in, got, ok)
		}
	})
}

func mustUnescape(s string) string {
	if strings.Contains(s, "%") {
		if u, err := url.PathUnescape(s); err == nil {
			return u
		}
	}
	return s
}

func FuzzParseBearer(f *testing.F) {
	a := unitAuth()
	f.Add("Bearer " + generateID(32))
	f.Add("Bearer " + a.signValue(generateID(32)))
	f.Add("bearer %41.%")
	f.Fuzz(func(t *testing.T, header string) {
		token, ok := a.parseBearer(header)
		if ok && !isTokenString(token) && !strings.Contains(header, ".") {
			t.Fatalf("parseBearer(%q) returned non-token %q", header, token)
		}
	})
}

func FuzzParseVerificationToken(f *testing.F) {
	a := unitAuth()
	f.Add(a.signVerificationToken("a@example.com", "", time.Hour))
	f.Add("a.b.c")
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		email, _, err := a.parseVerificationToken(in)
		if err == nil && (email == "" || email != strings.ToLower(email)) {
			t.Fatalf("parse(%q) = %q", in, email)
		}
	})
}

func TestMatchesOrigin(t *testing.T) {
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
	for _, c := range cases {
		if got := matchesOrigin(c.url, c.pattern, c.relative); got != c.want {
			t.Errorf("matchesOrigin(%q, %q, %v) = %v, want %v", c.url, c.pattern, c.relative, got, c.want)
		}
	}
}

func TestNormalizeIP(t *testing.T) {
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
			t.Errorf("normalizeIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMobileTrustedOrigins(t *testing.T) {
	if got := MobileTrustedOrigins(true); len(got) != 1 || got[0] != "betterstack://" {
		t.Fatalf("production = %v", got)
	}
	if got := MobileTrustedOrigins(false); len(got) != 3 {
		t.Fatalf("development = %v", got)
	}
}

func TestPasswordHashing(t *testing.T) {
	p := argonParams{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	h1, err := hashPassword("hunter2hunter2", p)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := hashPassword("hunter2hunter2", p)
	if h1 == h2 || !strings.HasPrefix(h1, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hashes %q %q", h1, h2)
	}
	if ok, err := verifyPassword("hunter2hunter2", h1); !ok || err != nil {
		t.Fatalf("verify = %v %v", ok, err)
	}
	if ok, _ := verifyPassword("hunter2hunter3", h1); ok {
		t.Fatal("wrong password verified")
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
			t.Errorf("%s: verify = %v %v, want malformed", name, ok, err)
		}
	}
}

func TestBetterAuthScryptHash(t *testing.T) {
	if ok, err := verifyPassword("hunter2hunter2", betterAuthHash); !ok || err != nil {
		t.Fatalf("verify = %v %v", ok, err)
	}
	if ok, _ := verifyPassword("hunter2hunter3", betterAuthHash); ok {
		t.Fatal("wrong password verified")
	}
	for _, bad := range []string{":abcd", "salt:zz", "salt:abcd", "a:b:c"} {
		if ok, _ := verifyPassword("x", bad); ok {
			t.Errorf("%q verified", bad)
		}
	}
}

func TestPasswordHashDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("slow with -race")
	}
	p := argonParams{Memory: 64 * 1024, Time: 3, Threads: 4, SaltLen: 16, KeyLen: 32}
	h, err := hashPassword("pw", p)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash = %q %v", h, err)
	}
	if ok, _ := verifyPassword("pw", h); !ok {
		t.Fatal("verify failed")
	}
}

func TestPasswordLengthCountsCharacters(t *testing.T) {
	a := unitAuth()
	if err := a.checkPasswordLength("ééééééé"); !errors.Is(err, ErrPasswordTooShort) { // 7 chars, 14 bytes
		t.Fatalf("err = %v", err)
	}
	if err := a.checkPasswordLength("éééééééé"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryRateLimiter(t *testing.T) {
	m := NewMemoryRateLimiter()
	now := time.Unix(0, 0)
	m.now = func() time.Time { return now }
	rule := RateRule{Window: 10 * time.Second, Max: 3}
	ctx := context.Background()
	for i := range 3 {
		if retry, _ := m.Allow(ctx, "k", rule); retry != 0 {
			t.Fatalf("request %d limited", i)
		}
	}
	now = now.Add(4 * time.Second)
	if retry, _ := m.Allow(ctx, "k", rule); retry != 6*time.Second {
		t.Fatalf("retry = %v, want 6s", retry)
	}
	if retry, _ := m.Allow(ctx, "other", rule); retry != 0 {
		t.Fatal("keys share a budget")
	}
	now = now.Add(6 * time.Second)
	if retry, _ := m.Allow(ctx, "k", rule); retry != 0 {
		t.Fatal("window did not reset")
	}
	// Stale windows are swept.
	now = now.Add(2 * time.Minute)
	_, _ = m.Allow(ctx, "fresh", rule)
	if _, ok := m.windows["other"]; ok {
		t.Fatal("stale window kept")
	}
}

type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, string, RateRule) (time.Duration, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestRateLimiterFailureFailsOpen(t *testing.T) {
	a := unitAuth()
	a.limiter = failingLimiter{}
	ctx := context.WithValue(context.Background(), ctxKey{}, &requestMeta{ip: "1.2.3.4"})
	if err := a.rateLimit(ctx, "sign-in", RuleSignIn); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	if asError(&RateLimitError{RetryAfter: time.Second}) != ErrTooManyRequests {
		t.Fatal("rate limit error not mapped")
	}
	if asError(io.EOF) != nil {
		t.Fatal("foreign error mapped")
	}
	if ErrInvalidToken.Error() != "Invalid token" {
		t.Fatal("message")
	}
}
