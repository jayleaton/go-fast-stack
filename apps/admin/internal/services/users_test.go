package services_test

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services/mocks"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

func newUserService(t *testing.T) (*services.UserService, *mocks.MockUserDirectory, *mocks.MockAuditLog) {
	p := newPorts(t)
	return services.NewUserService(p.dir, p.log, services.Policy{RequireTwoFactor: true}), p.dir, p.log
}

// userAudit is the entry grace's action on ada writes.
func userAudit(action string, details map[string]string) services.NewAuditEntry {
	return services.NewAuditEntry{
		ActorID: "a1", ActorEmail: "grace@example.com", Action: action,
		TargetType: "user", TargetID: "u1", TargetLabel: "ada@example.com", Details: details,
	}
}

func TestListUsers(t *testing.T) {
	svc, dir, _ := newUserService(t)
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
		t.Fatalf("ListUsers = %+v, %v", got, err)
	}

	in.SortDirection = "asc"
	dir.EXPECT().ListUsers(ctx, gomock.Cond(func(q services.UserQuery) bool { return !q.SortDesc })).Return(services.UserPage{}, nil)
	if got, err := svc.ListUsers(ctx, grace, in); err != nil || got.Users == nil {
		t.Fatalf("empty page = %#v, %v; want an empty, non-nil list", got, err)
	}

	dir.EXPECT().ListUsers(ctx, gomock.Any()).Return(services.UserPage{}, errDB)
	_, err := svc.ListUsers(ctx, grace, in)
	wantInternal(t, err, errDB)
}

func TestGetUser(t *testing.T) {
	svc, dir, _ := newUserService(t)
	sessions := []services.Session{{ID: "s1"}}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser, Sessions: sessions}, nil)
	if got, err := svc.GetUser(ctx, grace, "u1"); err != nil || got.User != adaUser || len(got.Sessions) != 1 {
		t.Fatalf("GetUser = %+v, %v", got, err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser}, nil)
	if got, err := svc.GetUser(ctx, grace, "u1"); err != nil || got.Sessions == nil {
		t.Fatalf("no sessions = %#v, %v; want an empty, non-nil list", got, err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	_, err := svc.GetUser(ctx, grace, "u1")
	wantErr(t, err, services.ErrUserNotFound)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errDB)
	_, err = svc.GetUser(ctx, grace, "u1")
	wantInternal(t, err, errDB)
}

// The generic fail-closed cases (intent or outcome not written, a refused or
// failing change) of every mutation are in TestMutationsFailClosed.

func TestBanUser(t *testing.T) {
	svc, dir, log := newUserService(t)
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
			t.Fatal(err)
		}
	}

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.ban", map[string]string{"reason": "spam", "expiresIn": "1h0m0s"})).Return(entryID, nil),
		dir.EXPECT().BanUser(ctx, "u1", "spam", time.Hour).Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.BanUser(ctx, grace, "u1", validate.BanUser{BanReason: "spam", BanExpiresIn: ptr[int64](3600)}); err != nil {
		t.Fatal(err)
	}

	in := validate.BanUser{BanReason: "spam"}

	// A missing user is a 404 before anything is written or changed.
	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	wantErr(t, svc.BanUser(ctx, grace, "u1", in), services.ErrUserNotFound)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, errDB)
	wantInternal(t, svc.BanUser(ctx, grace, "u1", in), errDB)
}

func TestUnbanUser(t *testing.T) {
	svc, dir, log := newUserService(t)
	banned := adaUser
	banned.Banned = true

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: banned}, nil),
		log.EXPECT().Begin(ctx, userAudit("user.unban", nil)).Return(entryID, nil),
		dir.EXPECT().UnbanUser(ctx, "u1").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.UnbanUser(ctx, grace, "u1"); err != nil {
		t.Fatal(err)
	}

	// Rules and lookups write nothing.
	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser}, nil)
	wantErr(t, svc.UnbanUser(ctx, grace, "u1"), services.ErrUserNotBanned)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(t, svc.UnbanUser(ctx, grace, "u1"), services.ErrUserNotFound)
}

func TestSetRole(t *testing.T) {
	svc, dir, log := newUserService(t)
	detail := services.UserDetail{User: adaUser}
	in := validate.SetUserRole{Role: "admin"}

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.set_role", map[string]string{"from": "user", "to": "admin"})).Return(entryID, nil),
		dir.EXPECT().SetRole(ctx, "u1", "admin").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.SetRole(ctx, grace, "u1", in); err != nil {
		t.Fatal(err)
	}

	// An unknown role is refused before any port call.
	wantErr(t, svc.SetRole(ctx, grace, "u1", validate.SetUserRole{Role: "superuser"}), services.ErrInvalidRole)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil)
	wantErr(t, svc.SetRole(ctx, grace, "u1", validate.SetUserRole{Role: "user"}), services.ErrRoleUnchanged)

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(t, svc.SetRole(ctx, grace, "u1", in), services.ErrUserNotFound)

	// Each refusal auth reports is recorded with its message.
	for _, portErr := range []*services.Error{services.ErrInvalidRole, services.ErrUserNotFound} {
		gomock.InOrder(
			dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
			log.EXPECT().Begin(ctx, gomock.Any()).Return(entryID, nil),
			dir.EXPECT().SetRole(ctx, "u1", "admin").Return(fmt.Errorf("auth: %w", portErr)),
			log.EXPECT().Resolve(ctx, entryID, failed(portErr.Message)).Return(nil),
		)
		wantErr(t, svc.SetRole(ctx, grace, "u1", in), portErr)
	}
}

func TestRevokeSession(t *testing.T) {
	svc, dir, log := newUserService(t)
	detail := services.UserDetail{User: adaUser}

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.revoke_session", map[string]string{"sessionId": "s9"})).Return(entryID, nil),
		dir.EXPECT().RevokeSession(ctx, "u1", "s9").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.RevokeSession(ctx, grace, "u1", "s9"); err != nil {
		t.Fatal(err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(t, svc.RevokeSession(ctx, grace, "u1", "s9"), services.ErrUserNotFound)

	// Another user's session is not found, and the attempt is recorded as failed.
	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(detail, nil),
		log.EXPECT().Begin(ctx, userAudit("user.revoke_session", map[string]string{"sessionId": "other-users-session"})).Return(entryID, nil),
		dir.EXPECT().RevokeSession(ctx, "u1", "other-users-session").Return(fmt.Errorf("auth: %w", services.ErrSessionNotFound)),
		log.EXPECT().Resolve(ctx, entryID, failed("Session not found")).Return(nil),
	)
	wantErr(t, svc.RevokeSession(ctx, grace, "u1", "other-users-session"), services.ErrSessionNotFound)
}

func TestRevokeSessions(t *testing.T) {
	svc, dir, log := newUserService(t)

	gomock.InOrder(
		dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{User: adaUser}, nil),
		log.EXPECT().Begin(ctx, userAudit("user.revoke_sessions", nil)).Return(entryID, nil),
		dir.EXPECT().RevokeUserSessions(ctx, "u1").Return(nil),
		log.EXPECT().Resolve(ctx, entryID, succeeded).Return(nil),
	)
	if err := svc.RevokeSessions(ctx, grace, "u1"); err != nil {
		t.Fatal(err)
	}

	dir.EXPECT().GetUserDetail(ctx, "u1").Return(services.UserDetail{}, services.ErrUserNotFound)
	wantErr(t, svc.RevokeSessions(ctx, grace, "u1"), services.ErrUserNotFound)
}
