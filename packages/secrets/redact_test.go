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

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
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
	for _, key := range []string{
		"password", "new_password", "Passwd", "passphrase", "secret", "AUTH_SECRET", "client-secret",
		"token", "session_token", "accessToken", "refresh.token", "csrfToken", "api_key", "apiKey", "X-API-Key", "APIKey", "apikey",
		"Authorization", "cookie", "Set-Cookie", "dsn", "DATABASE_URL", "admin_database_url", "dbURL",
		"otp", "totpCode", "backup_codes", "recoveryCode", "private_key", "privateKey", "AWS_ACCESS_KEY", "credentials", "signature",
	} {
		out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) { l.Info("m", key, "s3cr3t-value") })
		if strings.Contains(out, "s3cr3t-value") || strings.Count(out, "[redacted]") != 2 {
			t.Errorf("key %q not redacted: %s", key, out)
		}
	}
	// Harmless keys stay readable.
	for _, key := range []string{"status", "code", "keys", "secrets_provider", "key", "url", "email", "provider", "user_id", "footprint"} {
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
	// A non-string value under a credential key is redacted too.
	out = redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) { l.Info("m", "otp", 123456) })
	if strings.Contains(out, "123456") {
		t.Errorf("int otp: %s", out)
	}
}

func TestRedactString(t *testing.T) {
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
		{"status 200 ok", "status 200 ok"},
		{"page=2&sort=name", "page=2&sort=name"},
		{"abc", "abc"},
	} {
		if got := secrets.RedactString(tt.in); got != tt.want {
			t.Errorf("RedactString(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactingHandlerValues(t *testing.T) {
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
	type session struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	out := redactLog(t, secrets.RedactOptions{}, func(l *slog.Logger) {
		l.Info("m",
			"headers", http.Header{"Authorization": {"Bearer s3cr3t-a"}, "Cookie": {"sid=s3cr3t-b"}, "Accept": {"text/html"}},
			"query", url.Values{"token": {"s3cr3t-c"}, "page": {"2"}},
			"labels", map[string]string{"api_key": "s3cr3t-d", "region": "eu"},
			"session", session{ID: "sess-1", Token: "s3cr3t-e"},
			"ptr", &session{ID: "sess-2", Token: "s3cr3t-f"},
			"plain", struct{ N int }{7},
		)
	})
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("leaked: %s", out)
	}
	for _, want := range []string{"text/html", "page=2", "eu", "sess-1", "sess-2", "N:7"} {
		if !strings.Contains(out, want) && !strings.Contains(out, strings.ReplaceAll(want, "=", `":"`)) {
			t.Errorf("lost %q: %s", want, out)
		}
	}
}
