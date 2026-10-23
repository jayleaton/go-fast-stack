package ui

import (
	"context"
	"errors"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"

	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

// serviceErrorKeys translates user-service errors, most specific first: the
// named errors, then the kind sentinels (errors.Is(err, apiclient.ErrNotFound)
// holds for every not-found error).
var serviceErrorKeys = []struct {
	err error
	key string
}{
	{apiclient.ErrInvalidPassword, "pages.account.errors.wrongPassword"},
	{apiclient.ErrPasswordPolicy, "pages.account.errors.passwordPolicy"},
	{apiclient.ErrSamePassword, "pages.account.errors.samePassword"},
	{apiclient.ErrNoPassword, "pages.account.errors.noPassword"},
	{apiclient.ErrSameEmail, "pages.account.errors.sameEmail"},
	{apiclient.ErrEmailTaken, "errors.emailTaken"},
	{apiclient.ErrSessionNotFresh, "pages.account.errors.sessionNotFresh"},
	{apiclient.ErrSessionNotFound, "pages.account.errors.sessionNotFound"},
	{apiclient.ErrCurrentSession, "pages.account.errors.currentSession"},
	{apiclient.ErrRateLimited, "errors.tooManyRequests"},
	{apiclient.ErrValidation, "errors.invalidInput"},
	{apiclient.ErrForbidden, "errors.forbidden"},
	{apiclient.ErrNotFound, "errors.notFound"},
	{apiclient.ErrConflict, "errors.conflict"},
}

// fieldKeys translates validator failures by field, first match wins.
var fieldKeys = []struct{ field, key string }{
	{"name", "pages.account.errors.name"},
	{"image", "pages.account.errors.image"},
	{"newEmail", "errors.emailInvalid"},
	{"currentPassword", "errors.passwordRequired"},
	{"newPassword", "errors.passwordRequired"},
}

// serviceMessage translates a validation or user-service error for display.
// Internal errors are logged and shown as errors.somethingWentWrong.
func (u *UI) serviceMessage(ctx context.Context, err error) string {
	if verr, ok := errors.AsType[*apiclient.ValidationError](err); ok {
		for _, f := range fieldKeys {
			if _, bad := verr.Fields[f.field]; bad {
				return i18n.T(ctx, f.key)
			}
		}
		return i18n.T(ctx, "errors.invalidInput")
	}
	for _, e := range serviceErrorKeys {
		if errors.Is(err, e.err) {
			return i18n.T(ctx, e.key)
		}
	}
	u.log.ErrorContext(ctx, "ui: user service", "err", err)
	return i18n.T(ctx, "errors.somethingWentWrong")
}
