package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type resendPayload struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
	Text    string   `json:"text"`
}

func TestResendMailerSends(t *testing.T) {
	const link = "https://app.example.test/api/auth/verify-email?token=abc&next=/dashboard"

	tests := []struct {
		name        string
		to          string
		send        func(m *ResendMailer, to string) error
		subject     string
		htmlContain []string
		textContain []string
		noLink      bool // the email carries no link
	}{
		{
			name:        "verification",
			to:          "user@example.test",
			send:        func(m *ResendMailer, to string) error { return m.SendVerificationEmail(context.Background(), to, link) },
			subject:     "Verify your email address",
			htmlContain: []string{"Verify Email Address", "user@example.test", "expire in 24 hours"},
			textContain: []string{"user@example.test", "expire in 24 hours"},
		},
		{
			name: "password reset",
			to:   "user@example.test",
			send: func(m *ResendMailer, to string) error {
				return m.SendPasswordResetEmail(context.Background(), to, link)
			},
			subject:     "Reset your password",
			htmlContain: []string{"Reset Password", "user@example.test", "expire in 1 hour", "safely ignore this email"},
			textContain: []string{"user@example.test", "expire in 1 hour", "safely ignore this email"},
		},
		{
			name: "email change notice",
			to:   "old@example.test",
			send: func(m *ResendMailer, to string) error {
				return m.SendEmailChangeNotice(context.Background(), to, "<i>new</i>@example.test")
			},
			subject:     "Your email address is being changed",
			htmlContain: []string{"old@example.test", "&lt;i&gt;new&lt;/i&gt;@example.test", "reset your password"},
			textContain: []string{"old@example.test", "<i>new</i>@example.test", "reset your password"},
			noLink:      true,
		},
		{
			name:        "email is html-escaped",
			to:          "<b>evil</b>@example.test",
			send:        func(m *ResendMailer, to string) error { return m.SendVerificationEmail(context.Background(), to, link) },
			subject:     "Verify your email address",
			htmlContain: []string{"&lt;b&gt;evil&lt;/b&gt;@example.test"},
			textContain: []string{"<b>evil</b>@example.test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got resendPayload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if r.URL.Path != "/emails" {
					t.Errorf("path = %s, want /emails", r.URL.Path)
				}
				if h := r.Header.Get("Authorization"); h != "Bearer re_test" {
					t.Errorf("Authorization = %q", h)
				}
				if h := r.Header.Get("Content-Type"); h != "application/json" {
					t.Errorf("Content-Type = %q", h)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode body: %v", err)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":"email_1"}`)
			}))
			defer srv.Close()

			m := NewResendMailer("re_test", "App <noreply@example.test>")
			m.Endpoint = srv.URL + "/emails"
			if err := tt.send(m, tt.to); err != nil {
				t.Fatalf("send: %v", err)
			}

			if got.From != "App <noreply@example.test>" {
				t.Errorf("from = %q", got.From)
			}
			if len(got.To) != 1 || got.To[0] != tt.to {
				t.Errorf("to = %v, want [%s]", got.To, tt.to)
			}
			if got.Subject != tt.subject {
				t.Errorf("subject = %q, want %q", got.Subject, tt.subject)
			}
			// html/template escapes & in attributes and text as &amp;.
			htmlWant, textWant := tt.htmlContain, tt.textContain
			if !tt.noLink {
				htmlWant = append([]string{strings.ReplaceAll(link, "&", "&amp;")}, htmlWant...)
				textWant = append([]string{link}, textWant...)
			}
			for _, s := range htmlWant {
				if !strings.Contains(got.HTML, s) {
					t.Errorf("html missing %q", s)
				}
			}
			if strings.Contains(tt.to, "<") && strings.Contains(got.HTML, tt.to) {
				t.Errorf("html contains unescaped email %q", tt.to)
			}
			for _, s := range textWant {
				if !strings.Contains(got.Text, s) {
					t.Errorf("text missing %q", s)
				}
			}
		})
	}
}

func TestResendMailerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"invalid from address"}`)
	}))
	defer srv.Close()

	tests := []struct {
		name    string
		mailer  *ResendMailer
		wantErr string
	}{
		{"non-2xx", &ResendMailer{APIKey: "re_test", From: "a@example.test", Endpoint: srv.URL}, "invalid from address"},
		{"non-2xx status", &ResendMailer{APIKey: "re_test", From: "a@example.test", Endpoint: srv.URL}, "422"},
		{"empty api key", &ResendMailer{From: "a@example.test", Endpoint: srv.URL}, "API key"},
		{"empty from", &ResendMailer{APIKey: "re_test", Endpoint: srv.URL}, "from address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mailer.SendPasswordResetEmail(context.Background(), "user@example.test", "https://app.example.test/reset")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDevMailerLogsLink(t *testing.T) {
	tests := []struct {
		name string
		send func(m *DevMailer) error
		msg  string
	}{
		{"verification", func(m *DevMailer) error {
			return m.SendVerificationEmail(context.Background(), "user@example.test", "https://app.example.test/verify?token=t1")
		}, "verification email"},
		{"password reset", func(m *DevMailer) error {
			return m.SendPasswordResetEmail(context.Background(), "user@example.test", "https://app.example.test/verify?token=t1")
		}, "password reset email"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			m := NewDevMailer(slog.New(slog.NewTextHandler(&buf, nil)))
			if err := tt.send(m); err != nil {
				t.Fatalf("send: %v", err)
			}
			out := buf.String()
			for _, s := range []string{tt.msg, "to=user@example.test", "https://app.example.test/verify?token=t1"} {
				if !strings.Contains(out, s) {
					t.Errorf("log %q missing %q", out, s)
				}
			}
		})
	}
}

func TestNewDevMailerNilLogger(t *testing.T) {
	if m := NewDevMailer(nil); m.Logger == nil {
		t.Fatal("expected default logger")
	}
}

func TestSwapMailerUsesTheLatestMailer(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	resend := func(key string) *ResendMailer {
		return &ResendMailer{APIKey: key, From: "a@example.test", Endpoint: srv.URL, HTTPClient: srv.Client()}
	}

	var logs bytes.Buffer
	m := NewSwapMailer(NewDevMailer(slog.New(slog.NewTextHandler(&logs, nil))))
	ctx := context.Background()
	if err := m.SendVerificationEmail(ctx, "u@example.test", "https://app.example.test/v"); err != nil {
		t.Fatal(err)
	}
	m.Swap(resend("re_old"))
	if err := m.SendPasswordResetEmail(ctx, "u@example.test", "https://app.example.test/r"); err != nil {
		t.Fatal(err)
	}
	m.Swap(resend("re_new")) // the key rotated
	if err := m.SendEmailChangeNotice(ctx, "u@example.test", "n@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := m.SendSetPasswordEmail(ctx, "u@example.test", "https://app.example.test/s"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "verification email") || strings.Join(keys, ",") != "Bearer re_old,Bearer re_new,Bearer re_new" {
		t.Fatalf("dev log %q, resend keys %v", logs.String(), keys)
	}
}
