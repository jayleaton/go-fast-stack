package services_test

import (
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services/mocks"
)

func newAuthEventService(t *testing.T) (*services.AuthEventService, *mocks.MockAuditLog) {
	p := newPorts(t)
	return services.NewAuthEventService(p.log), p.log
}

// at is when the test's events happened: they arrive after the fact, and the
// entry keeps that time so the log sorts them where they happened.
var at = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// selfAudit is the entry an admin's own authentication event writes: they
// are both actor and target.
func selfAudit(id, email, action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: id, ActorEmail: email, Action: action,
		TargetType: "admin", TargetID: id, TargetLabel: email, Details: details, At: at,
	}
}

// No policy check by design: the events describe authentication itself, so
// even an admin without 2FA is recorded.
func TestSignedIn(t *testing.T) {
	t.Parallel()
	svc, log := newAuthEventService(t)

	for _, method := range []string{services.SignInPassword, services.SignInTwoFactor, services.SignInSetPassword} {
		log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.sign_in", map[string]string{"method": method})).Return(nil)
		if err := svc.SignedIn(ctx, at, "a1", "grace@example.com", method); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.SignedIn(ctx, at, "a1", "grace@example.com", services.SignInPassword), errDB)
}

func TestSignedOut(t *testing.T) {
	t.Parallel()
	svc, log := newAuthEventService(t)

	log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.sign_out", nil)).Return(nil)
	if err := svc.SignedOut(ctx, at, "a1", "grace@example.com"); err != nil {
		t.Fatal(err)
	}

	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.SignedOut(ctx, at, "a1", "grace@example.com"), errDB)
}

func TestTwoFactorFailed(t *testing.T) {
	t.Parallel()
	svc, log := newAuthEventService(t)

	// Attributed to the admin of the pending sign-in, with auth's reason.
	log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.two_factor_failed", map[string]string{"reason": "INVALID_CODE"})).Return(nil)
	if err := svc.TwoFactorFailed(ctx, at, "a1", "grace@example.com", "INVALID_CODE"); err != nil {
		t.Fatal(err)
	}

	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.TwoFactorFailed(ctx, at, "a1", "grace@example.com", "INVALID_CODE"), errDB)
}

func TestBackupCodeUsed(t *testing.T) {
	t.Parallel()
	svc, log := newAuthEventService(t)

	log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.backup_code_used", nil)).Return(nil)
	if err := svc.BackupCodeUsed(ctx, at, "a1", "grace@example.com"); err != nil {
		t.Fatal(err)
	}

	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.BackupCodeUsed(ctx, at, "a1", "grace@example.com"), errDB)
}
