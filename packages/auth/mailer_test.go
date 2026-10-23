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

func TestResendMailerSends(test *testing.T) {
	test.Parallel()
	const link = "https://app.example.test/api/auth/verify-email?token=abc&next=/dashboard"

	tests := []struct {
		name        string
		to          string
		send        func(mailer *ResendMailer, to string) error
		subject     string
		htmlContain []string
		textContain []string
		noLink      bool // the email carries no link
	}{
		{
			name: "verification",
			to:   "user@example.test",
			send: func(mailer *ResendMailer, to string) error {
				return mailer.SendVerificationEmail(context.Background(), to, link)
			},
			subject:     "Verify your email address",
			htmlContain: []string{"Verify Email Address", "user@example.test", "expire in 24 hours"},
			textContain: []string{"user@example.test", "expire in 24 hours"},
		},
		{
			name: "password reset",
			to:   "user@example.test",
			send: func(mailer *ResendMailer, to string) error {
				return mailer.SendPasswordResetEmail(context.Background(), to, link)
			},
			subject:     "Reset your password",
			htmlContain: []string{"Reset Password", "user@example.test", "expire in 1 hour", "safely ignore this email"},
			textContain: []string{"user@example.test", "expire in 1 hour", "safely ignore this email"},
		},
		{
			name: "email change notice",
			to:   "old@example.test",
			send: func(mailer *ResendMailer, to string) error {
				return mailer.SendEmailChangeNotice(context.Background(), to, "<i>new</i>@example.test")
			},
			subject:     "Your email address is being changed",
			htmlContain: []string{"old@example.test", "&lt;i&gt;new&lt;/i&gt;@example.test", "reset your password"},
			textContain: []string{"old@example.test", "<i>new</i>@example.test", "reset your password"},
			noLink:      true,
		},
		{
			name: "email is html-escaped",
			to:   "<b>evil</b>@example.test",
			send: func(mailer *ResendMailer, to string) error {
				return mailer.SendVerificationEmail(context.Background(), to, link)
			},
			subject:     "Verify your email address",
			htmlContain: []string{"&lt;b&gt;evil&lt;/b&gt;@example.test"},
			textContain: []string{"<b>evil</b>@example.test"},
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			test.Parallel()
			var got resendPayload
			srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost {
					test.Errorf("method = %s, want POST", request.Method)
				}
				if request.URL.Path != "/emails" {
					test.Errorf("path = %s, want /emails", request.URL.Path)
				}
				if header := request.Header.Get("Authorization"); header != "Bearer re_test" {
					test.Errorf("Authorization = %q", header)
				}
				if header := request.Header.Get("Content-Type"); header != "application/json" {
					test.Errorf("Content-Type = %q", header)
				}
				if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
					test.Errorf("decode body: %v", err)
				}
				writer.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(writer, `{"id":"email_1"}`)
			}))
			defer srv.Close()

			mailer := NewResendMailer("re_test", "App <noreply@example.test>")
			mailer.Endpoint = srv.URL + "/emails"
			if err := tt.send(mailer, tt.to); err != nil {
				test.Fatalf("send: %v", err)
			}

			if got.From != "App <noreply@example.test>" {
				test.Errorf("from = %q", got.From)
			}
			if len(got.To) != 1 || got.To[0] != tt.to {
				test.Errorf("to = %v, want [%s]", got.To, tt.to)
			}
			if got.Subject != tt.subject {
				test.Errorf("subject = %q, want %q", got.Subject, tt.subject)
			}
			// html/template escapes & in attributes and text as &amp;.
			htmlWant, textWant := tt.htmlContain, tt.textContain
			if !tt.noLink {
				htmlWant = append([]string{strings.ReplaceAll(link, "&", "&amp;")}, htmlWant...)
				textWant = append([]string{link}, textWant...)
			}
			for _, text := range htmlWant {
				if !strings.Contains(got.HTML, text) {
					test.Errorf("html missing %q", text)
				}
			}
			if strings.Contains(tt.to, "<") && strings.Contains(got.HTML, tt.to) {
				test.Errorf("html contains unescaped email %q", tt.to)
			}
			for _, text := range textWant {
				if !strings.Contains(got.Text, text) {
					test.Errorf("text missing %q", text)
				}
			}
		})
	}
}

func TestResendMailerErrors(test *testing.T) {
	test.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(writer, `{"message":"invalid from address"}`)
	}))
	test.Cleanup(srv.Close) // after the parallel subtests, not when this function returns

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
		test.Run(tt.name, func(test *testing.T) {
			test.Parallel()
			err := tt.mailer.SendPasswordResetEmail(context.Background(), "user@example.test", "https://app.example.test/reset")
			if err == nil {
				test.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				test.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDevMailerLogsLink(test *testing.T) {
	test.Parallel()
	tests := []struct {
		name string
		send func(mailer *DevMailer) error
		msg  string
	}{
		{"verification", func(mailer *DevMailer) error {
			return mailer.SendVerificationEmail(context.Background(), "user@example.test", "https://app.example.test/verify?token=t1")
		}, "verification email"},
		{"password reset", func(mailer *DevMailer) error {
			return mailer.SendPasswordResetEmail(context.Background(), "user@example.test", "https://app.example.test/verify?token=t1")
		}, "password reset email"},
	}
	for _, tt := range tests {
		test.Run(tt.name, func(test *testing.T) {
			test.Parallel()
			var buf bytes.Buffer
			mailer := NewDevMailer(slog.New(slog.NewTextHandler(&buf, nil)))
			if err := tt.send(mailer); err != nil {
				test.Fatalf("send: %v", err)
			}
			out := buf.String()
			for _, text := range []string{tt.msg, "to=user@example.test", "https://app.example.test/verify?token=t1"} {
				if !strings.Contains(out, text) {
					test.Errorf("log %q missing %q", out, text)
				}
			}
		})
	}
}

func TestNewDevMailerNilLogger(test *testing.T) {
	test.Parallel()
	if mailer := NewDevMailer(nil); mailer.Logger == nil {
		test.Fatal("expected default logger")
	}
}

func TestSwapMailerUsesTheLatestMailer(test *testing.T) {
	test.Parallel()
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		keys = append(keys, request.Header.Get("Authorization"))
		writer.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	resend := func(key string) *ResendMailer {
		return &ResendMailer{APIKey: key, From: "a@example.test", Endpoint: srv.URL, HTTPClient: srv.Client()}
	}

	var logs bytes.Buffer
	mailer := NewSwapMailer(NewDevMailer(slog.New(slog.NewTextHandler(&logs, nil))))
	ctx := context.Background()
	if err := mailer.SendVerificationEmail(ctx, "u@example.test", "https://app.example.test/v"); err != nil {
		test.Fatal(err)
	}
	mailer.Swap(resend("re_old"))
	if err := mailer.SendPasswordResetEmail(ctx, "u@example.test", "https://app.example.test/r"); err != nil {
		test.Fatal(err)
	}
	mailer.Swap(resend("re_new")) // the key rotated
	if err := mailer.SendEmailChangeNotice(ctx, "u@example.test", "n@example.test"); err != nil {
		test.Fatal(err)
	}
	if err := mailer.SendSetPasswordEmail(ctx, "u@example.test", "https://app.example.test/s"); err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(logs.String(), "verification email") || strings.Join(keys, ",") != "Bearer re_old,Bearer re_new,Bearer re_new" {
		test.Fatalf("dev log %q, resend keys %v", logs.String(), keys)
	}
}
