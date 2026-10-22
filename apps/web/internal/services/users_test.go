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

func ptr[T any](v T) *T { return &v }

func newService(t *testing.T) (*services.UserService, *mocks.MockUserRepository, *mocks.MockAccountCredentials) {
	ctrl := gomock.NewController(t)
	repo, creds := mocks.NewMockUserRepository(ctrl), mocks.NewMockAccountCredentials(ctrl)
	return services.NewUserService(repo, creds), repo, creds
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

// Every method refuses to act on another user before touching a port: the
// mocks have no expectations, so any call fails the test.
func TestUserServiceForbidsOtherUsers(t *testing.T) {
	t.Parallel()
	type service = *services.UserService
	calls := map[string]func(service, services.Actor, string) error{
		"GetProfile": func(svc service, a services.Actor, id string) error { _, err := svc.GetProfile(ctx, a, id); return err },
		"UpdateProfile": func(svc service, a services.Actor, id string) error {
			_, err := svc.UpdateProfile(ctx, a, id, validate.UpdateUserSettings{Name: ptr("Eve")})
			return err
		},
		"RequestEmailChange": func(svc service, a services.Actor, id string) error {
			return svc.RequestEmailChange(ctx, a, id, validate.UpdateUserEmail{NewEmail: "x@example.com"})
		},
		"ChangePassword": func(svc service, a services.Actor, id string) error {
			return svc.ChangePassword(ctx, a, id, validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456"})
		},
		"ListSessions": func(svc service, a services.Actor, id string) error {
			_, err := svc.ListSessions(ctx, a, id)
			return err
		},
		"RevokeSession":       func(svc service, a services.Actor, id string) error { return svc.RevokeSession(ctx, a, id, "s9") },
		"RevokeOtherSessions": func(svc service, a services.Actor, id string) error { return svc.RevokeOtherSessions(ctx, a, id) },
		"DeleteAccount":       func(svc service, a services.Actor, id string) error { return svc.DeleteAccount(ctx, a, id) },
		"DataSummary": func(svc service, a services.Actor, id string) error {
			_, err := svc.DataSummary(ctx, a, id)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, _, _ := newService(t) // its own mocks: an unexpected call fails this subtest
			wantErr(t, call(svc, ada, "u2"), services.ErrForbidden)
			wantErr(t, call(svc, services.Actor{}, ""), services.ErrForbidden) // no actor is nobody
		})
	}
}

func TestGetProfile(t *testing.T) {
	t.Parallel()
	svc, repo, _ := newService(t)
	repo.EXPECT().GetByID(ctx, "u1").Return(adaUser, nil)
	if u, err := svc.GetProfile(ctx, ada, "u1"); err != nil || u != adaUser {
		t.Fatalf("GetProfile = %+v, %v", u, err)
	}

	repo.EXPECT().GetByID(ctx, "u1").Return(services.User{}, fmt.Errorf("get: %w", services.ErrRecordNotFound))
	_, err := svc.GetProfile(ctx, ada, "u1")
	wantErr(t, err, services.ErrUserNotFound)

	repo.EXPECT().GetByID(ctx, "u1").Return(services.User{}, errDB)
	_, err = svc.GetProfile(ctx, ada, "u1")
	wantInternal(t, err, errDB)
}

func TestUpdateProfile(t *testing.T) {
	t.Parallel()
	svc, repo, _ := newService(t)
	renamed := adaUser
	renamed.Name = "Ada L"

	repo.EXPECT().UpdateProfile(ctx, "u1", services.UserProfileUpdate{Name: ptr("Ada L")}).Return(renamed, nil)
	if u, err := svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{Name: ptr("Ada L")}); err != nil || u.Name != "Ada L" {
		t.Fatalf("rename = %+v, %v", u, err)
	}

	// An explicit null image clears it.
	repo.EXPECT().UpdateProfile(ctx, "u1", services.UserProfileUpdate{SetImage: true}).Return(adaUser, nil)
	if _, err := svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{Image: validate.Nullable[string]{Set: true}}); err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().UpdateProfile(ctx, "u1", gomock.Any()).Return(services.User{}, services.ErrRecordNotFound)
	_, err := svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{})
	wantErr(t, err, services.ErrUserNotFound)

	repo.EXPECT().UpdateProfile(ctx, "u1", gomock.Any()).Return(services.User{}, errDB)
	_, err = svc.UpdateProfile(ctx, ada, "u1", validate.UpdateUserSettings{})
	wantInternal(t, err, errDB)
}

func TestRequestEmailChange(t *testing.T) {
	t.Parallel()
	svc, _, creds := newService(t)
	in := validate.UpdateUserEmail{NewEmail: "ada@new.example", CallbackURL: "/dashboard"}

	creds.EXPECT().ChangeEmail(ctx, "u1", "ada@new.example", "/dashboard").Return(nil)
	if err := svc.RequestEmailChange(ctx, ada, "u1", in); err != nil {
		t.Fatal(err)
	}

	err := svc.RequestEmailChange(ctx, ada, "u1", validate.UpdateUserEmail{NewEmail: "ADA@example.com"})
	wantErr(t, err, services.ErrSameEmail)

	// Port failures keep their identity through the service (the UI translates per error).
	for _, portErr := range []error{services.ErrEmailTaken, services.ErrInvalidCallbackURL, services.ErrUserNotFound} {
		creds.EXPECT().ChangeEmail(ctx, "u1", gomock.Any(), gomock.Any()).Return(fmt.Errorf("auth: %w", portErr))
		wantErr(t, svc.RequestEmailChange(ctx, ada, "u1", in), portErr)
	}

	creds.EXPECT().ChangeEmail(ctx, "u1", gomock.Any(), gomock.Any()).Return(errDB)
	wantInternal(t, svc.RequestEmailChange(ctx, ada, "u1", in), errDB)
}

func TestChangePassword(t *testing.T) {
	t.Parallel()
	svc, _, creds := newService(t)
	in := validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password456", RevokeOtherSessions: true}

	// The actor's session survives a revoke-others password change.
	creds.EXPECT().ChangePassword(ctx, "u1", "password123", "password456", true, "s1").Return(nil)
	if err := svc.ChangePassword(ctx, ada, "u1", in); err != nil {
		t.Fatal(err)
	}

	for _, next := range []string{"short", strings.Repeat("x", 129), "\u00e9\u00e9\u00e9\u00e9\u00e9\u00e9\u00e9"} {
		err := svc.ChangePassword(ctx, ada, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: next})
		wantErr(t, err, services.ErrPasswordPolicy)
	}
	// Characters, not bytes: eight 2-byte runes pass.
	creds.EXPECT().ChangePassword(ctx, "u1", "password123", strings.Repeat("\u00e9", 8), false, "s1").Return(nil)
	if err := svc.ChangePassword(ctx, ada, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: strings.Repeat("\u00e9", 8)}); err != nil {
		t.Fatal(err)
	}

	err := svc.ChangePassword(ctx, ada, "u1", validate.UpdateUserPassword{CurrentPassword: "password123", NewPassword: "password123"})
	wantErr(t, err, services.ErrSamePassword)

	for _, portErr := range []error{services.ErrInvalidPassword, services.ErrPasswordPolicy, services.ErrUserNotFound} {
		creds.EXPECT().ChangePassword(ctx, "u1", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("auth: %w", portErr))
		wantErr(t, svc.ChangePassword(ctx, ada, "u1", in), portErr)
	}

	creds.EXPECT().ChangePassword(ctx, "u1", gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errDB)
	wantInternal(t, svc.ChangePassword(ctx, ada, "u1", in), errDB)
}

func TestListSessions(t *testing.T) {
	t.Parallel()
	svc, _, creds := newService(t)

	creds.EXPECT().ListSessions(ctx, "u1").Return([]services.Session{{ID: "s1"}, {ID: "s2"}}, nil)
	got, err := svc.ListSessions(ctx, ada, "u1")
	if err != nil || len(got) != 2 || !got[0].Current || got[1].Current {
		t.Fatalf("ListSessions = %+v, %v; want s1 marked current", got, err)
	}

	creds.EXPECT().ListSessions(ctx, "u1").Return(nil, nil)
	if got, err := svc.ListSessions(ctx, ada, "u1"); err != nil || got == nil {
		t.Fatalf("no sessions = %#v, %v; want an empty, non-nil list", got, err)
	}

	creds.EXPECT().ListSessions(ctx, "u1").Return(nil, fmt.Errorf("auth: %w", services.ErrUserNotFound))
	_, err = svc.ListSessions(ctx, ada, "u1")
	wantErr(t, err, services.ErrUserNotFound)

	creds.EXPECT().ListSessions(ctx, "u1").Return(nil, errDB)
	_, err = svc.ListSessions(ctx, ada, "u1")
	wantInternal(t, err, errDB)
}

func TestRevokeSession(t *testing.T) {
	t.Parallel()
	svc, _, creds := newService(t)

	creds.EXPECT().RevokeSession(ctx, "u1", "s2").Return(nil)
	if err := svc.RevokeSession(ctx, ada, "u1", "s2"); err != nil {
		t.Fatal(err)
	}

	wantErr(t, svc.RevokeSession(ctx, ada, "u1", "s1"), services.ErrCurrentSession)

	creds.EXPECT().RevokeSession(ctx, "u1", "other-users-session").Return(fmt.Errorf("auth: %w", services.ErrSessionNotFound))
	wantErr(t, svc.RevokeSession(ctx, ada, "u1", "other-users-session"), services.ErrSessionNotFound)

	creds.EXPECT().RevokeSession(ctx, "u1", "s2").Return(errDB)
	wantInternal(t, svc.RevokeSession(ctx, ada, "u1", "s2"), errDB)
}

func TestRevokeOtherSessions(t *testing.T) {
	t.Parallel()
	svc, _, creds := newService(t)

	creds.EXPECT().RevokeOtherSessions(ctx, "u1", "s1").Return(nil)
	if err := svc.RevokeOtherSessions(ctx, ada, "u1"); err != nil {
		t.Fatal(err)
	}

	creds.EXPECT().RevokeOtherSessions(ctx, "u1", "s1").Return(services.ErrUserNotFound)
	wantErr(t, svc.RevokeOtherSessions(ctx, ada, "u1"), services.ErrUserNotFound)

	creds.EXPECT().RevokeOtherSessions(ctx, "u1", "s1").Return(errDB)
	wantInternal(t, svc.RevokeOtherSessions(ctx, ada, "u1"), errDB)
}

func TestDeleteAccount(t *testing.T) {
	t.Parallel()
	svc, repo, _ := newService(t)

	repo.EXPECT().Delete(ctx, "u1").Return(nil)
	if err := svc.DeleteAccount(ctx, ada, "u1"); err != nil {
		t.Fatal(err)
	}

	repo.EXPECT().Delete(ctx, "u1").Return(services.ErrRecordNotFound)
	wantErr(t, svc.DeleteAccount(ctx, ada, "u1"), services.ErrUserNotFound)

	repo.EXPECT().Delete(ctx, "u1").Return(errDB)
	wantInternal(t, svc.DeleteAccount(ctx, ada, "u1"), errDB)
}

func TestDataSummary(t *testing.T) {
	t.Parallel()
	svc, repo, _ := newService(t)
	want := services.UserDataSummary{User: adaUser, Sessions: 2}

	repo.EXPECT().DataSummary(ctx, "u1").Return(want, nil)
	if got, err := svc.DataSummary(ctx, ada, "u1"); err != nil || got != want {
		t.Fatalf("DataSummary = %+v, %v", got, err)
	}

	repo.EXPECT().DataSummary(ctx, "u1").Return(services.UserDataSummary{}, services.ErrRecordNotFound)
	_, err := svc.DataSummary(ctx, ada, "u1")
	wantErr(t, err, services.ErrUserNotFound)

	repo.EXPECT().DataSummary(ctx, "u1").Return(services.UserDataSummary{}, errDB)
	_, err = svc.DataSummary(ctx, ada, "u1")
	wantInternal(t, err, errDB)
}

// Kind sentinels match every error of their kind; specific errors only themselves.
func TestErrorKinds(t *testing.T) {
	t.Parallel()
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
	for _, c := range cases {
		if got := errors.Is(c.err, c.target); got != c.want {
			t.Errorf("errors.Is(%v, %v) = %v, want %v", c.err, c.target, got, c.want)
		}
	}
	if services.KindOf(services.ErrEmailTaken) != services.KindConflict || services.KindOf(errDB) != services.KindInternal {
		t.Error("KindOf")
	}
}
