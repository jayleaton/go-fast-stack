package services_test

import (
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services/mocks"
)

func newAuthEventService(t *testing.T) (*services.AuthEventService, *mocks.MockTwoFactorAttempts, *mocks.MockAuditLog) {
	p := newPorts(t)
	return services.NewAuthEventService(p.attempts, p.log), p.attempts, p.log
}

// selfAudit is the entry an admin's own authentication event writes: they
// are both actor and target.
func selfAudit(id, email, action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: id, ActorEmail: email, Action: action,
		TargetType: "admin", TargetID: id, TargetLabel: email, Details: details,
	}
}

// No policy check by design: the events describe authentication itself, so
// even an admin without 2FA is recorded.
func TestSignedIn(t *testing.T) {
	svc, _, log := newAuthEventService(t)

	for _, method := range []string{services.SignInPassword, services.SignInTwoFactor, services.SignInSetPassword} {
		log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.sign_in", map[string]string{"method": method})).Return(nil)
		if err := svc.SignedIn(ctx, "a1", "grace@example.com", method); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}

	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.SignedIn(ctx, "a1", "grace@example.com", services.SignInPassword), errDB)
}

func TestSignedOut(t *testing.T) {
	svc, _, log := newAuthEventService(t)

	log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.sign_out", nil)).Return(nil)
	if err := svc.SignedOut(ctx, "a1", "grace@example.com"); err != nil {
		t.Fatal(err)
	}

	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.SignedOut(ctx, "a1", "grace@example.com"), errDB)
}

func TestTwoFactorFailed(t *testing.T) {
	svc, attempts, log := newAuthEventService(t)
	since := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	failed := []services.TwoFactorFailure{
		{AdminID: "a1", Email: "grace@example.com", Failures: 1},
		{AdminID: "a2", Email: "bob@example.com", Failures: 5},
	}

	// One entry per admin whose counter moved, in order.
	gomock.InOrder(
		attempts.EXPECT().FailedSince(ctx, since).Return(failed, nil),
		log.EXPECT().Append(ctx, selfAudit("a1", "grace@example.com", "admin.two_factor_failed", map[string]string{"failures": "1"})).Return(nil),
		log.EXPECT().Append(ctx, selfAudit("a2", "bob@example.com", "admin.two_factor_failed", map[string]string{"failures": "5"})).Return(nil),
	)
	if err := svc.TwoFactorFailed(ctx, since); err != nil {
		t.Fatal(err)
	}

	// Nothing moved: nothing is written.
	attempts.EXPECT().FailedSince(ctx, since).Return(nil, nil)
	if err := svc.TwoFactorFailed(ctx, since); err != nil {
		t.Fatal(err)
	}

	attempts.EXPECT().FailedSince(ctx, since).Return(nil, errDB)
	wantInternal(t, svc.TwoFactorFailed(ctx, since), errDB)

	// A failed write stops at that entry and is returned.
	attempts.EXPECT().FailedSince(ctx, since).Return(failed, nil)
	log.EXPECT().Append(ctx, gomock.Any()).Return(errDB)
	wantInternal(t, svc.TwoFactorFailed(ctx, since), errDB)
}
