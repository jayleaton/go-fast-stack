// Package view is the web app's layout (uikit.Page + uikit.AppShell with
// its nav, theme toggle, language menu and sign out), the per-request
// layout state, and the few app-local components uikit does not have.
// Pages use packages/uikit for everything else.
package view

import (
	"context"
	"net/http"
	"strings"
	"unicode"

	"github.com/jayleaton/go-fast-stack/packages/uikit"
)

// State is the per-request data the layout needs.
type State struct {
	// AppName is the product name in the header and <title> (Deps.AppName).
	AppName       string
	Authenticated bool
	// Theme is "dark", "light" or "" (system).
	Theme string
	// Path is the current path and query, used to come back after switching locale.
	Path string
}

type stateKey struct{}

// WithState stores the layout state on ctx.
func WithState(ctx context.Context, s State) context.Context {
	return context.WithValue(ctx, stateKey{}, s)
}

// StateFrom returns the layout state, zero when absent.
func StateFrom(ctx context.Context) State {
	s, _ := ctx.Value(stateKey{}).(State)
	return s
}

// ThemeFrom reads the theme cookie: "dark", "light" or "" (system).
func ThemeFrom(r *http.Request) string { return uikit.ThemeFrom(r) }

// PostForm is the Datastar action that submits the closest form urlencoded.
func PostForm(path string) string { return uikit.PostForm(path) }

// Get is the Datastar action for a GET SSE stream.
func Get(path string) string { return uikit.Get(path) }

// Assign is the expression that navigates to path (a safe relative path).
func Assign(path string) string { return uikit.Assign(path) }

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
