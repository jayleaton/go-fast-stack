// Scratch: CI billing shape (web-only change).
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
)

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func memberSince(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return i18n.From(ctx).Date(t)
}

// jsString encodes s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func cooldownSignals(seconds int) string {
	return fmt.Sprintf("{_cooldown: %d}", seconds)
}

// cooldownLabel renders "Resend in m:ss" while the cooldown runs.
func cooldownLabel(resendIn, resend string) string {
	return fmt.Sprintf(
		"$_cooldown > 0 ? %s + ' ' + Math.floor($_cooldown / 60) + ':' + String($_cooldown %% 60).padStart(2, '0') : %s",
		jsString(resendIn), jsString(resend),
	)
}
