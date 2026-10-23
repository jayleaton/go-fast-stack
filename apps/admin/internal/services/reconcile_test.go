package services_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
)

// The reconciler's reasons, as shown in the audit log.
var (
	stateMatches = services.AuditOutcome{Status: services.StatusSucceeded, Reason: "Reconciled: the current state shows the change", Reconciled: true}
	stateDiffers = services.AuditOutcome{Status: services.StatusFailed, Reason: "Reconciled: the current state does not show the change", Reconciled: true}
	undecidable  = services.AuditOutcome{Status: services.StatusUnknown, Reason: "Reconciled: the outcome cannot be told from the current state", Reconciled: true}
)

const olderThan = 10 * time.Minute

func newReconciler(t *testing.T) (*services.AuditReconciler, ports) {
	p := newPorts(t)
	return services.NewAuditReconciler(p.log, p.dir, p.accounts), p
}

// staleCutoff matches Pending's before argument: now - olderThan, give or
// take the test's own runtime (the reconciler's clock is unexported).
func staleCutoff() gomock.Matcher {
	return gomock.Cond(func(before time.Time) bool {
		age := time.Since(before)
		return age >= olderThan && age < olderThan+time.Minute
	})
}

// pendingEntry is a stale intent of action on target id/label.
func pendingEntry(id int64, action, targetID, targetLabel string, details map[string]string) services.AuditEntry {
	return services.AuditEntry{
		ID: id, ActorID: "a1", ActorEmail: "grace@example.com", Action: action,
		TargetID: targetID, TargetLabel: targetLabel, Details: details,
		CreatedAt: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), Status: services.StatusPending,
	}
}

// Every action is judged by the state its change should have produced.
func TestReconcileVerdicts(t *testing.T) {
	t.Parallel()
	entryAt := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	user := func(account services.User, sessions ...services.Session) func(fakes ports) {
		return func(fakes ports) {
			fakes.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: account, Sessions: sessions}, nil)
		}
	}
	userGone := func(fakes ports) {
		fakes.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	}
	adminByID := func(admin services.Admin) func(fakes ports) {
		return func(fakes ports) { fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(admin, nil) }
	}
	adminGone := func(fakes ports) {
		fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, fmt.Errorf("get: %w", services.ErrRecordNotFound))
	}
	adminByEmail := func(found bool) func(fakes ports) {
		return func(fakes ports) {
			if found {
				fakes.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil)
				return
			}
			fakes.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)
		}
	}
	banned := adaUser
	banned.Banned = true
	promoted := adaUser
	promoted.Role = "admin"
	role := map[string]string{"from": "user", "to": "admin"}
	session := map[string]string{"sessionId": "s9"}

	cases := []struct {
		name   string
		action string
		target string // the entry's target id; its label follows
		detail map[string]string
		state  func(fakes ports) // nil: nothing is read
		want   services.AuditOutcome
	}{
		{"ban applied", "user.ban", "u1", nil, user(banned), stateMatches},
		{"ban not applied", "user.ban", "u1", nil, user(adaUser), stateDiffers},
		{"unban applied", "user.unban", "u1", nil, user(adaUser), stateMatches},
		{"unban not applied", "user.unban", "u1", nil, user(banned), stateDiffers},
		{"set_role applied", "user.set_role", "u1", role, user(promoted), stateMatches},
		{"set_role not applied", "user.set_role", "u1", role, user(adaUser), stateDiffers},
		{"revoke_session applied", "user.revoke_session", "u1", session, user(adaUser, services.Session{ID: "s1"}), stateMatches},
		{"revoke_session not applied", "user.revoke_session", "u1", session, user(adaUser, services.Session{ID: "s9"}), stateDiffers},
		{"revoke_sessions applied", "user.revoke_sessions", "u1", nil, user(adaUser, services.Session{ID: "new", CreatedAt: entryAt.Add(time.Minute)}), stateMatches},
		{"revoke_sessions not applied", "user.revoke_sessions", "u1", nil, user(adaUser, services.Session{ID: "old", CreatedAt: entryAt.Add(-time.Minute)}), stateDiffers},
		{"user gone", "user.ban", "u1", nil, userGone, undecidable},
		{"disable applied", "admin.disable", "a2", nil, adminByID(disBob), stateMatches},
		{"disable not applied", "admin.disable", "a2", nil, adminByID(bob), stateDiffers},
		{"reset 2fa applied", "admin.reset_two_factor", "a2", nil, adminByID(bob), stateMatches},
		{"reset 2fa not applied", "admin.reset_two_factor", "a2", nil, adminByID(bob2fa), stateDiffers},
		{"reset 2fa admin gone", "admin.reset_two_factor", "a2", nil, adminGone, undecidable},
		{"password reset", "admin.password_reset", "a2", nil, nil, undecidable}, // an email cannot be read back
		{"enable applied", "admin.enable", "a2", nil, adminByID(bob), stateMatches},
		{"enable not applied", "admin.enable", "a2", nil, adminByID(disBob), stateDiffers},
		{"disabled admin gone", "admin.disable", "a2", nil, adminGone, undecidable},
		{"enabled admin gone", "admin.enable", "a2", nil, adminGone, undecidable},
		{"remove applied", "admin.remove", "a2", nil, adminGone, stateMatches},
		{"remove not applied", "admin.remove", "a2", nil, adminByID(bob), stateDiffers},
		{"bootstrap applied", "admin.bootstrap", "bob@example.com", nil, adminByEmail(true), stateMatches},
		{"bootstrap not applied", "admin.bootstrap", "bob@example.com", nil, adminByEmail(false), stateDiffers},
		{"invite without admin", "admin.invite", "bob@example.com", nil, adminByEmail(false), stateDiffers},
		{"invite of an existing admin", "admin.invite", "a2", nil, adminByEmail(true), undecidable}, // the email cannot be read back
		{"unknown action", "admin.sign_in", "a2", nil, nil, undecidable},
	}
	labels := map[string]string{"u1": "ada@example.com", "a2": "bob@example.com", "bob@example.com": "bob@example.com"}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			reconciler, fakes := newReconciler(t)
			entry := pendingEntry(entryID, testCase.action, testCase.target, labels[testCase.target], testCase.detail)
			fakes.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return([]services.AuditEntry{entry}, nil)
			if testCase.state != nil {
				testCase.state(fakes)
			}
			fakes.log.EXPECT().Resolve(ctx, entryID, testCase.want).Return(nil)

			got, err := reconciler.Reconcile(ctx, olderThan)
			var want services.ReconcileResult
			switch testCase.want.Status {
			case services.StatusSucceeded:
				want.Succeeded = 1
			case services.StatusFailed:
				want.Failed = 1
			default:
				want.Unknown = 1
			}
			if err != nil || got != want {
				t.Fatalf("Reconcile = %+v, %v; want %+v", got, err, want)
			}
		})
	}
}

// An entry whose state cannot be read stays pending; the others are resolved
// and the read errors are returned.
func TestReconcileSkipsUnreadableState(t *testing.T) {
	t.Parallel()
	r, p := newReconciler(t)
	errUsers := errors.New("app database down")
	errAdmins := errors.New("admin database down")
	entries := []services.AuditEntry{
		pendingEntry(1, "user.ban", "u1", "ada@example.com", nil),
		pendingEntry(2, "admin.disable", "a2", "bob@example.com", nil),
		pendingEntry(3, "admin.invite", "bob@example.com", "bob@example.com", nil),
		pendingEntry(4, "admin.remove", "a3", "eve@example.com", nil),
	}
	gomock.InOrder(
		p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(entries, nil),
		p.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errUsers),
		p.accounts.EXPECT().AdminByID(ctx, "a2").Return(services.Admin{}, errAdmins),
		p.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, errAdmins),
		p.accounts.EXPECT().AdminByID(ctx, "a3").Return(services.Admin{}, services.ErrRecordNotFound),
		p.log.EXPECT().Resolve(ctx, int64(4), stateMatches).Return(nil),
	)

	got, err := r.Reconcile(ctx, olderThan)
	if want := (services.ReconcileResult{Succeeded: 1, Skipped: 3}); got != want {
		t.Fatalf("Reconcile = %+v, want %+v", got, want)
	}
	if !errors.Is(err, errUsers) || !errors.Is(err, errAdmins) {
		t.Fatalf("err = %v, want the read errors", err)
	}
}

// A failed outcome write stops the pass: the store is likely down.
func TestReconcileStopsWhenResolveFails(t *testing.T) {
	t.Parallel()
	r, p := newReconciler(t)
	errRead := errors.New("app database down")
	entries := []services.AuditEntry{
		pendingEntry(1, "user.ban", "u1", "ada@example.com", nil),
		pendingEntry(2, "admin.sign_in", "a2", "bob@example.com", nil),
		pendingEntry(3, "admin.sign_in", "a2", "bob@example.com", nil), // never reached
	}
	gomock.InOrder(
		p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(entries, nil),
		p.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errRead),
		p.log.EXPECT().Resolve(ctx, int64(2), undecidable).Return(errResolve),
	)

	got, err := r.Reconcile(ctx, olderThan)
	if want := (services.ReconcileResult{Skipped: 1}); got != want {
		t.Fatalf("Reconcile = %+v, want %+v", got, want)
	}
	if !errors.Is(err, errResolve) || !errors.Is(err, errRead) {
		t.Fatalf("err = %v, want the resolve and read errors", err)
	}
}

func TestReconcilePendingFails(t *testing.T) {
	t.Parallel()
	r, p := newReconciler(t)
	p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(nil, errDB)
	got, err := r.Reconcile(ctx, olderThan)
	wantInternal(t, err, errDB)
	if got != (services.ReconcileResult{}) {
		t.Fatalf("Reconcile = %+v, want nothing done", got)
	}
}

func TestReconcileNothingPending(t *testing.T) {
	t.Parallel()
	r, p := newReconciler(t)
	p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(nil, nil)
	if got, err := r.Reconcile(ctx, olderThan); err != nil || got != (services.ReconcileResult{}) {
		t.Fatalf("Reconcile = %+v, %v", got, err)
	}
}

// A full batch that made progress is followed by the next one, until a
// batch comes back short.
func TestReconcileBatches(t *testing.T) {
	t.Parallel()
	r, p := newReconciler(t)
	batch := func(from, n int) []services.AuditEntry {
		entries := make([]services.AuditEntry, n)
		for i := range entries {
			entries[i] = pendingEntry(int64(from+i), "admin.sign_in", "a2", "bob@example.com", nil)
		}
		return entries
	}
	gomock.InOrder(
		p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(batch(1, 100), nil),
		p.log.EXPECT().Resolve(ctx, gomock.Any(), undecidable).Return(nil).Times(100),
		p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(batch(101, 3), nil),
		p.log.EXPECT().Resolve(ctx, gomock.Any(), undecidable).Return(nil).Times(3),
	)
	if got, err := r.Reconcile(ctx, olderThan); err != nil || got != (services.ReconcileResult{Unknown: 103}) {
		t.Fatalf("Reconcile = %+v, %v", got, err)
	}
}

// An unreadable entry in a full batch does not end the pass: the next read
// asks for one more row (the skipped entry comes back first) and skips it.
func TestReconcileReadsPastSkippedEntries(t *testing.T) {
	t.Parallel()
	r, p := newReconciler(t)
	errRead := errors.New("app database down")
	unreadable := pendingEntry(1, "user.ban", "u1", "ada@example.com", nil)
	first := []services.AuditEntry{unreadable}
	for i := range 99 {
		first = append(first, pendingEntry(int64(2+i), "admin.sign_in", "a2", "bob@example.com", nil))
	}
	gomock.InOrder(
		p.log.EXPECT().Pending(ctx, staleCutoff(), 100).Return(first, nil),
		p.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errRead),
		p.log.EXPECT().Resolve(ctx, gomock.Any(), undecidable).Return(nil).Times(99),
		p.log.EXPECT().Pending(ctx, staleCutoff(), 101).Return([]services.AuditEntry{unreadable, pendingEntry(200, "admin.sign_in", "a2", "bob@example.com", nil)}, nil),
		p.log.EXPECT().Resolve(ctx, int64(200), undecidable).Return(nil),
	)
	got, err := r.Reconcile(ctx, olderThan)
	if want := (services.ReconcileResult{Unknown: 100, Skipped: 1}); got != want || !errors.Is(err, errRead) {
		t.Fatalf("Reconcile = %+v, %v; want %+v and the read error", got, err, want)
	}
}
