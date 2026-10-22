package services

import (
	"context"
)

// Audit actions for admin authentication. The admin is both actor and target.
const (
	ActionAdminSignIn          = "admin.sign_in"
	ActionAdminSignOut         = "admin.sign_out"
	ActionAdminTwoFactorFailed = "admin.two_factor_failed"
)

// How an admin signed in (the "method" detail of ActionAdminSignIn).
const (
	SignInPassword    = "password"     // no second factor on this sign-in
	SignInTwoFactor   = "two_factor"   // password, then a TOTP or backup code
	SignInSetPassword = "set_password" // the invite / reset link
)

// AuthEventService audits admin sign-ins, sign-outs and failed 2FA codes.
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
func (s *AuthEventService) SignedIn(ctx context.Context, adminID, email, method string) error {
	return s.record(ctx, ActionAdminSignIn, adminID, email, map[string]string{"method": method})
}

// SignedOut records an admin ending their session.
func (s *AuthEventService) SignedOut(ctx context.Context, adminID, email string) error {
	return s.record(ctx, ActionAdminSignOut, adminID, email, nil)
}

// TwoFactorFailed records a refused second-factor code for the admin whose
// pending sign-in (or enrolment) it was; reason is auth's error code.
func (s *AuthEventService) TwoFactorFailed(ctx context.Context, adminID, email, reason string) error {
	return s.record(ctx, ActionAdminTwoFactorFailed, adminID, email, map[string]string{"reason": reason})
}

func (s *AuthEventService) record(ctx context.Context, action, adminID, email string, details map[string]string) error {
	self := Actor{AdminID: adminID, Email: email}
	return audit(ctx, s.log, self, NewAuditEntry{Action: action, TargetType: TargetAdmin, TargetID: adminID, TargetLabel: email, Details: details})
}
