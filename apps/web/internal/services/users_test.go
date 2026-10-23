package services_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services/mocks"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/validate"
)

var (
	ctx     = context.Background()
	ada     = services.Actor{UserID: "u1", Email: "ada@example.com", SessionID: "s1"}
	adaUser = services.User{ID: "u1", Name: "Ada", Email: "ada@example.com"}
	errDB   = errors.New("connection refused")
)

func ptr[Value any](value Value) *Value { return &value }

func newService(test *testing.T) (*services.UserService, *mocks.MockUserRepository, *mocks.MockAccountCredentials) {
	ctrl := gomock.NewController(test)
	repo, creds := mocks.NewMockUserRepository(ctrl), mocks.NewMockAccountCredentials(ctrl)
	return services.NewUserService(repo, creds), repo, creds
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

// Every method refuses to act on another user before touching a port: the
// mocks have no expectations, so any call fails the test.
func TestUserServiceForbidsOtherUsers(test *testing.T) {
	test.Parallel()
	type service = *services.UserService
	calls := map[string]func(service, services.Actor, string) error{
		"GetProfile": func(svc service, actor services.Actor, id string) error {
			_, err := svc.GetProfile(ctx, actor, id)
			return err
		},
		"UpdateProfile": func(svc service, actor services.Actor, id string) error {
			_, err := svc.UpdateProfile(ctx, actor, id, validate.UpdateUserSettings{Name: ptr("Eve")})
			return err
		},
		"RequestEmailChange": func(svc service, actor services.Actor, id string) error {
			return svc.RequestEmailChange(ctx, actor, id, validate.UpdateUserEmail{NewEmail: "x@example.com"})
		},
		"ChangePassword": func(svc service, actor services.Actor, id string) error {
			return svc.ChangePassword(ctx, actor, id, validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456"})
		},
		"ListSessions": func(svc service, actor services.Actor, id string) error {
			_, err := svc.ListSessions(ctx, actor, id)
			return err
		},
		"RevokeSession": func(svc service, actor services.Actor, id string) error {
			return svc.RevokeSession(ctx, actor, id, "s9")
		},
		"RevokeOtherSessions": func(svc service, actor services.Actor, id string) error {
			return svc.RevokeOtherSessions(ctx, actor, id)
		},
		"DeleteAccount": func(svc service, actor services.Actor, id string) error { return svc.DeleteAccount(ctx, actor, id) },
		"DataSummary": func(svc service, actor services.Actor, id string) error {
			_, err := svc.DataSummary(ctx, actor, id)
			return err
		},
	}
	for name, call := range calls {
		test.Run(name, func(test *testing.T) {
			test.Parallel()
			svc, _, _ := newService(test) // its own mocks: an unexpected call fails this subtest
			wantErr(test, call(svc, ada, "u2"), services.ErrForbidden)
			wantErr(test, call(svc, services.Actor{}, ""), services.ErrForbidden) // no actor is nobody
		})
	}
}

func TestGetProfile(test *testing.T) {
	test.Parallel()
	svc, repo, _ := newService(test)
	repo.EXPECT().GetByID(ctx, "u1").Return(adaUser, nil)
	if user, err := svc.GetProfile(ctx, ada, "u1"); err != nil || user != adaUser {
		test.Fatalf("GetProfile = %+v, %v", user, err)
	}

	repo.EXPECT().GetByID(ctx, "u1").Return(services.User{}, fmt.Errorf("get: %w", services.ErrRecordNotFound))
	_, err := svc.GetProfile(ctx, ada, "u1")
	wantErr(test, err, services.ErrUserNotFound)

	repo.EXPECT().GetByID(ctx, "u1").Return(services.User{}, errDB)
	_, err = svc.GetProfile(ctx, ada, "u1")
	wantInternal(test, err, errDB)
}

func TestUpdateProfile(test *testing.T) {
	test.Parallel()
	svc, repo, _ := newService(test)
	renamed := adaUser
	renamed.Name = "Ada L"

	repo.EXPECT().UpdateProfile(ctx, "u1", services.UserProfileUpdate{Name: ptr("Ada L")}).Return(renamed, nil)
	if user, err := svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{Name: ptr("Ada L")}); err != nil || user.Name != "Ada L" {
		test.Fatalf("rename = %+v, %v", user, err)
	}

	// An explicit null image clears it.
	repo.EXPECT().UpdateProfile(ctx, "u1", services.UserProfileUpdate{SetImage: true}).Return(adaUser, nil)
	if _, err := svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{Image: validate.Nullable[string]{Set: true}}); err != nil {
		test.Fatal(err)
	}

	repo.EXPECT().UpdateProfile(ctx, "u1", gomock.Any()).Return(services.User{}, services.ErrRecordNotFound)
	_, err := svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{})
	wantErr(test, err, services.ErrUserNotFound)

	repo.EXPECT().UpdateProfile(ctx, "u1", gomock.Any()).Return(services.User{}, errDB)
	_, err = svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{})
	wantInternal(test, err, errDB)
}

func TestRequestEmailChange(test *testing.T) {
	test.Parallel()
	svc, _, creds := newService(test)
	in := validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: "/dashboard"}

	creds.EXPECT().ChangeEmail(ctx, "u1", "ada@new.example", "/dashboard").Return(nil)
	if err := svc.RequestEmailChange(ctx, ada, "u1", in); err != nil {
		test.Fatal(err)
	}

	err := svc.RequestEmailChange(ctx, ada, "u1", validate.UpdateUserEmail{NewEmail: "ADA@example.com"})
	wantErr(test, err, services.ErrSameEmail)

	// Port failures keep their identity through the service (the UI translates per error).
	for _, portErr := range []error{services.ErrEmailTaken, services.ErrInvalidCallbackURL, services.ErrUserNotFound} {
		creds.EXPECT().ChangeEmail(ctx, "u1", gomock.Any(), gomock.Any()).Return(fmt.Errorf("auth: %w", portErr))
		wantErr(test, svc.RequestEmailChange(ctx, ada, "u1", in), portErr)
	}

	creds.EXPECT().ChangeEmail(ctx, "u1", gomock.Any(), gomock.Any()).Return(errDB)
	wantInternal(test, svc.RequestEmailChange(ctx, ada, "u1", in), errDB)
}

func TestChangePassword(test *testing.T) {
	test.Parallel()
	svc, _, creds := newService(test)
	in := validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456", RevokeOtherSessions: true}

	// The actor's session survives a revoke-others password change.
	creds.EXPECT().ChangePassword(ctx, "u1", "password123", "password456", true, "s1").Return(nil)
	if err := svc.ChangePassword(ctx, ada, "u1", in); err != nil {
		test.Fatal(err)
	}

	for _, next := range []string{"short", strings.Repeat("x", 129), "\u00e9\u00e9\u00e9\u00e9\u00e9\u00e9\u00e9"} {
		err := svc.ChangePassword(ctx, ada, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: next})
		wantErr(test, err, services.ErrPasswordPolicy)
	}
	// Characters, not bytes: eight 2-byte runes pass.
	creds.EXPECT().ChangePassword(ctx, "u1", "password123", strings.Repeat("\u00e9", 8), false, "s1").Return(nil)
	if err := svc.ChangePassword(ctx, ada, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: strings.Repeat("\u00e9", 8)}); err != nil {
		test.Fatal(err)
	}

	err := svc.ChangePassword(ctx, ada, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password123"})
	wantErr(test, err, services.ErrSamePassword)

	for _, portErr := range []error{services.ErrInvalidPassword, services.ErrPasswordPolicy, services.ErrUserNotFound} {
		creds.EXPECT().ChangePassword(ctx, "u1", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("auth: %w", portErr))
		wantErr(test, svc.ChangePassword(ctx, ada, "u1", in), portErr)
	}

	creds.EXPECT().ChangePassword(ctx, "u1", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errDB)
	wantInternal(test, svc.ChangePassword(ctx, ada, "u1", in), errDB)
}

func TestListSessions(test *testing.T) {
	test.Parallel()
	svc, _, creds := newService(test)

	creds.EXPECT().ListSessions(ctx, "u1").Return([]services.Session{{ID: "s1"}, {ID: "s2"}}, nil)
	got, err := svc.ListSessions(ctx, ada, "u1")
	if err != nil || len(got) != 2 || !got[0].Current || got[1].Current {
		test.Fatalf("ListSessions = %+v, %v; want s1 marked current", got, err)
	}

	creds.EXPECT().ListSessions(ctx, "u1").Return(nil, nil)
	if got, err := svc.ListSessions(ctx, ada, "u1"); err != nil || got == nil {
		test.Fatalf("no sessions = %#v, %v; want an empty, non-nil list", got, err)
	}

	creds.EXPECT().ListSessions(ctx, "u1").Return(nil, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	_, err = svc.ListSessions(ctx, ada, "u1")
	wantErr(test, err, services.ErrUserNotFound)

	creds.EXPECT().ListSessions(ctx, "u1").Return(nil, errDB)
	_, err = svc.ListSessions(ctx, ada, "u1")
	wantInternal(test, err, errDB)
}

func TestRevokeSession(test *testing.T) {
	test.Parallel()
	svc, _, creds := newService(test)

	creds.EXPECT().RevokeSession(ctx, "u1", "s2").Return(nil)
	if err := svc.RevokeSession(ctx, ada, "u1", "s2"); err != nil {
		test.Fatal(err)
	}

	wantErr(test, svc.RevokeSession(ctx, ada, "u1", "s1"), services.ErrCurrentSession)

	creds.EXPECT().RevokeSession(ctx, "u1", "other-users-session").Return(fmt.Errorf("auth: %w", services.ErrSessionNotFound))
	wantErr(test, svc.RevokeSession(ctx, ada, "u1", "other-users-session"), services.ErrSessionNotFound)

	creds.EXPECT().RevokeSession(ctx, "u1", "s2").Return(errDB)
	wantInternal(test, svc.RevokeSession(ctx, ada, "u1", "s2"), errDB)
}

func TestRevokeOtherSessions(test *testing.T) {
	test.Parallel()
	svc, _, creds := newService(test)

	creds.EXPECT().RevokeOtherSessions(ctx, "u1", "s1").Return(nil)
	if err := svc.RevokeOtherSessions(ctx, ada, "u1"); err != nil {
		test.Fatal(err)
	}

	creds.EXPECT().RevokeOtherSessions(ctx, "u1", "s1").Return(services.ErrUserNotFound)
	wantErr(test, svc.RevokeOtherSessions(ctx, ada, "u1"), services.ErrUserNotFound)

	creds.EXPECT().RevokeOtherSessions(ctx, "u1", "s1").Return(errDB)
	wantInternal(test, svc.RevokeOtherSessions(ctx, ada, "u1"), errDB)
}

func TestDeleteAccount(test *testing.T) {
	test.Parallel()
	svc, repo, _ := newService(test)

	repo.EXPECT().Delete(ctx, "u1").Return(nil)
	if err := svc.DeleteAccount(ctx, ada, "u1"); err != nil {
		test.Fatal(err)
	}

	repo.EXPECT().Delete(ctx, "u1").Return(services.ErrRecordNotFound)
	wantErr(test, svc.DeleteAccount(ctx, ada, "u1"), services.ErrUserNotFound)

	repo.EXPECT().Delete(ctx, "u1").Return(errDB)
	wantInternal(test, svc.DeleteAccount(ctx, ada, "u1"), errDB)
}

func TestDataSummary(test *testing.T) {
	test.Parallel()
	svc, repo, _ := newService(test)
	want := services.UserDataSummary{User: adaUser, Sessions: 2}

	repo.EXPECT().DataSummary(ctx, "u1").Return(want, nil)
	if got, err := svc.DataSummary(ctx, ada, "u1"); err != nil || got != want {
		test.Fatalf("DataSummary = %+v, %v", got, err)
	}

	repo.EXPECT().DataSummary(ctx, "u1").Return(services.UserDataSummary{}, services.ErrRecordNotFound)
	_, err := svc.DataSummary(ctx, ada, "u1")
	wantErr(test, err, services.ErrUserNotFound)

	repo.EXPECT().DataSummary(ctx, "u1").Return(services.UserDataSummary{}, errDB)
	_, err = svc.DataSummary(ctx, ada, "u1")
	wantInternal(test, err, errDB)
}

// Kind sentinels match every error of their kind; specific errors only themselves.
func TestErrorKinds(test *testing.T) {
	test.Parallel()
	cases := []struct {
		err, target error
		want        bool
	}{
		{services.ErrEmailTaken, services.ErrConflict, true},
		{services.ErrSamePassword, services.ErrValidation, true},
		{fmt.Errorf("wrapped: %w", services.ErrSessionNotFound), services.ErrNotFound, true},
		{services.ErrForbidden, services.ErrForbidden, true},
		{services.ErrEmailTaken, services.ErrNotFound, false},
		{services.ErrSessionNotFound, services.ErrUserNotFound, false},
		{services.ErrNotFound, services.ErrUserNotFound, false},
		{errDB, services.ErrNotFound, false},
	}
	for _, testCase := range cases {
		if got := errors.Is(testCase.err, testCase.target); got != testCase.want {
			test.Errorf("errors.Is(%v, %v) = %v, want %v", testCase.err, testCase.target, got, testCase.want)
		}
	}
	if services.KindOf(services.ErrEmailTaken) != services.KindConflict || services.KindOf(errDB) != services.KindInternal {
		test.Error("KindOf")
	}
}
