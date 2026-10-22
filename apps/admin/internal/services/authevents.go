package services

//go:generate go tool mockgen -source=authevents.go -destination=mocks/authevents.go -package=mocks

import (
	"context"
	"strconv"
	"time"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
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

// TwoFactorFailure is an admin whose wrong second-factor codes were counted
// (re-exported from the repository model).
type TwoFactorFailure = model.TwoFactorFailure

// TwoFactorAttempts is the port to the admin auth instance's 2FA failure
// counters (adminrepo.Admins, read-only over the admin database).
type TwoFactorAttempts interface {
	// FailedSince lists the admins whose failure counter moved at or after since.
	FailedSince(ctx context.Context, since time.Time) ([]TwoFactorFailure, error)
}

// AuthEventService audits admin sign-ins, sign-outs and failed 2FA codes.
// It is called by apps/admin/internal/server around the admin auth instance
// (the UI adapter and the JSON auth endpoints), never by a transport for a
// caller: the events describe authentication itself, so there is no actor
// to authorize yet.
type AuthEventService struct {
	attempts TwoFactorAttempts
	log      AuditLog
}

// NewAuthEventService returns an AuthEventService over its ports.
func NewAuthEventService(attempts TwoFactorAttempts, log AuditLog) *AuthEventService {
	return &AuthEventService{attempts: attempts, log: log}
}

// SignedIn records a new admin session; method is one of the SignIn* values.
func (s *AuthEventService) SignedIn(ctx context.Context, adminID, email, method string) error {
	return s.record(ctx, ActionAdminSignIn, adminID, email, map[string]string{"method": method})
}

// SignedOut records an admin ending their session.
func (s *AuthEventService) SignedOut(ctx context.Context, adminID, email string) error {
	return s.record(ctx, ActionAdminSignOut, adminID, email, nil)
}

// TwoFactorFailed records every admin whose wrong-code counter moved since
// since. The server calls it after a failed verification it started at since
// (verifications are serialized, so the counter that moved is that one's).
func (s *AuthEventService) TwoFactorFailed(ctx context.Context, since time.Time) error {
	failed, err := s.attempts.FailedSince(ctx, since)
	if err != nil {
		return err
	}
	for _, f := range failed {
		if err := s.record(ctx, ActionAdminTwoFactorFailed, f.AdminID, f.Email, map[string]string{"failures": strconv.FormatInt(f.Failures, 10)}); err != nil {
			return err
		}
	}
	return nil
}

func (s *AuthEventService) record(ctx context.Context, action, adminID, email string, details map[string]string) error {
	self := Actor{AdminID: adminID, Email: email}
	return audit(ctx, s.log, self, NewAuditEntry{Action: action, TargetType: TargetAdmin, TargetID: adminID, TargetLabel: email, Details: details})
}
