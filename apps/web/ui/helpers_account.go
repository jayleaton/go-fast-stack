package ui

import (
	"context"
	"strings"
	"unicode"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/i18n"
)

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// initials is the avatar fallback: the first letter of up to two words.
func initials(name string) string {
	var out []rune
	for _, w := range strings.Fields(name) {
		out = append(out, unicode.ToUpper([]rune(w)[0]))
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

func hasOtherSessions(sessions []services.Session) bool {
	for _, s := range sessions {
		if !s.Current {
			return true
		}
	}
	return false
}

// Browser and OS names by the user-agent token that identifies them, most
// specific first (Edge and Opera also say Chrome; Chrome also says Safari).
var (
	browsers = []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"},
		{"CriOS/", "Chrome"}, {"Safari/", "Safari"}, {"Expo", "Expo"}, {"okhttp", "Android app"},
	}
	systems = []struct{ token, name string }{
		{"iPhone", "iOS"}, {"iPad", "iPadOS"}, {"Android", "Android"}, {"Windows", "Windows"},
		{"Mac OS X", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	}
)

// deviceLabel names a session's device ("Chrome on macOS") from its user agent.
func deviceLabel(ctx context.Context, ua *string) string {
	agent := deref(ua)
	browser, system := "", ""
	for _, b := range browsers {
		if strings.Contains(agent, b.token) {
			browser = b.name
			break
		}
	}
	for _, s := range systems {
		if strings.Contains(agent, s.token) {
			system = s.name
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return i18n.Tf(ctx, "pages.account.sessions.device", "browser", browser, "os", system)
	case browser != "":
		return browser
	case system != "":
		return system
	}
	return i18n.T(ctx, "pages.account.sessions.unknownDevice")
}
