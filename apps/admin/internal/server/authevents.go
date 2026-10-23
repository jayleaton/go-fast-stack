package server

import (
	"context"
	"log/slog"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/packages/auth"
)

// NewAuditEvents is the admin auth instance's auth.Config.Events: it audits
// admin sign-ins, sign-outs, refused second-factor codes and spent backup codes. Each event names
// its admin (for 2FA, the admin of the pending sign-in), so attribution is
// exact however many admin instances run against the database, and it covers
// every path (UI and JSON auth endpoints) without wrapping them.
//
// It runs after the fact on auth's event worker: a failed audit write is
// logged at error level and never undoes or delays the authentication.
func NewAuditEvents(svc *services.AuthEventService, log *slog.Logger) auth.EventHandler {
	return func(ctx context.Context, event auth.Event) {
		var err error
		switch event.Kind {
		case auth.EventSignIn:
			err = svc.SignedIn(ctx, event.At, event.UserID, event.Email, signInMethod(event.Method))
		case auth.EventSignOut:
			err = svc.SignedOut(ctx, event.At, event.UserID, event.Email)
		case auth.EventTwoFactorFailed:
			if event.UserID == "" {
				return // no valid pending sign-in: there is no admin to attribute it to
			}
			err = svc.TwoFactorFailed(ctx, event.At, event.UserID, event.Email, event.Reason)
		case auth.EventTwoFactorSucceeded:
			if event.Method != auth.TwoFactorMethodBackup {
				return // TOTP codes are covered by the sign-in they complete
			}
			err = svc.BackupCodeUsed(ctx, event.At, event.UserID, event.Email)
		default:
			return
		}
		if err != nil {
			log.ErrorContext(ctx, "admin: audit of "+string(event.Kind)+" not recorded", "adminId", event.UserID, "err", err)
		}
	}
}

// signInMethod maps auth's sign-in methods to the audit's.
func signInMethod(m string) string {
	switch m {
	case auth.SignInMethodTwoFactor:
		return services.SignInTwoFactor
	case auth.SignInMethodSetPassword:
		return services.SignInSetPassword
	case auth.SignInMethodPassword:
		return services.SignInPassword
	default:
		return m
	}
}
