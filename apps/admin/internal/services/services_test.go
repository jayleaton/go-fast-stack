package services_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services/mocks"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

var (
	ctx     = context.Background()
	grace   = services.Actor{AdminID: "a1", Email: "grace@example.com", SessionID: "s1", TwoFactorEnabled: true}
	cli     = services.SystemActor("cli")
	adaUser = services.User{ID: "u1", Name: "Ada", Email: "ada@example.com", Role: "user"}
	errDB   = errors.New("connection refused")
)

func ptr[T any](v T) *T { return &v }

// entryID is the id Begin hands out for an intent in these tests.
const entryID int64 = 7

// succeeded is the outcome of a change that was applied.
var succeeded = services.AuditOutcome{Status: services.StatusSucceeded}

// failed is the outcome of a change that was refused with reason.
func failed(reason string) services.AuditOutcome {
	return services.AuditOutcome{Status: services.StatusFailed, Reason: reason}
}

// internalFailure is the outcome of a change that failed with a raw error:
// the raw text never reaches the log.
var internalFailure = failed("Internal error")

// ports holds every generated port mock; a service under test uses some.
type ports struct {
	dir       *mocks.MockUserDirectory
	accounts  *mocks.MockAdminAccounts
	log       *mocks.MockAuditLog
	analytics *mocks.MockAnalytics
}

func newPorts(t *testing.T) ports {
	ctrl := gomock.NewController(t)
	return ports{
		dir:       mocks.NewMockUserDirectory(ctrl),
		accounts:  mocks.NewMockAdminAccounts(ctrl),
		log:       mocks.NewMockAuditLog(ctrl),
		analytics: mocks.NewMockAnalytics(ctrl),
	}
}

// wantErr fails unless errors.Is(err, target).
func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

// wantInternal fails unless err is cause passed through as an internal error.
func wantInternal(t *testing.T, err, cause error) {
	t.Helper()
	if services.KindOf(err) != services.KindInternal || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want internal wrapping %v", err, cause)
	}
}

// policyCalls calls every method of every service with actor, over ports p.
func policyCalls(p ports, policy services.Policy) map[string]func(services.Actor) error {
	users := services.NewUserService(p.dir, p.log, policy)
	admins := services.NewAdminService(p.accounts, p.log, policy)
	auditSvc := services.NewAuditService(p.log, policy)
	analytics := services.NewAnalyticsService(p.analytics, policy)
	return map[string]func(services.Actor) error{
		"UserService.ListUsers": func(a services.Actor) error {
			_, err := users.ListUsers(ctx, a, validate.ListUsers{Limit: 25})
			return err
		},
		"UserService.GetUser": func(a services.Actor) error { _, err := users.GetUser(ctx, a, "u1"); return err },
		"UserService.BanUser": func(a services.Actor) error {
			return users.BanUser(ctx, a, "u1", validate.BanUser{BanReason: "spam"})
		},
		"UserService.UnbanUser": func(a services.Actor) error { return users.UnbanUser(ctx, a, "u1") },
		"UserService.SetRole": func(a services.Actor) error {
			return users.SetRole(ctx, a, "u1", validate.SetUserRole{Role: "admin"})
		},
		"UserService.RevokeSession":  func(a services.Actor) error { return users.RevokeSession(ctx, a, "u1", "s9") },
		"UserService.RevokeSessions": func(a services.Actor) error { return users.RevokeSessions(ctx, a, "u1") },
		"AdminService.ListAdmins":    func(a services.Actor) error { _, err := admins.ListAdmins(ctx, a); return err },
		"AdminService.InviteAdmin": func(a services.Actor) error {
			_, err := admins.InviteAdmin(ctx, a, invite)
			return err
		},
		"AdminService.DisableAdmin": func(a services.Actor) error { return admins.DisableAdmin(ctx, a, "a2") },
		"AdminService.EnableAdmin":  func(a services.Actor) error { return admins.EnableAdmin(ctx, a, "a2") },
		"AdminService.RemoveAdmin":  func(a services.Actor) error { return admins.RemoveAdmin(ctx, a, "a2") },
		"AdminService.Bootstrap": func(a services.Actor) error {
			_, _, err := admins.Bootstrap(ctx, a, invite)
			return err
		},
		"AuditService.List": func(a services.Actor) error {
			_, err := auditSvc.List(ctx, a, validate.ListAuditLog{Limit: 25})
			return err
		},
		"AnalyticsService.Overview": func(a services.Actor) error { _, err := analytics.Overview(ctx, a); return err },
	}
}

// Every method refuses before touching a port: the mocks have no
// expectations, so any call fails the test.
func TestServicesRefuseUnauthorizedActors(t *testing.T) {
	t.Parallel()
	policy := services.Policy{RequireTwoFactor: true}
	for name := range policyCalls(newPorts(t), policy) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			call := policyCalls(newPorts(t), policy)[name]            // mocks bound to this subtest
			wantErr(t, call(services.Actor{}), services.ErrForbidden) // no actor is nobody
			noTwoFactor := grace
			noTwoFactor.TwoFactorEnabled = false
			err := call(noTwoFactor)
			if name == "AdminService.Bootstrap" {
				wantErr(t, err, services.ErrForbidden) // only the operator bootstraps
				wantErr(t, call(grace), services.ErrForbidden)
				return
			}
			wantErr(t, err, services.ErrTwoFactorRequired)
		})
	}
}

// Without the 2FA requirement any admin may act, and the operator always may.
func TestPolicyAllowsAdminsAndSystem(t *testing.T) {
	t.Parallel()
	for _, policy := range []services.Policy{{}, {RequireTwoFactor: true}} {
		for _, actor := range []services.Actor{cli, {AdminID: "a2", Email: "bob@example.com"}} {
			if policy.RequireTwoFactor && !actor.System {
				continue // covered by TestServicesRefuseUnauthorizedActors
			}
			p := newPorts(t)
			p.analytics.EXPECT().Overview(ctx).Return(services.Overview{TotalUsers: 3}, nil)
			got, err := services.NewAnalyticsService(p.analytics, policy).Overview(ctx, actor)
			if err != nil || got.TotalUsers != 3 {
				t.Fatalf("policy %+v, actor %+v: Overview = %+v, %v", policy, actor, got, err)
			}
		}
	}
}

func TestSystemActor(t *testing.T) {
	t.Parallel()
	want := services.Actor{AdminID: "system:cli", Email: "cli", System: true}
	if cli != want {
		t.Fatalf("SystemActor = %+v, want %+v", cli, want)
	}
}

// Kind sentinels match every error of their kind; specific errors only themselves.
func TestErrorKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err, target error
		want        bool
	}{
		{services.ErrAdminExists, services.ErrConflict, true},
		{services.ErrRoleUnchanged, services.ErrValidation, true},
		{fmt.Errorf("wrapped: %w", services.ErrSessionNotFound), services.ErrNotFound, true},
		{services.ErrTwoFactorRequired, services.ErrForbidden, true},
		{services.ErrForbidden, services.ErrTwoFactorRequired, false},
		{services.ErrAdminExists, services.ErrNotFound, false},
		{services.ErrSessionNotFound, services.ErrUserNotFound, false},
		{services.ErrNotFound, services.ErrUserNotFound, false},
		{errDB, services.ErrNotFound, false},
	}
	for _, c := range cases {
		if got := errors.Is(c.err, c.target); got != c.want {
			t.Errorf("errors.Is(%v, %v) = %v, want %v", c.err, c.target, got, c.want)
		}
	}
	if services.KindOf(services.ErrAdminExists) != services.KindConflict || services.KindOf(errDB) != services.KindInternal {
		t.Error("KindOf")
	}
	if services.ErrUserNotFound.Error() != "User not found" {
		t.Errorf("Error() = %q", services.ErrUserNotFound.Error())
	}
}

// errResolve is a failed outcome write, distinct from the change's errDB.
var errResolve = errors.New("audit outcome write timed out")

// mutation is one audited admin mutation: the reads and rules that run before
// the intent is written, the change itself, and the service call.
type mutation struct {
	name    string
	intent  services.NewAuditEntry             // what Begin must receive
	reads   func(p ports) []any                // expectations before Begin
	change  func(p ports, portErr error) []any // the change port(s), failing with portErr (nil: applied)
	call    func(p ports) error
	refusal error // a service error the change port reports...
	want    error // ...and what the caller gets for it
}

func mutations() []mutation {
	policy := services.Policy{RequireTwoFactor: true}
	users := func(p ports) *services.UserService { return services.NewUserService(p.dir, p.log, policy) }
	admins := func(p ports) *services.AdminService { return services.NewAdminService(p.accounts, p.log, policy) }
	userDetail := func(u services.User) func(p ports) []any {
		return func(p ports) []any {
			return []any{p.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: u}, nil)}
		}
	}
	bannedAda := adaUser
	bannedAda.Banned = true
	userRefusal := fmt.Errorf("auth: %w", services.ErrUserNotFound)
	newBob := func(p ports) []any {
		return []any{p.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)}
	}
	emailIntent := func(actor services.Actor, action string, details map[string]string) services.NewAuditEntry {
		return services.NewAuditEntry{
			ActorID: actor.AdminID, ActorEmail: actor.Email, Action: action,
			TargetType: "admin", TargetID: "bob@example.com", TargetLabel: "bob@example.com", Details: details,
		}
	}

	return []mutation{
		{
			name:   "BanUser",
			intent: userAudit("user.ban", map[string]string{"reason": "spam", "expiresIn": "permanent"}),
			reads:  userDetail(adaUser),
			change: func(p ports, err error) []any {
				return []any{p.dir.EXPECT().BanUser(ctx, "u1", "spam", time.Duration(0)).Return(err)}
			},
			call:    func(p ports) error { return users(p).BanUser(ctx, grace, "u1", validate.BanUser{BanReason: "spam"}) },
			refusal: userRefusal, want: services.ErrUserNotFound,
		},
		{
			name:   "UnbanUser",
			intent: userAudit("user.unban", nil),
			reads:  userDetail(bannedAda),
			change: func(p ports, err error) []any {
				return []any{p.dir.EXPECT().UnbanUser(ctx, "u1").Return(err)}
			},
			call:    func(p ports) error { return users(p).UnbanUser(ctx, grace, "u1") },
			refusal: userRefusal, want: services.ErrUserNotFound,
		},
		{
			name:   "SetRole",
			intent: userAudit("user.set_role", map[string]string{"from": "user", "to": "admin"}),
			reads:  userDetail(adaUser),
			change: func(p ports, err error) []any {
				return []any{p.dir.EXPECT().SetRole(ctx, "u1", "admin").Return(err)}
			},
			call:    func(p ports) error { return users(p).SetRole(ctx, grace, "u1", validate.SetUserRole{Role: "admin"}) },
			refusal: fmt.Errorf("auth: %w", services.ErrInvalidRole), want: services.ErrInvalidRole,
		},
		{
			name:   "RevokeSession",
			intent: userAudit("user.revoke_session", map[string]string{"sessionId": "s9"}),
			reads:  userDetail(adaUser),
			change: func(p ports, err error) []any {
				return []any{p.dir.EXPECT().RevokeSession(ctx, "u1", "s9").Return(err)}
			},
			call:    func(p ports) error { return users(p).RevokeSession(ctx, grace, "u1", "s9") },
			refusal: fmt.Errorf("auth: %w", services.ErrSessionNotFound), want: services.ErrSessionNotFound,
		},
		{
			name:   "RevokeSessions",
			intent: userAudit("user.revoke_sessions", nil),
			reads:  userDetail(adaUser),
			change: func(p ports, err error) []any {
				return []any{p.dir.EXPECT().RevokeUserSessions(ctx, "u1").Return(err)}
			},
			call:    func(p ports) error { return users(p).RevokeSessions(ctx, grace, "u1") },
			refusal: userRefusal, want: services.ErrUserNotFound,
		},
		{
			// A first invite targets the email: the admin has no id yet.
			name:   "InviteAdmin/new",
			intent: emailIntent(grace, "admin.invite", map[string]string{}),
			reads:  newBob,
			change: func(p ports, err error) []any {
				if err != nil {
					return []any{p.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(services.Admin{}, err)}
				}
				return []any{
					p.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
					p.accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
				}
			},
			call: func(p ports) error {
				got, err := admins(p).InviteAdmin(ctx, grace, invite)
				if err == nil && got != pending {
					return fmt.Errorf("InviteAdmin = %+v", got)
				}
				return err
			},
			refusal: services.ErrAdminExists, want: services.ErrAdminExists, // created concurrently
		},
		{
			name:   "InviteAdmin/resend",
			intent: adminAudit(grace, "admin.invite", map[string]string{"resent": "true"}),
			reads: func(p ports) []any {
				return []any{
					p.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil),
					p.log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{Total: 1}, nil),
				}
			},
			change: func(p ports, err error) []any {
				return []any{p.accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(err)}
			},
			call: func(p ports) error {
				got, err := admins(p).InviteAdmin(ctx, grace, invite)
				if err == nil && got != pending {
					return fmt.Errorf("InviteAdmin = %+v", got)
				}
				return err
			},
			refusal: services.ErrAdminNotFound, want: services.ErrAdminNotFound,
		},
		{
			name:   "DisableAdmin",
			intent: adminAudit(grace, "admin.disable", nil),
			reads: func(p ports) []any {
				return []any{
					p.accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil),
					p.accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{graceAd, bob}, nil),
				}
			},
			change: func(p ports, err error) []any {
				return []any{p.accounts.EXPECT().DisableAdmin(ctx, "a2").Return(err)}
			},
			call:    func(p ports) error { return admins(p).DisableAdmin(ctx, grace, "a2") },
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound, // removed meanwhile
		},
		{
			name:   "EnableAdmin",
			intent: adminAudit(grace, "admin.enable", nil),
			reads: func(p ports) []any {
				return []any{p.accounts.EXPECT().AdminByID(ctx, "a2").Return(disBob, nil)}
			},
			change: func(p ports, err error) []any {
				return []any{p.accounts.EXPECT().EnableAdmin(ctx, "a2").Return(err)}
			},
			call:    func(p ports) error { return admins(p).EnableAdmin(ctx, grace, "a2") },
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound,
		},
		{
			name:   "RemoveAdmin",
			intent: adminAudit(grace, "admin.remove", nil),
			reads: func(p ports) []any {
				return []any{p.accounts.EXPECT().AdminByID(ctx, "a2").Return(pending, nil)}
			},
			change: func(p ports, err error) []any {
				return []any{p.accounts.EXPECT().RemoveAdmin(ctx, "a2").Return(err)}
			},
			call:    func(p ports) error { return admins(p).RemoveAdmin(ctx, grace, "a2") },
			refusal: fmt.Errorf("delete: %w", services.ErrRecordNotFound), want: services.ErrAdminNotFound,
		},
		{
			// Bootstrap targets the email, acting as the operator.
			name:   "Bootstrap",
			intent: emailIntent(cli, "admin.bootstrap", nil),
			reads:  newBob,
			change: func(p ports, err error) []any {
				if err != nil {
					return []any{p.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(services.Admin{}, err)}
				}
				return []any{p.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil)}
			},
			call: func(p ports) error {
				got, created, err := admins(p).Bootstrap(ctx, cli, invite)
				if err == nil && (got != pending || !created) {
					return fmt.Errorf("Bootstrap = %+v, %v", got, created)
				}
				if err != nil && created {
					return fmt.Errorf("created on error: %w", err)
				}
				return err
			},
			refusal: services.ErrAdminExists, want: services.ErrAdminExists,
		},
	}
}

// Every mutation fails closed: the intent is written before the change, a
// failed intent write stops the change (the change ports have no
// expectations, so calling one fails the test), and the outcome follows it.
func TestMutationsFailClosed(t *testing.T) {
	t.Parallel()
	for _, m := range mutations() {
		// sequence expects the reads, the intent, the change failing with
		// portErr and its outcome write (returning resolveErr), in order.
		sequence := func(p ports, portErr error, outcome services.AuditOutcome, resolveErr error) {
			gomock.InOrder(slices.Concat(
				m.reads(p),
				[]any{p.log.EXPECT().Begin(ctx, m.intent).Return(entryID, nil)},
				m.change(p, portErr),
				[]any{p.log.EXPECT().Resolve(ctx, entryID, outcome).Return(resolveErr)},
			)...)
		}

		t.Run(m.name+"/succeeded", func(t *testing.T) {
			t.Parallel()
			p := newPorts(t)
			sequence(p, nil, succeeded, nil)
			if err := m.call(p); err != nil {
				t.Fatal(err)
			}
		})

		t.Run(m.name+"/intent not written", func(t *testing.T) {
			t.Parallel()
			p := newPorts(t)
			gomock.InOrder(append(m.reads(p), p.log.EXPECT().Begin(ctx, m.intent).Return(int64(0), errDB))...)
			err := m.call(p)
			wantErr(t, err, services.ErrAuditUnavailable)
			if services.KindOf(err) != services.KindUnavailable || !errors.Is(err, errDB) {
				t.Fatalf("err = %v, want unavailable wrapping %v", err, errDB)
			}
		})

		t.Run(m.name+"/outcome not written", func(t *testing.T) {
			t.Parallel()
			p := newPorts(t)
			sequence(p, nil, succeeded, errResolve)
			err := m.call(p)
			wantInternal(t, err, errResolve)
			if !strings.Contains(err.Error(), "WAS applied") {
				t.Fatalf("err = %v, want it to say the change was applied", err)
			}
		})

		t.Run(m.name+"/refused", func(t *testing.T) {
			t.Parallel()
			p := newPorts(t)
			sequence(p, m.refusal, failed(m.want.Error()), nil)
			wantErr(t, m.call(p), m.want)
		})

		t.Run(m.name+"/refused, outcome not written", func(t *testing.T) {
			t.Parallel()
			p := newPorts(t)
			sequence(p, m.refusal, failed(m.want.Error()), errResolve)
			err := m.call(p)
			wantErr(t, err, m.want)
			if errors.Is(err, errResolve) {
				t.Fatalf("err = %v, want the change's own error", err)
			}
		})

		t.Run(m.name+"/raw error", func(t *testing.T) {
			t.Parallel()
			p := newPorts(t)
			sequence(p, errDB, internalFailure, nil)
			wantInternal(t, m.call(p), errDB)
		})
	}
}
