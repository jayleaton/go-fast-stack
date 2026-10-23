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

func ptr[Value any](value Value) *Value { return &value }

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

func newPorts(test *testing.T) ports {
	ctrl := gomock.NewController(test)
	return ports{
		dir:       mocks.NewMockUserDirectory(ctrl),
		accounts:  mocks.NewMockAdminAccounts(ctrl),
		log:       mocks.NewMockAuditLog(ctrl),
		analytics: mocks.NewMockAnalytics(ctrl),
	}
}

// wantErr fails unless errors.Is(err, target).
func wantErr(test *testing.T, err, target error) {
	test.Helper()
	if !errors.Is(err, target) {
		test.Fatalf("err = %v, want %v", err, target)
	}
}

// wantInternal fails unless err is cause passed through as an internal error.
func wantInternal(test *testing.T, err, cause error) {
	test.Helper()
	if services.KindOf(err) != services.KindInternal || !errors.Is(err, cause) {
		test.Fatalf("err = %v, want internal wrapping %v", err, cause)
	}
}

// policyCalls calls every method of every service with actor, over ports p.
func policyCalls(fakes ports, policy services.Policy) map[string]func(services.Actor) error {
	users := services.NewUserService(fakes.dir, fakes.log, policy)
	admins := services.NewAdminService(fakes.accounts, fakes.log, policy)
	auditSvc := services.NewAuditService(fakes.log, policy)
	analytics := services.NewAnalyticsService(fakes.analytics, policy)
	return map[string]func(services.Actor) error{
		"UserService.ListUsers": func(actor services.Actor) error {
			_, err := users.ListUsers(ctx, actor, validate.ListUsers{Limit: 25})
			return err
		},
		"UserService.GetUser": func(actor services.Actor) error { _, err := users.GetUser(ctx, actor, "u1"); return err },
		"UserService.BanUser": func(actor services.Actor) error {
			return users.BanUser(ctx, actor, "u1", validate.BanUser{BanReason: "spam"})
		},
		"UserService.UnbanUser": func(actor services.Actor) error { return users.UnbanUser(ctx, actor, "u1") },
		"UserService.SetRole": func(actor services.Actor) error {
			return users.SetRole(ctx, actor, "u1", validate.SetUserRole{Role: "admin"})
		},
		"UserService.RevokeSession":  func(actor services.Actor) error { return users.RevokeSession(ctx, actor, "u1", "s9") },
		"UserService.RevokeSessions": func(actor services.Actor) error { return users.RevokeSessions(ctx, actor, "u1") },
		"AdminService.ListAdmins":    func(actor services.Actor) error { _, err := admins.ListAdmins(ctx, actor); return err },
		"AdminService.InviteAdmin": func(actor services.Actor) error {
			_, err := admins.InviteAdmin(ctx, actor, invite)
			return err
		},
		"AdminService.DisableAdmin": func(actor services.Actor) error { return admins.DisableAdmin(ctx, actor, "a2") },
		"AdminService.EnableAdmin":  func(actor services.Actor) error { return admins.EnableAdmin(ctx, actor, "a2") },
		"AdminService.RemoveAdmin":  func(actor services.Actor) error { return admins.RemoveAdmin(ctx, actor, "a2") },
		"AdminService.Bootstrap": func(actor services.Actor) error {
			_, _, err := admins.Bootstrap(ctx, actor, invite)
			return err
		},
		"AdminService.SendPasswordReset": func(actor services.Actor) error { return admins.SendPasswordReset(ctx, actor, "a2") },
		"AdminService.ResetTwoFactor":    func(actor services.Actor) error { return admins.ResetTwoFactor(ctx, actor, "a2") },
		"AdminService.ResetTwoFactorByEmail": func(actor services.Actor) error {
			_, err := admins.ResetTwoFactorByEmail(ctx, actor, validate.ResetAdminTwoFactor{Email: "bob@example.com"})
			return err
		},
		"AdminService.InviteBootstrapAdmin": func(actor services.Actor) error {
			_, err := admins.InviteBootstrapAdmin(ctx, actor, pending, time.Hour)
			return err
		},
		"AuditService.List": func(actor services.Actor) error {
			_, err := auditSvc.List(ctx, actor, validate.ListAuditLog{Limit: 25})
			return err
		},
		"AnalyticsService.Overview": func(actor services.Actor) error { _, err := analytics.Overview(ctx, actor); return err },
	}
}

// operatorOnly are the methods only services.SystemActor (the CLI) may call.
var operatorOnly = map[string]bool{
	"AdminService.Bootstrap":             true,
	"AdminService.InviteBootstrapAdmin":  true,
	"AdminService.ResetTwoFactorByEmail": true,
}

// Every method refuses before touching a port: the mocks have no
// expectations, so any call fails the test.
func TestServicesRefuseUnauthorizedActors(test *testing.T) {
	test.Parallel()
	policy := services.Policy{RequireTwoFactor: true}
	for name := range policyCalls(newPorts(test), policy) {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			call := policyCalls(newPorts(test), policy)[name]            // mocks bound to this subtest
			wantErr(test, call(services.Actor{}), services.ErrForbidden) // no actor is nobody
			noTwoFactor := grace
			noTwoFactor.TwoFactorEnabled = false
			err := call(noTwoFactor)
			if operatorOnly[name] {
				wantErr(test, err, services.ErrForbidden) // only the operator calls these
				wantErr(test, call(grace), services.ErrForbidden)
				return
			}
			wantErr(test, err, services.ErrTwoFactorRequired)
		})
	}
}

// Without the 2FA requirement any admin may act, and the operator always may.
func TestPolicyAllowsAdminsAndSystem(test *testing.T) {
	test.Parallel()
	for _, policy := range []services.Policy{{}, {RequireTwoFactor: true}} {
		for _, actor := range []services.Actor{cli, {AdminID: "a2", Email: "bob@example.com"}} {
			if policy.RequireTwoFactor && !actor.System {
				continue // covered by TestServicesRefuseUnauthorizedActors
			}
			ports := newPorts(test)
			ports.analytics.EXPECT().Overview(ctx).Return(services.Overview{TotalUsers: 3}, nil)
			got, err := services.NewAnalyticsService(ports.analytics, policy).Overview(ctx, actor)
			if err != nil || got.TotalUsers != 3 {
				test.Fatalf("policy %+v, actor %+v: Overview = %+v, %v", policy, actor, got, err)
			}
		}
	}
}

func TestSystemActor(test *testing.T) {
	test.Parallel()
	want := services.Actor{AdminID: "system:cli", Email: "cli", System: true}
	if cli != want {
		test.Fatalf("SystemActor = %+v, want %+v", cli, want)
	}
}

// Kind sentinels match every error of their kind; specific errors only themselves.
func TestErrorKinds(test *testing.T) {
	test.Parallel()
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
	for _, testCase := range cases {
		if got := errors.Is(testCase.err, testCase.target); got != testCase.want {
			test.Errorf("errors.Is(%v, %v) = %v, want %v", testCase.err, testCase.target, got, testCase.want)
		}
	}
	if services.KindOf(services.ErrAdminExists) != services.KindConflict || services.KindOf(errDB) != services.KindInternal {
		test.Error("KindOf")
	}
	if services.ErrUserNotFound.Error() != "User not found" {
		test.Errorf("Error() = %q", services.ErrUserNotFound.Error())
	}
}

// errResolve is a failed outcome write, distinct from the change's errDB.
var errResolve = errors.New("audit outcome write timed out")

// mutation is one audited admin mutation: the reads and rules that run before
// the intent is written, the change itself, and the service call.
type mutation struct {
	name    string
	intent  services.NewAuditEntry                     // what Begin must receive
	reads   func(portMocks ports) []any                // expectations before Begin
	change  func(portMocks ports, portErr error) []any // the change port(s), failing with portErr (nil: applied)
	call    func(portMocks ports) error
	refusal error // a service error the change port reports...
	want    error // ...and what the caller gets for it
}

func mutations() []mutation {
	policy := services.Policy{RequireTwoFactor: true}
	users := func(fakes ports) *services.UserService { return services.NewUserService(fakes.dir, fakes.log, policy) }
	admins := func(fakes ports) *services.AdminService {
		return services.NewAdminService(fakes.accounts, fakes.log, policy)
	}
	userDetail := func(account services.User) func(fakes ports) []any {
		return func(fakes ports) []any {
			return []any{fakes.dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: account}, nil)}
		}
	}
	bannedAda := adaUser
	bannedAda.Banned = true
	userRefusal := fmt.Errorf("auth: %w", services.ErrUserNotFound)
	newBob := func(fakes ports) []any {
		return []any{fakes.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(services.Admin{}, services.ErrRecordNotFound)}
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
			change: func(fakes ports, err error) []any {
				return []any{fakes.dir.EXPECT().BanUser(ctx, "u1", "spam", time.Duration(0)).Return(err)}
			},
			call: func(fakes ports) error {
				return users(fakes).BanUser(ctx, grace, "u1", validate.BanUser{BanReason: "spam"})
			},
			refusal: userRefusal, want: services.ErrUserNotFound,
		},
		{
			name:   "UnbanUser",
			intent: userAudit("user.unban", nil),
			reads:  userDetail(bannedAda),
			change: func(fakes ports, err error) []any {
				return []any{fakes.dir.EXPECT().UnbanUser(ctx, "u1").Return(err)}
			},
			call:    func(fakes ports) error { return users(fakes).UnbanUser(ctx, grace, "u1") },
			refusal: userRefusal, want: services.ErrUserNotFound,
		},
		{
			name:   "SetRole",
			intent: userAudit("user.set_role", map[string]string{"from": "user", "to": "admin"}),
			reads:  userDetail(adaUser),
			change: func(fakes ports, err error) []any {
				return []any{fakes.dir.EXPECT().SetRole(ctx, "u1", "admin").Return(err)}
			},
			call: func(fakes ports) error {
				return users(fakes).SetRole(ctx, grace, "u1", validate.SetUserRole{Role: "admin"})
			},
			refusal: fmt.Errorf("auth: %w", services.ErrInvalidRole), want: services.ErrInvalidRole,
		},
		{
			name:   "RevokeSession",
			intent: userAudit("user.revoke_session", map[string]string{"sessionId": "s9"}),
			reads:  userDetail(adaUser),
			change: func(fakes ports, err error) []any {
				return []any{fakes.dir.EXPECT().RevokeSession(ctx, "u1", "s9").Return(err)}
			},
			call:    func(fakes ports) error { return users(fakes).RevokeSession(ctx, grace, "u1", "s9") },
			refusal: fmt.Errorf("auth: %w", services.ErrSessionNotFound), want: services.ErrSessionNotFound,
		},
		{
			name:   "RevokeSessions",
			intent: userAudit("user.revoke_sessions", nil),
			reads:  userDetail(adaUser),
			change: func(fakes ports, err error) []any {
				return []any{fakes.dir.EXPECT().RevokeUserSessions(ctx, "u1").Return(err)}
			},
			call:    func(fakes ports) error { return users(fakes).RevokeSessions(ctx, grace, "u1") },
			refusal: userRefusal, want: services.ErrUserNotFound,
		},
		{
			// A first invite targets the email: the admin has no id yet.
			name:   "InviteAdmin/new",
			intent: emailIntent(grace, "admin.invite", map[string]string{}),
			reads:  newBob,
			change: func(fakes ports, err error) []any {
				if err != nil {
					return []any{fakes.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(services.Admin{}, err)}
				}
				return []any{
					fakes.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil),
					fakes.accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(nil),
				}
			},
			call: func(fakes ports) error {
				got, err := admins(fakes).InviteAdmin(ctx, grace, invite)
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
			reads: func(fakes ports) []any {
				return []any{
					fakes.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(pending, nil),
					fakes.log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{Total: 1}, nil),
				}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(err)}
			},
			call: func(fakes ports) error {
				got, err := admins(fakes).InviteAdmin(ctx, grace, invite)
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
			reads: func(fakes ports) []any {
				return []any{
					fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil),
					fakes.accounts.EXPECT().ListAdmins(ctx).Return([]services.Admin{graceAd, bob}, nil),
				}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().DisableAdmin(ctx, "a2").Return(err)}
			},
			call:    func(fakes ports) error { return admins(fakes).DisableAdmin(ctx, grace, "a2") },
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound, // removed meanwhile
		},
		{
			name:   "EnableAdmin",
			intent: adminAudit(grace, "admin.enable", nil),
			reads: func(fakes ports) []any {
				return []any{fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(disBob, nil)}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().EnableAdmin(ctx, "a2").Return(err)}
			},
			call:    func(fakes ports) error { return admins(fakes).EnableAdmin(ctx, grace, "a2") },
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound,
		},
		{
			name:   "RemoveAdmin",
			intent: adminAudit(grace, "admin.remove", nil),
			reads: func(fakes ports) []any {
				return []any{fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(pending, nil)}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().RemoveAdmin(ctx, "a2").Return(err)}
			},
			call:    func(fakes ports) error { return admins(fakes).RemoveAdmin(ctx, grace, "a2") },
			refusal: fmt.Errorf("delete: %w", services.ErrRecordNotFound), want: services.ErrAdminNotFound,
		},
		{
			name:   "SendPasswordReset",
			intent: adminAudit(grace, "admin.password_reset", nil),
			reads: func(fakes ports) []any {
				return []any{fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(bob, nil)}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(err)}
			},
			call:    func(fakes ports) error { return admins(fakes).SendPasswordReset(ctx, grace, "a2") },
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound, // removed meanwhile
		},
		{
			name:   "ResetTwoFactor",
			intent: adminAudit(grace, "admin.reset_two_factor", nil),
			reads: func(fakes ports) []any {
				return []any{fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(bob2fa, nil)}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().ResetTwoFactor(ctx, "a2").Return(err)}
			},
			call:    func(fakes ports) error { return admins(fakes).ResetTwoFactor(ctx, grace, "a2") },
			refusal: fmt.Errorf("reset: %w", services.ErrRecordNotFound), want: services.ErrAdminNotFound,
		},
		{
			name:   "ResetTwoFactorByEmail",
			intent: adminAudit(cli, "admin.reset_two_factor", nil),
			reads: func(fakes ports) []any {
				return []any{
					fakes.accounts.EXPECT().AdminByEmail(ctx, "bob@example.com").Return(bob2fa, nil),
					fakes.accounts.EXPECT().AdminByID(ctx, "a2").Return(bob2fa, nil),
				}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().ResetTwoFactor(ctx, "a2").Return(err)}
			},
			call: func(fakes ports) error {
				_, err := admins(fakes).ResetTwoFactorByEmail(ctx, cli, validate.ResetAdminTwoFactor{Email: "bob@example.com"})
				return err
			},
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound,
		},
		{
			name:   "InviteBootstrapAdmin",
			intent: adminAudit(cli, "admin.invite", map[string]string{}),
			reads: func(fakes ports) []any {
				return []any{
					fakes.log.EXPECT().List(ctx, priorInvite).Return(services.AuditPage{}, nil),
					fakes.log.EXPECT().List(ctx, priorEmailInvite).Return(services.AuditPage{}, nil),
				}
			},
			change: func(fakes ports, err error) []any {
				return []any{fakes.accounts.EXPECT().SendSetPasswordLink(ctx, "a2").Return(err)}
			},
			call: func(fakes ports) error {
				sent, err := admins(fakes).InviteBootstrapAdmin(ctx, cli, pending, time.Hour)
				if sent != (err == nil) {
					return fmt.Errorf("sent = %v with err %w", sent, err)
				}
				return err
			},
			refusal: services.ErrRecordNotFound, want: services.ErrAdminNotFound,
		},
		{
			// Bootstrap targets the email, acting as the operator.
			name:   "Bootstrap",
			intent: emailIntent(cli, "admin.bootstrap", nil),
			reads:  newBob,
			change: func(fakes ports, err error) []any {
				if err != nil {
					return []any{fakes.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(services.Admin{}, err)}
				}
				return []any{fakes.accounts.EXPECT().CreateAdmin(ctx, "bob@example.com", "Bob").Return(pending, nil)}
			},
			call: func(fakes ports) error {
				got, created, err := admins(fakes).Bootstrap(ctx, cli, invite)
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
func TestMutationsFailClosed(test *testing.T) {
	test.Parallel()
	for _, mutation := range mutations() {
		// sequence expects the reads, the intent, the change failing with
		// portErr and its outcome write (returning resolveErr), in order.
		sequence := func(ports ports, portErr error, outcome services.AuditOutcome, resolveErr error) {
			gomock.InOrder(slices.Concat(
				mutation.reads(ports),
				[]any{ports.log.EXPECT().Begin(ctx, mutation.intent).Return(entryID, nil)},
				mutation.change(ports, portErr),
				[]any{ports.log.EXPECT().Resolve(ctx, entryID, outcome).Return(resolveErr)},
			)...)
		}

		test.Run(mutation.name+"/succeeded", func(test *testing.T) {
			test.Parallel()
			ports := newPorts(test)
			sequence(ports, nil, succeeded, nil)
			if err := mutation.call(ports); err != nil {
				test.Fatal(err)
			}
		})

		test.Run(mutation.name+"/intent not written", func(test *testing.T) {
			test.Parallel()
			ports := newPorts(test)
			gomock.InOrder(append(mutation.reads(ports), ports.log.EXPECT().Begin(ctx, mutation.intent).Return(int64(0), errDB))...)
			err := mutation.call(ports)
			wantErr(test, err, services.ErrAuditUnavailable)
			if services.KindOf(err) != services.KindUnavailable || !errors.Is(err, errDB) {
				test.Fatalf("err = %v, want unavailable wrapping %v", err, errDB)
			}
		})

		test.Run(mutation.name+"/outcome not written", func(test *testing.T) {
			test.Parallel()
			ports := newPorts(test)
			sequence(ports, nil, succeeded, errResolve)
			err := mutation.call(ports)
			wantInternal(test, err, errResolve)
			if !strings.Contains(err.Error(), "WAS applied") {
				test.Fatalf("err = %v, want it to say the change was applied", err)
			}
		})

		test.Run(mutation.name+"/refused", func(test *testing.T) {
			test.Parallel()
			ports := newPorts(test)
			sequence(ports, mutation.refusal, failed(mutation.want.Error()), nil)
			wantErr(test, mutation.call(ports), mutation.want)
		})

		test.Run(mutation.name+"/refused, outcome not written", func(test *testing.T) {
			test.Parallel()
			ports := newPorts(test)
			sequence(ports, mutation.refusal, failed(mutation.want.Error()), errResolve)
			err := mutation.call(ports)
			wantErr(test, err, mutation.want)
			if errors.Is(err, errResolve) {
				test.Fatalf("err = %v, want the change's own error", err)
			}
		})

		test.Run(mutation.name+"/raw error", func(test *testing.T) {
			test.Parallel()
			ports := newPorts(test)
			sequence(ports, errDB, internalFailure, nil)
			wantInternal(test, mutation.call(ports), errDB)
		})
	}
}
