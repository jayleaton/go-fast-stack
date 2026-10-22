// Package view holds the layout shell and the shared templ components
// (inputs, buttons, alerts, skeletons) ported from apps/web-app/components.
package view

import (
	"context"
	"net/http"

	"github.com/jayleaton/ai-patterns-go/packages/uikit"
)

// ThemeCookie stores the theme toggle choice ("dark" or "light"). Without it
// the CSS follows prefers-color-scheme.
const ThemeCookie = uikit.ThemeCookie

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

// Post is the Datastar action that posts the page's signals as JSON.
func Post(path string) string { return uikit.Post(path) }

// Get is the Datastar action for a GET SSE stream.
func Get(path string) string { return uikit.Get(path) }

// Assign is the expression that navigates to path (a safe relative path).
func Assign(path string) string { return uikit.Assign(path) }

// Shared Tailwind classes, copied from the original pages.
const (
	inputClass = "mt-1 block w-full rounded-md border border-neutral-300 bg-white px-3 py-2 text-neutral-900 placeholder-neutral-500 shadow-sm focus:outline-none focus:ring-2 focus:ring-neutral-300 focus:border-neutral-300 sm:text-sm dark:border-neutral-800 dark:bg-neutral-900 dark:text-neutral-100 dark:placeholder-neutral-500 dark:focus:ring-neutral-700 dark:focus:border-neutral-700"
	labelClass = "block text-sm font-medium text-neutral-700 dark:text-neutral-300"

	PrimaryButtonClass = "group relative flex w-full justify-center rounded-md border border-transparent bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-600/90 focus:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-blue-500 dark:hover:bg-blue-400"
	SuccessButtonClass = "group relative flex w-full justify-center rounded-md border border-transparent bg-green-600 px-4 py-2 text-sm font-medium text-white hover:bg-green-600/90 focus:outline-none focus-visible:ring-2 focus-visible:ring-green-500"
	TextLinkClass      = "font-medium text-blue-600 hover:text-blue-600/90 dark:text-blue-400 dark:hover:text-blue-300"
	ErrorAlertClass    = "rounded-md border border-red-300 bg-red-50 px-4 py-3 text-red-700 dark:border-red-800 dark:bg-red-950/40 dark:text-red-300"
	SuccessAlertClass  = "rounded-md border border-green-300 bg-green-50 px-4 py-3 text-green-700 dark:border-green-800 dark:bg-green-950/40 dark:text-green-300"
	HeadingClass       = "mt-6 text-center text-3xl font-extrabold text-neutral-900 dark:text-neutral-100"
	SubheadingClass    = "mt-2 text-center text-sm text-neutral-600 dark:text-neutral-300"
)

// Field describes one labelled form input.
type Field struct {
	ID           string
	Type         string
	Label        string
	Placeholder  string
	Autocomplete string
	Value        string
	MinLength    int
	MaxLength    int
	Hint         string
	// Optional drops the required attribute (fields are required by default).
	Optional bool
}
