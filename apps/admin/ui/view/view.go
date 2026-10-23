// Package view holds the admin layout: the uikit document and app shell with
// the app name, section nav, signed-in admin, sign out, theme and language.
// Pages build everything else from packages/uikit components.
package view

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/packages/uikit"
)

// DefaultAppName is the brand when Deps.AppName is empty.
const DefaultAppName = "Admin"

// Nav sections; State.Section highlights one.
const (
	SectionUsers     = "users"
	SectionAdmins    = "admins"
	SectionAudit     = "audit"
	SectionAnalytics = "analytics"
)

// State is the per-request data the layout needs.
type State struct {
	// AppName is the brand in the header and <title>.
	AppName       string
	Authenticated bool
	// Email is the signed-in admin's address, shown in the header.
	Email string
	// Section is the current nav section ("" outside the dashboard).
	Section string
	// Theme is "dark", "light" or "" (system).
	Theme string
	// Path is the current path and query, used to come back after switching locale.
	Path string
}

type stateKey struct{}

// WithState stores the layout state on ctx.
func WithState(ctx context.Context, state State) context.Context {
	return context.WithValue(ctx, stateKey{}, state)
}

// StateFrom returns the layout state, zero when absent.
func StateFrom(ctx context.Context) State {
	state, _ := ctx.Value(stateKey{}).(State)
	return state
}

// ThemeFrom reads the theme cookie: "dark", "light" or "" (system).
func ThemeFrom(request *http.Request) string { return uikit.ThemeFrom(request) }
