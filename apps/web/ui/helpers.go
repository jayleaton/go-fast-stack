package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

func orDash(text string) string {
	if text == "" {
		return "—"
	}
	return text
}

func memberSince(ctx context.Context, moment time.Time) string {
	if moment.IsZero() {
		return "—"
	}
	return i18n.From(ctx).Date(moment)
}

// jsString encodes s as a JavaScript string literal.
func jsString(text string) string {
	data, _ := json.Marshal(text)
	return string(data)
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
