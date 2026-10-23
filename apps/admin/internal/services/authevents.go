package services

import (
	"context"
	"time"
)

// Audit actions for admin authentication. The admin is both actor and target.
const (
	ActionAdminSignIn          = "admin.sign_in"
	ActionAdminSignOut         = "admin.sign_out"
	ActionAdminTwoFactorFailed = "admin.two_factor_failed"
	// ActionAdminBackupCodeUsed: a backup code was accepted (and spent). An
	// admin who signs in with one has probably lost the authenticator.
	ActionAdminBackupCodeUsed = "admin.backup_code_used"
)

// How an admin signed in (the "method" detail of ActionAdminSignIn).
const (
	SignInPassword    = "password"     // no second factor on this sign-in
	SignInTwoFactor   = "two_factor"   // password, then a TOTP or backup code
	SignInSetPassword = "set_password" // the invite / reset link
)

// AuthEventService audits admin sign-ins, sign-outs, failed 2FA codes and
// spent backup codes.
// apps/admin/internal/server feeds it the admin auth instance's events
// (auth.Config.Events), never a transport for a caller: the events describe
// authentication itself, so there is no actor to authorize yet.
type AuthEventService struct {
	log AuditLog
}

// NewAuthEventService returns an AuthEventService over the audit log.
func NewAuthEventService(log AuditLog) *AuthEventService {
	return &AuthEventService{log: log}
}

// SignedIn records a new admin session; method is one of the SignIn* values.
// at is when it happened: events arrive after the fact, and the log is
// ordered by it.
func (s *AuthEventService) SignedIn(ctx context.Context, at time.Time, adminID, email, method string) error {
	return s.record(ctx, at, ActionAdminSignIn, adminID, email, map[string]string{"method": method})
}

// SignedOut records an admin ending their session.
func (s *AuthEventService) SignedOut(ctx context.Context, at time.Time, adminID, email string) error {
	return s.record(ctx, at, ActionAdminSignOut, adminID, email, nil)
}

// TwoFactorFailed records a refused second-factor code for the admin whose
// pending sign-in (or enrolment) it was; reason is auth's error code.
func (s *AuthEventService) TwoFactorFailed(ctx context.Context, at time.Time, adminID, email, reason string) error {
	return s.record(ctx, at, ActionAdminTwoFactorFailed, adminID, email, map[string]string{"reason": reason})
}

// BackupCodeUsed records an accepted backup code for the admin whose pending
// sign-in it completed. The code is single-use: it is gone once recorded.
func (s *AuthEventService) BackupCodeUsed(ctx context.Context, at time.Time, adminID, email string) error {
	return s.record(ctx, at, ActionAdminBackupCodeUsed, adminID, email, nil)
}

func (s *AuthEventService) record(ctx context.Context, at time.Time, action, adminID, email string, details map[string]string) error {
	self := Actor{AdminID: adminID, Email: email}
	return audit(ctx, s.log, self, NewAuditEntry{Action: action, TargetType: TargetAdmin, TargetID: adminID, TargetLabel: email, Details: details, At: at})
}
