package auth

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// DevMailer is a Mailer for development: instead of sending emails it logs
// the recipient and the link, so verification and password reset flows can be
// completed by copying the URL from the server log.
type DevMailer struct {
	Logger *slog.Logger
}

var (
	_ Mailer = (*DevMailer)(nil)
	_ Mailer = (*ResendMailer)(nil)
	_ Mailer = (*SwapMailer)(nil)
)

// NewDevMailer returns a DevMailer that writes to logger, or to slog.Default()
// when logger is nil.
func NewDevMailer(logger *slog.Logger) *DevMailer {
	if logger == nil {
		logger = slog.Default()
	}
	return &DevMailer{Logger: logger}
}

// SendVerificationEmail logs the email verification link for to. It never fails.
func (m *DevMailer) SendVerificationEmail(ctx context.Context, to, url string) error {
	m.logger().InfoContext(ctx, "auth: verification email (dev mode, not sent)", "to", to, "url", url)
	return nil
}

// SendPasswordResetEmail logs the password reset link for to. It never fails.
func (m *DevMailer) SendPasswordResetEmail(ctx context.Context, to, url string) error {
	m.logger().InfoContext(ctx, "auth: password reset email (dev mode, not sent)", "to", to, "url", url)
	return nil
}

// SendSetPasswordEmail logs the set-password (invite) link for to. It never fails.
func (m *DevMailer) SendSetPasswordEmail(ctx context.Context, to, url string) error {
	m.logger().InfoContext(ctx, "auth: set-password email (dev mode, not sent)", "to", to, "url", url)
	return nil
}

// SendEmailChangeNotice logs the email change notice for to. It never fails.
func (m *DevMailer) SendEmailChangeNotice(ctx context.Context, to, newEmail string) error {
	m.logger().InfoContext(ctx, "auth: email change notice (dev mode, not sent)", "to", to, "newEmail", newEmail)
	return nil
}

// logger lets a zero-value DevMailer work without NewDevMailer.
func (m *DevMailer) logger() *slog.Logger {
	if m.Logger == nil {
		return slog.Default()
	}
	return m.Logger
}

// SwapMailer is a Mailer whose implementation can be replaced while the
// server runs: when RESEND_API_KEY rotates, the wiring builds a new
// ResendMailer and swaps it in. Emails already being sent finish on the old
// one. Safe for concurrent use.
type SwapMailer struct {
	cur atomic.Pointer[mailerBox]
}

type mailerBox struct{ Mailer }

// NewSwapMailer returns a SwapMailer that starts with initial.
func NewSwapMailer(initial Mailer) *SwapMailer {
	m := &SwapMailer{}
	m.Swap(initial)
	return m
}

// Swap makes next handle every email sent from now on.
func (m *SwapMailer) Swap(next Mailer) { m.cur.Store(&mailerBox{next}) }

// SendVerificationEmail delegates to the current Mailer.
func (m *SwapMailer) SendVerificationEmail(ctx context.Context, to, url string) error {
	return m.cur.Load().SendVerificationEmail(ctx, to, url)
}

// SendPasswordResetEmail delegates to the current Mailer.
func (m *SwapMailer) SendPasswordResetEmail(ctx context.Context, to, url string) error {
	return m.cur.Load().SendPasswordResetEmail(ctx, to, url)
}

// SendEmailChangeNotice delegates to the current Mailer.
func (m *SwapMailer) SendEmailChangeNotice(ctx context.Context, to, newEmail string) error {
	return m.cur.Load().SendEmailChangeNotice(ctx, to, newEmail)
}

// SendSetPasswordEmail delegates to the current Mailer.
func (m *SwapMailer) SendSetPasswordEmail(ctx context.Context, to, url string) error {
	return m.cur.Load().SendSetPasswordEmail(ctx, to, url)
}
