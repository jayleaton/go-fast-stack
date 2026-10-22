package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/packages/secrets"
)

// redactLog logs through a redacting text and JSON handler and returns both
// outputs.
func redactLog(t *testing.T, opts secrets.RedactOptions, do func(*slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		do(slog.New(secrets.NewRedactingHandler(h, opts)))
	}
	return buf.String()
}

type valuer struct{ token string }

func (v valuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", v.token), slog.String("link", "https://a.example/reset-password/"+v.token))
}

type stringer struct{ s string }

func (s *stringer) String() string { return s.s }

func TestRedactingHandlerKeys(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"password", "new_password", "Passwd", "passphrase", "secret", "AUTH_SECRET", "client-secret",
		"token", "session_token", "accessToken", "refresh.token", "csrfToken", "api_key", "apiKey", "X-API-Key", "APIKey", "apikey",
		"Authorization", "cookie", "Set-Cookie", "dsn", "DATABASE_URL", "admin_database_url", "dbURL",
		"otp", "totpCode", "backup_codes", "recoveryCode", "private_key", "privateKey", "AWS_ACCESS_KEY", "credentials", "signature",
		"password_hash", "newPassword", "PGPASSWORD", "smtp_pass", "authToken", "refresh_token", "csrftoken", "jwt", "client_secret",
		"x-api-key", "aws_secret_access_key", "private_key_pem", "code_verifier", "auth", "auth_header", "sig", "connect.sid", "JSESSIONID",
	} {
		out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) { l.Info("m", key, "s3cr3t-value") })
		if strings.Contains(out, "s3cr3t-value") || strings.Count(out, "[redacted]") != 2 {
			t.Errorf("key %q not redacted: %s", key, out)
		}
	}
	// Harmless keys stay readable.
	for _, key := range []string{"status", "code", "keys", "secrets_provider", "key", "session", "url", "email", "provider", "user_id", "footprint"} {
		out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) { l.Info("m", key, "plainvalue") })
		if strings.Count(out, "plainvalue") != 2 {
			t.Errorf("key %q over-redacted: %s", key, out)
		}
	}
	// Fingerprints, empty values and already-redacted Secrets stay as they are.
	fp := secrets.NewSecret("re_live").Fingerprint()
	out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.Info("m", "resend_api_key", fp, "doppler_token", "", "auth_secret", secrets.NewSecret("hunter22"), "none_token", secrets.Secret{}.Fingerprint())
	})
	if strings.Count(out, fp) != 2 || strings.Contains(out, "hunter22") || !strings.Contains(out, "none_token=none") {
		t.Errorf("fingerprints: %s", out)
	}
	// A non-string value under a credential key is redacted too; a boolean
	// cannot carry one.
	out = redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) { l.Info("m", "otp", 123456, "has_password", true) })
	if strings.Contains(out, "123456") || strings.Count(out, "true") != 2 {
		t.Errorf("non-string values: %s", out)
	}
}

// Names that mention a credential without holding one stay readable, as an
// attribute key, as name=value in a message, and inside error text.
func TestRedactingHandlerFalsePositives(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		// Facts about a credential: a word after it marks metadata.
		"set_password_page", "password_page", "reset_password_path", "token_ttl", "secret_name",
		"password_min_length", "token_expires_at", "api_key_fingerprint", "cookie_name", "token_type",
		"session_id", "passwordChangedAt",
		// Words that merely contain a credential word.
		"status_code", "exit_code", "keys", "passed", "session_count", "author", "keyboard", "bypass",
		"passage", "max_tokens", "cache_key", "public_key",
	} {
		out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
			l.Info("m "+name+"=plainvalue", name, "plainvalue", "err", errors.New("failed: "+name+"=plainvalue"))
		})
		if strings.Contains(out, "[redacted]") || strings.Count(out, "plainvalue") != 6 {
			t.Errorf("%q over-redacted: %s", name, out)
		}
	}
	// The admin invite flow's exact record.
	out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.Info("invite sent", "email", "ada@example.com", "set_password_page", "https://admin.example/set-password")
	})
	if strings.Count(out, "https://admin.example/set-password") != 2 {
		t.Errorf("set_password_page: %s", out)
	}
}

func TestRedactString(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ in, want string }{
		{"Authorization: Bearer abc.def-ghi", "Authorization: [redacted]"},
		{"sent Bearer eyJhbGciOi.x_y== to upstream", "sent Bearer [redacted] to upstream"},
		{"cookie: better-auth.session_token=abc; theme=dark", "cookie: [redacted]"},
		{"Set-Cookie: s=1; HttpOnly\nnext line", "Set-Cookie: [redacted]\nnext line"},
		{"X-API-Key: re_123", "X-API-Key: [redacted]"},
		{"postgres://app:p4ss@db.internal:5432/app?sslmode=disable", "postgres://app:[redacted]@db.internal:5432/app?sslmode=disable"},
		{"dial redis://:hunter2@cache:6379 failed", "dial redis://:[redacted]@cache:6379 failed"},
		{"http://host:8080/path", "http://host:8080/path"},
		{"https://a.example/api/auth/reset-password/tok123?callbackURL=%2Freset", "https://a.example/api/auth/reset-password/[redacted]?callbackURL=%2Freset"},
		{"/auth/set-password/abc", "/auth/set-password/[redacted]"},
		{"https://a.example/api/auth/verify-email?token=abc.def&callbackURL=%2F", "https://a.example/api/auth/verify-email?token=[redacted]&callbackURL=%2F"},
		{"/cb?code=xyz&state=1", "/cb?code=[redacted]&state=1"},
		{"/x?api_key=k1&X-Amz-Signature=s1&sig=s2&page=2", "/x?api_key=[redacted]&X-Amz-Signature=[redacted]&sig=[redacted]&page=2"},
		{"token=abc", "token=[redacted]"},
		{"reset with password=hunter2, done", "reset with password=[redacted], done"},
		{"better-auth.session_token=abc.sig; Path=/", "better-auth.session_token=[redacted]; Path=/"},
		{"/cb?code=abc&state=x", "/cb?code=[redacted]&state=x"},
		{"/maps?key=k1&zoom=3", "/maps?key=[redacted]&zoom=3"},
		{"/f?token=a=b==&n=1", "/f?token=[redacted]&n=1"},
		{"sessionid=abc; sid=def; connect.sid=ghi; session=jkl", "sessionid=[redacted]; sid=[redacted]; connect.sid=[redacted]; session=[redacted]"},
		{"x-api-key=k2 newPassword=p1 password_hash=h1 client_secret=c1 refresh_token=r1", "x-api-key=[redacted] newPassword=[redacted] password_hash=[redacted] client_secret=[redacted] refresh_token=[redacted]"},
		{`bad query "token=abc"`, `bad query "token=[redacted]"`},
		// A readable parameter's value is still scanned.
		{"next=/x?token=abc password_page=/y?code=xyz", "next=/x?token=[redacted] password_page=/y?code=[redacted]"},
		{"token=", "token="},
		{"mytoken=abc", "mytoken=[redacted]"},
		{"a:token=abc", "a:token=abc"}, // not a parameter boundary
		// JSON fields by the same word rule; empty values and fingerprints stay.
		{`{"id":"u1","password_hash":"h2","password_min_length":"8","newPassword":"p\"2","token":"","api_key":"fp:0123abcd"}`,
			`{"id":"u1","password_hash":"[redacted]","password_min_length":"8","newPassword":"[redacted]","token":"","api_key":"fp:0123abcd"}`},
		{`{"code":"E42","status_code":"500"}`, `{"code":"E42","status_code":"500"}`},
		{"status 200 ok", "status 200 ok"},
		{"page=2&sort=name", "page=2&sort=name"},
		{"status_code=500 exit_code=1 keys=3 passed=true session_count=2 author=ada keyboard=us bypass=no passage=x",
			"status_code=500 exit_code=1 keys=3 passed=true session_count=2 author=ada keyboard=us bypass=no passage=x"},
		{"abc", "abc"},
	} {
		if got := secrets.RedactString(tt.in); got != tt.want {
			t.Errorf("RedactString(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactingHandlerValues(t *testing.T) {
	t.Parallel()

	link := "https://a.example/api/auth/reset-password/tok123?callbackURL=%2F"
	out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.Info("dial postgres://u:pw1@h/db", // the message too
			"err", fmt.Errorf("connect: %w", errors.New("postgres://u:pw2@h/db: refused")),
			"link", link,
			slog.Group("req", slog.String("authorization", "Bearer tok4"), slog.Group("inner", slog.String("next", "/x?token=tok5"))),
			"user", valuer{"tok6"},
			"u", &url.URL{Scheme: "https", Host: "h", User: url.UserPassword("u", "pw7")},
			"links", []string{"/verify-email?token=tok8"},
			"nilurl", (*stringer)(nil),
			"plain", &stringer{"fine"},
			"n", 42,
		)
	})
	for _, leak := range []string{"pw1", "pw2", "tok123", "tok4", "tok5", "tok6", "pw7", "tok8"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %s: %s", leak, out)
		}
	}
	for _, keep := range []string{"refused", "callbackURL", "fine", "42", `"inner":{"next":"/x?token=[redacted]"}`, "req.inner.next="} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q: %s", keep, out)
		}
	}
	// A credential-named group is redacted whole.
	out = redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.Info("m", slog.Group("cookie", slog.String("name", "sess9")))
	})
	if strings.Contains(out, "sess9") {
		t.Errorf("group: %s", out)
	}
}

func TestRedactingHandlerWithAttrsAndGroup(t *testing.T) {
	t.Parallel()

	out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.With("token", "tok1", "dsn", "postgres://u:pw2@h").WithGroup("g").Info("m", "access_token", "tok3", "ok", "visible")
	})
	for _, leak := range []string{"tok1", "pw2", "tok3"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %s: %s", leak, out)
		}
	}
	if !strings.Contains(out, "g.ok=visible") || !strings.Contains(out, `"g":{"access_token":"[redacted]","ok":"visible"}`) {
		t.Errorf("group lost: %s", out)
	}
	h := secrets.NewRedactingHandler(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}), secrets.RedactOptions{})
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled does not follow the wrapped handler")
	}
}

func TestRedactingHandlerDevMailLinks(t *testing.T) {
	t.Parallel()

	link := "http://dev-box:3000/api/auth/reset-password/tok123?callbackURL=%2Freset"
	mail := func(l *slog.Logger) {
		// The exact shape packages/auth DevMailer logs.
		l.Info("auth: password reset email (dev mode, not sent)", "to", "ada@example.com", "url", link, "token", "tok9")
	}
	for _, tt := range []struct {
		name     string
		opts     secrets.RedactOptions
		do       func(*slog.Logger)
		wantLink bool
	}{
		{"production: links redacted", secrets.RedactOptions{}, mail, false},
		{"development: dev mailer link kept", secrets.RedactOptions{KeepDevMailLinks: true}, mail, true},
		{"development: other messages still redacted", secrets.RedactOptions{KeepDevMailLinks: true}, func(l *slog.Logger) {
			l.Info("auth: request", "url", link)
		}, false},
		{"development: other keys of a dev mail still redacted", secrets.RedactOptions{KeepDevMailLinks: true}, func(l *slog.Logger) {
			l.Info("auth: verification email (dev mode, not sent)", "link", link)
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := redactLog(t, tt.opts, tt.do)
			if got := strings.Count(out, "tok123") == 2; got != tt.wantLink {
				t.Errorf("link readable = %v, want %v: %s", got, tt.wantLink, out)
			}
			if strings.Contains(out, "tok9") || (tt.wantLink && !strings.Contains(out, "ada@example.com")) {
				t.Errorf("other attrs: %s", out)
			}
		})
	}
}

// Maps log as groups and structs by their JSON, so a logged header set or
// session never carries its credentials.
func TestRedactingHandlerMapsAndStructs(t *testing.T) {
	t.Parallel()

	type session struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.Info("m",
			"headers", http.Header{"Authorization": {"Bearer s3cr3t-a"}, "Cookie": {"sid=s3cr3t-b"}, "Accept": {"text/html"}},
			"query", url.Values{"token": {"s3cr3t-c"}, "code": {"s3cr3t-g"}, "page": {"2"}},
			"labels", map[string]string{"api_key": "s3cr3t-d", "region": "eu"},
			"session", session{ID: "sess-1", Token: "s3cr3t-e"},
			"ptr", &session{ID: "sess-2", Token: "s3cr3t-f"},
			"plain", struct{ N int }{7},
			"key", map[string]string{"region": "us"}, // a bare name counts inside a map, not as its key
		)
	})
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("leaked: %s", out)
	}
	for _, want := range []string{"text/html", "page=2", "eu", "sess-1", "sess-2", "N:7", "region=us"} {
		if !strings.Contains(out, want) && !strings.Contains(out, strings.ReplaceAll(want, "=", `":"`)) {
			t.Errorf("lost %q: %s", want, out)
		}
	}
}
