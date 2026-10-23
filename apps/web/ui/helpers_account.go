package ui

import (
	"context"
	"strings"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/apiclient"

	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

func deref(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

func hasOtherSessions(sessions []apiclient.Session) bool {
	for _, session := range sessions {
		if !session.Current {
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

// mobileDevice reports whether a session's user agent is a phone or tablet
// (the sessions list picks its icon by it).
func mobileDevice(ua *string) bool {
	agent := deref(ua)
	for _, token := range []string{"iPhone", "iPad", "Android", "Mobile", "Expo", "okhttp"} {
		if strings.Contains(agent, token) {
			return true
		}
	}
	return false
}

// deviceLabel names a session's device ("Chrome on macOS") from its user agent.
func deviceLabel(ctx context.Context, ua *string) string {
	agent := deref(ua)
	browser, system := "", ""
	for _, item := range browsers {
		if strings.Contains(agent, item.token) {
			browser = item.name
			break
		}
	}
	for _, item := range systems {
		if strings.Contains(agent, item.token) {
			system = item.name
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
	return i18n.Translate(ctx, "pages.account.sessions.unknownDevice")
}
