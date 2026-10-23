package services_test

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services/mocks"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

func newUserService(test *testing.T) (*services.UserService, *mocks.MockUserDirectory, *mocks.MockAuditLog) {
	ports := newPorts(test)
	return services.NewUserService(ports.dir, ports.log, services.Policy{RequireTwoFactor: true}), ports.dir, ports.log
}

// userAudit is the entry grace's action on ada writes.
func userAudit(action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: "a1", ActorEmail: "grace@example.com", Action: action,
		TargetType: "user", TargetID: "u1", TargetLabel: "ada@example.com", Details: details,
	}
}

func TestListUsers(test *testing.T) {
	test.Parallel()
	svc, dir, _ := newUserService(test)
	in := validate.ListUsers{
		Search: "ada", SearchField: "name", Role: "admin", Banned: ptr(true),
		SortBy: "email", SortDirection: "desc", Limit: 10, Offset: 20,
	}
	want := services.UserPage{Users: []services.User{adaUser}, Total: 21, Limit: 10, Offset: 20}

	dir.EXPECT().ListUsers(ctx, services.UserQuery{
		Search: "ada", SearchField: "name", Role: "admin", Banned: in.Banned,
		SortBy: "email", SortDesc: true, Limit: 10, Offset: 20,
	}).Return(want, nil)
	if got, err := svc.ListUsers(ctx, grace, in); err != nil || len(got.Users) != 1 || got.Total != 21 {
		test.Fatalf("ListUsers = %+v, %v", got, err)
	}

	in.SortDirection = "asc"
	dir.EXPECT().ListUsers(ctx, gomock.Cond(func(query services.UserQuery) bool { return !query.SortDesc })).Return(services.UserPage{}, nil)
	if got, err := svc.ListUsers(ctx, grace, in); err != nil || got.Users == nil {
		test.Fatalf("empty page = %#v, %v; want an empty, non-nil list", got, err)
	}

	dir.EXPECT().ListUsers(ctx, gomock.Any()).Return(services.UserPage{}, errDB)
	_, err := svc.ListUsers(ctx, grace, in)
	wantInternal(test, err, errDB)
}

func TestGetUser(test *testing.T) {
	test.Parallel()
	svc, dir, _ := newUserService(test)
	sessions := []services.Session{{ID: "s1"}}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser, Sessions: sessions}, nil)
	if got, err := svc.GetUser(ctx, grace, "u1"); err != nil || got.User != adaUser || len(got.Sessions) != 1 {
		test.Fatalf("GetUser = %+v, %v", got, err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser}, nil)
	if got, err := svc.GetUser(ctx, grace, "u1"); err != nil || got.Sessions == nil {
		test.Fatalf("no sessions = %#v, %v; want an empty, non-nil list", got, err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	_, err := svc.GetUser(ctx, grace, "u1")
	wantErr(test, err, services.ErrUserNotFound)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errDB)
	_, err = svc.GetUser(ctx, grace, "u1")
	wantInternal(test, err, errDB)
}

// The generic fail-closed cases (intent or outcome not written, a refused or
// failing change) of every mutation are in TestMutationsFailClosed.

func TestBanUser(test *testing.T) {
	test.Parallel()
	svc, dir, log := newUserService(test)
	detail := services.UserDetail{User: adaUser}

	// No expiry is a permanent ban; 0 is permanent too.
	for _, expiresIn := range []*int64{nil, ptr[int64](0)} {
		gomock.InOrder(
			dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
			log.EXPECT().Begin(ctx, userAudit("user.ban", map[string]string{"reason": "spam", "expiresIn": "permanent"})).Return(entryID, nil),
			dir.EXPECT().BanUser(ctx, "u1", "spam", time.Duration(0)).Return(nil),
			log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
		)
		if err := svc.BanUser(ctx, grace, "u1", validate.BanUser{BanReason: "spam", BanExpiresIn: expiresIn}); err != nil {
			test.Fatal(err)
		}
	}

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.ban", map[string]string{"reason": "spam", "expiresIn": "1h0m0s"})).Return(entryID, nil),
		dir.EXPECT().BanUser(ctx, "u1", "spam", time.Hour).Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.BanUser(ctx, grace, "u1", validate.BanUser{BanReason: "spam", BanExpiresIn: ptr[int64](3600)}); err != nil {
		test.Fatal(err)
	}

	in := validate.BanUser{BanReason: "spam"}

	// A missing user is a 404 before anything is written or changed.
	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	wantErr(test, svc.BanUser(ctx, grace, "u1", in), services.ErrUserNotFound)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errDB)
	wantInternal(test, svc.BanUser(ctx, grace, "u1", in), errDB)
}

func TestUnbanUser(test *testing.T) {
	test.Parallel()
	svc, dir, log := newUserService(test)
	banned := adaUser
	banned.Banned = true

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: banned}, nil),
		log.EXPECT().Begin(ctx, userAudit("user.unban", nil)).Return(entryID, nil),
		dir.EXPECT().UnbanUser(ctx, "u1").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.UnbanUser(ctx, grace, "u1"); err != nil {
		test.Fatal(err)
	}

	// Rules and lookups write nothing.
	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser}, nil)
	wantErr(test, svc.UnbanUser(ctx, grace, "u1"), services.ErrUserNotBanned)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(test, svc.UnbanUser(ctx, grace, "u1"), services.ErrUserNotFound)
}

func TestSetRole(test *testing.T) {
	test.Parallel()
	svc, dir, log := newUserService(test)
	detail := services.UserDetail{User: adaUser}
	in := validate.SetUserRole{Role: "admin"}

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.set_role", map[string]string{"from": "user", "to": "admin"})).Return(entryID, nil),
		dir.EXPECT().SetRole(ctx, "u1", "admin").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.SetRole(ctx, grace, "u1", in); err != nil {
		test.Fatal(err)
	}

	// An unknown role is refused before any port call.
	wantErr(test, svc.SetRole(ctx, grace, "u1", validate.SetUserRole{Role: "superuser"}), services.ErrInvalidRole)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil)
	wantErr(test, svc.SetRole(ctx, grace, "u1", validate.SetUserRole{Role: "user"}), services.ErrRoleUnchanged)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(test, svc.SetRole(ctx, grace, "u1", in), services.ErrUserNotFound)

	// Each refusal auth reports is recorded with its message.
	for _, portErr := range []*services.Error{services.ErrInvalidRole, services.ErrUserNotFound} {
		gomock.InOrder(
			dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
			log.EXPECT().Begin(ctx, gomock.Any()).Return(entryID, nil),
			dir.EXPECT().SetRole(ctx, "u1", "admin").Return(fmt.Errorf("auth: %w", portErr)),
			log.EXPECT().Resolve(ctx, entryID, failed(portErr.Message)).Return(nil),
		)
		wantErr(test, svc.SetRole(ctx, grace, "u1", in), portErr)
	}
}

func TestRevokeSession(test *testing.T) {
	test.Parallel()
	svc, dir, log := newUserService(test)
	detail := services.UserDetail{User: adaUser}

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.revoke_session", map[string]string{"sessionId": "s9"})).Return(entryID, nil),
		dir.EXPECT().RevokeSession(ctx, "u1", "s9").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.RevokeSession(ctx, grace, "u1", "s9"); err != nil {
		test.Fatal(err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(test, svc.RevokeSession(ctx, grace, "u1", "s9"), services.ErrUserNotFound)

	// Another user's session is not found, and the attempt is recorded as failed.
	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.revoke_session", map[string]string{"sessionId": "other-users-session"})).Return(entryID, nil),
		dir.EXPECT().RevokeSession(ctx, "u1", "other-users-session").Return(fmt.Errorf("auth: %w", services.ErrSessionNotFound)),
		log.EXPECT().Resolve(ctx, entryID, failed("Session not found")).Return(nil),
	)
	wantErr(test, svc.RevokeSession(ctx, grace, "u1", "other-users-session"), services.ErrSessionNotFound)
}

func TestRevokeSessions(test *testing.T) {
	test.Parallel()
	svc, dir, log := newUserService(test)

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser}, nil),
		log.EXPECT().Begin(ctx, userAudit("user.revoke_sessions", nil)).Return(entryID, nil),
		dir.EXPECT().RevokeUserSessions(ctx, "u1").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.RevokeSessions(ctx, grace, "u1"); err != nil {
		test.Fatal(err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(test, svc.RevokeSessions(ctx, grace, "u1"), services.ErrUserNotFound)
}
