package auth

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"net/http"
	"strings"
	texttemplate "text/template"
	"time"
)

// DefaultResendEndpoint is the Resend API URL used when ResendMailer.Endpoint is empty.
const DefaultResendEndpoint = "https://api.resend.com/emails"

const (
	resendTimeout      = 15 * time.Second // the default HTTPClient's timeout
	resendErrorSnippet = 512              // bytes of a failed response quoted in the error
)

//go:embed templates
var emailTemplatesFS embed.FS

// Email templates are parsed once at startup; a broken template panics at init.
var (
	emailHTMLTemplates = htmltemplate.Must(htmltemplate.ParseFS(emailTemplatesFS, "templates/*.html"))
	emailTextTemplates = texttemplate.Must(texttemplate.ParseFS(emailTemplatesFS, "templates/*.txt"))
)

// emailData is the data passed to every email template.
type emailData struct {
	Email    string
	URL      string
	NewEmail string // email_change_notice only
}

// ResendMailer sends auth emails through the Resend HTTP API
// (https://resend.com/docs/api-reference/emails/send-email) without the SDK.
// Use NewResendMailer, or fill the fields directly; Endpoint and HTTPClient
// are optional (tests point Endpoint at an httptest server).
type ResendMailer struct {
	APIKey     string       // RESEND_API_KEY
	From       string       // sender, e.g. "App <noreply@example.com>"
	Endpoint   string       // defaults to DefaultResendEndpoint
	HTTPClient *http.Client // defaults to a client with a 15s timeout
}

// NewResendMailer returns a ResendMailer that sends from the given address
// using the default endpoint and a 15s-timeout HTTP client.
func NewResendMailer(apiKey, from string) *ResendMailer {
	return &ResendMailer{
		APIKey:     apiKey,
		From:       from,
		Endpoint:   DefaultResendEndpoint,
		HTTPClient: &http.Client{Timeout: resendTimeout},
	}
}

// SendVerificationEmail sends the "verify your email" email containing url to to.
func (mailer *ResendMailer) SendVerificationEmail(ctx context.Context, to, url string) error {
	return mailer.send(ctx, to, "Verify your email address", "verification", emailData{Email: to, URL: url})
}

// SendPasswordResetEmail sends the "reset your password" email containing url to to.
func (mailer *ResendMailer) SendPasswordResetEmail(ctx context.Context, to, url string) error {
	return mailer.send(ctx, to, "Reset your password", "password_reset", emailData{Email: to, URL: url})
}

// SendSetPasswordEmail sends the "set your password" invitation containing url to to.
func (mailer *ResendMailer) SendSetPasswordEmail(ctx context.Context, to, url string) error {
	return mailer.send(ctx, to, "Set your password", "set_password", emailData{Email: to, URL: url})
}

// SendEmailChangeNotice tells to that the account's email is being changed to newEmail.
func (mailer *ResendMailer) SendEmailChangeNotice(ctx context.Context, to, newEmail string) error {
	return mailer.send(ctx, to, "Your email address is being changed", "email_change_notice", emailData{Email: to, NewEmail: newEmail})
}

// send renders templates/<name>.html and .txt and posts them to Resend.
func (mailer *ResendMailer) send(ctx context.Context, to, subject, name string, data emailData) error {
	if mailer.APIKey == "" {
		return errors.New("auth: resend mailer: missing API key")
	}
	if mailer.From == "" {
		return errors.New("auth: resend mailer: missing from address")
	}

	var html, text strings.Builder
	if err := emailHTMLTemplates.ExecuteTemplate(&html, name+".html", data); err != nil {
		return fmt.Errorf("auth: render %s.html: %w", name, err)
	}
	if err := emailTextTemplates.ExecuteTemplate(&text, name+".txt", data); err != nil {
		return fmt.Errorf("auth: render %s.txt: %w", name, err)
	}

	body, err := json.Marshal(struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
		Text    string   `json:"text"`
	}{mailer.From, []string{to}, subject, html.String(), text.String()})
	if err != nil {
		return fmt.Errorf("auth: encode resend request: %w", err)
	}

	endpoint := mailer.Endpoint
	if endpoint == "" {
		endpoint = DefaultResendEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("auth: build resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+mailer.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := mailer.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: resendTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: resend request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, resendErrorSnippet))
		return fmt.Errorf("auth: resend returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
