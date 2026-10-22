package ui

import (
	"context"
	"errors"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
)

// serviceErrorKeys translates user-service errors, most specific first: the
// named errors, then the kind sentinels (errors.Is(err, services.ErrNotFound)
// holds for every not-found error).
var serviceErrorKeys = []struct {
	err error
	key string
}{
	{services.ErrInvalidPassword, "pages.account.errors.wrongPassword"},
	{services.ErrPasswordPolicy, "pages.account.errors.passwordPolicy"},
	{services.ErrSamePassword, "pages.account.errors.samePassword"},
	{services.ErrNoPassword, "pages.account.errors.noPassword"},
	{services.ErrSameEmail, "pages.account.errors.sameEmail"},
	{services.ErrEmailTaken, "errors.emailTaken"},
	{services.ErrSessionNotFresh, "pages.account.errors.sessionNotFresh"},
	{services.ErrSessionNotFound, "pages.account.errors.sessionNotFound"},
	{services.ErrCurrentSession, "pages.account.errors.currentSession"},
	{services.ErrRateLimited, "errors.tooManyRequests"},
	{services.ErrValidation, "errors.invalidInput"},
	{services.ErrForbidden, "errors.forbidden"},
	{services.ErrNotFound, "errors.notFound"},
	{services.ErrConflict, "errors.conflict"},
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
	if verr, ok := errors.AsType[*validate.Error](err); ok {
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
