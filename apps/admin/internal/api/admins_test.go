package api_test

import (
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

var (
	linus = services.Admin{ID: "a2", Name: "Linus T", Email: "linus@example.com"} // a pending invite

	emailRule = "must be a valid email address"

	adminNotFound = svcErr{services.ErrAdminNotFound, 404, "Admin not found"}
	adminSelf     = svcErr{services.ErrAdminSelf, 400, "You cannot disable or remove yourself"}
	lastAdmin     = svcErr{services.ErrLastAdmin, 409, "The last active admin cannot be disabled or removed"}
)

var adminRoutes = map[string]route{
	"GET admins": {
		method: "GET", path: r.Admins,
		expect: func(m svcs, err error) {
			m.admins.ListAdmins(anyCtx, actor).Return([]services.Admin{{ID: "a1", Email: grace.Email, HasPassword: true, TwoFactorEnabled: true}, linus}, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			got := apitest.Data[[]services.Admin](t, res)
			if len(got) != 2 || !got[0].HasPassword || got[1].HasPassword || got[1].Email != linus.Email {
				t.Errorf("data = %s", res.Data)
			}
		},
	},
	"POST admin invite": {
		method: "POST", path: r.AdminInvites, body: `{"email":"  Linus@Example.COM ","name":"  Linus T "}`,
		invalidBody: `{"email":"not an email"}`, field: "email",
		errs: []svcErr{{services.ErrAdminExists, 409, "This person is already an admin"}, unavailable},
		expect: func(m svcs, err error) {
			m.admins.InviteAdmin(anyCtx, actor, validate.CreateAdminInvite{Email: "linus@example.com", Name: "Linus T"}).Return(linus, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			if got := apitest.Data[services.Admin](t, res); got != linus {
				t.Errorf("data = %s", res.Data)
			}
		},
	},
	"POST admin disable": {
		method: "POST", path: config.AdminPath("admin 2", "/disable"),
		errs: []svcErr{adminNotFound, adminSelf, lastAdmin, {services.ErrAdminDisabled, 400, "This admin is already disabled"}, unavailable},
		expect: func(m svcs, err error) { // the path id arrives unescaped
			m.admins.DisableAdmin(anyCtx, actor, "admin 2").Return(err)
		},
		check: wantSuccess,
	},
	"POST admin enable": {
		method: "POST", path: config.AdminPath("a2", "/enable"),
		errs: []svcErr{adminNotFound, {services.ErrAdminNotDisabled, 400, "This admin is not disabled"}, unavailable},
		expect: func(m svcs, err error) {
			m.admins.EnableAdmin(anyCtx, actor, "a2").Return(err)
		},
		check: wantSuccess,
	},
	"POST admin password reset": {
		method: "POST", path: config.AdminPath("a2", "/password-reset"),
		errs: []svcErr{adminNotFound, {services.ErrAdminPending, 400, services.ErrAdminPending.Message}, {services.ErrAdminSelfReset, 400, services.ErrAdminSelfReset.Message}, unavailable},
		expect: func(m svcs, err error) {
			m.admins.SendPasswordReset(anyCtx, actor, "a2").Return(err)
		},
		check: wantSuccess,
	},
	"POST admin two-factor reset": {
		method: "POST", path: config.AdminPath("a2", "/two-factor/reset"),
		errs: []svcErr{adminNotFound, {services.ErrAdminNoTwoFactor, 400, services.ErrAdminNoTwoFactor.Message}, {services.ErrAdminSelfReset, 400, services.ErrAdminSelfReset.Message}, unavailable},
		expect: func(m svcs, err error) {
			m.admins.ResetTwoFactor(anyCtx, actor, "a2").Return(err)
		},
		check: wantSuccess,
	},
	"DELETE admin": {
		method: "DELETE", path: config.AdminPath("a2", ""), errs: []svcErr{adminNotFound, adminSelf, lastAdmin, unavailable},
		expect: func(m svcs, err error) {
			m.admins.RemoveAdmin(anyCtx, actor, "a2").Return(err)
		},
		check: wantSuccess,
	},
}

// TestCreateAdminInviteBody covers every CreateAdminInvite rule: the address
// is normalised and the name defaults to its local part.
func TestCreateAdminInviteBody(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 244) + "@example.com" // 256 characters
	for _, email := range []string{"", "   ", "not an email", "Ada <ada@example.com>", "ada@", "@example.com", long} {
		wantInvalid(t, "POST", r.AdminInvites, `{"email":"`+email+`","name":"Ada"}`, map[string]string{"email": emailRule})
	}
	wantInvalid(t, "POST", r.AdminInvites, `{}`, map[string]string{"email": emailRule})
	wantInvalid(t, "POST", r.AdminInvites, `{"email":"ada@example.com","name":"`+strings.Repeat("é", 256)+`"}`, map[string]string{"name": "must be at most 255 characters"})
	wantInvalid(t, "POST", r.AdminInvites, `{"email":"ada","name":"`+strings.Repeat("é", 256)+`"}`, map[string]string{"email": emailRule, "name": "must be at most 255 characters"})

	valid := []struct {
		body string
		want validate.CreateAdminInvite
	}{
		{`{"email":"Ada@Example.com"}`, validate.CreateAdminInvite{Email: "ada@example.com", Name: "ada"}},
		{`{"email":" ada.l@example.com ","name":"  "}`, validate.CreateAdminInvite{Email: "ada.l@example.com", Name: "ada.l"}},
		{`{"email":"ada@example.com","name":"` + strings.Repeat("é", 255) + `"}`, validate.CreateAdminInvite{Email: "ada@example.com", Name: strings.Repeat("é", 255)}},
		{`{"email":"` + long[1:] + `","name":"X"}`, validate.CreateAdminInvite{Email: long[1:], Name: "X"}}, // 255 characters
	}
	for _, c := range valid {
		wantOK(t, "POST", r.AdminInvites, c.body, func(m svcs) {
			m.admins.InviteAdmin(anyCtx, actor, c.want)
		})
	}
}
