// Package uikit is the templ component set both app UIs share: the document
// and app shell, buttons, form fields, cards, alerts and toasts, badges,
// tables, confirm dialogs, empty states and skeletons. Colors, radius,
// shadows and fonts come from the design tokens in theme.css, so components
// carry no dark: variants and re-theming never touches Go.
//
// Components take already translated strings and plain values: uikit knows
// nothing about an app's routes, i18n, session or services, and imports
// nothing from apps/ or the data packages (depguard, `make lint-layering`).
// Datastar attributes pass through templ.Attributes.
package uikit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/a-h/templ"
)

// ThemeCookie stores the theme toggle choice ("dark" or "light"). Without it
// the CSS follows prefers-color-scheme.
const ThemeCookie = "theme"

// ThemeFrom reads the theme cookie: "dark", "light" or "" (system).
func ThemeFrom(request *http.Request) string {
	if cookie, err := request.Cookie(ThemeCookie); err == nil && (cookie.Value == "dark" || cookie.Value == "light") {
		return cookie.Value
	}
	return ""
}

// themeToggle flips the effective theme, persists it in the theme cookie and
// updates <html> without a round trip. It runs as a Datastar expression
// (nonced by Datastar), so the strict CSP needs no inline script.
const themeToggle = `const root = document.documentElement;` +
	` const dark = root.classList.contains('dark') || (!root.classList.contains('light') && window.matchMedia('(prefers-color-scheme: dark)').matches);` +
	` root.classList.toggle('dark', !dark); root.classList.toggle('light', dark);` +
	` document.cookie = '` + ThemeCookie + `=' + (dark ? 'light' : 'dark') + '; path=/; max-age=31536000; samesite=lax' + (location.protocol === 'https:' ? '; secure' : '')`

// PostForm is the Datastar action that submits the closest form urlencoded.
func PostForm(path string) string {
	return fmt.Sprintf("@post('%s', {contentType: 'form'})", jsPath(path))
}

// ConfirmPostForm is PostForm behind a browser confirm(message): cancelling
// sends nothing. Prefer ConfirmForm (an accessible <dialog>) for new code.
func ConfirmPostForm(message, path string) string {
	return "confirm(" + jsString(message) + ") && " + PostForm(path)
}

// Post is the Datastar action that posts the page's signals as JSON.
func Post(path string) string {
	return fmt.Sprintf("@post('%s')", jsPath(path))
}

// Get is the Datastar action for a GET SSE stream.
func Get(path string) string {
	return fmt.Sprintf("@get('%s')", jsPath(path))
}

// Assign is the expression that navigates to path (a safe relative path).
func Assign(path string) string {
	return fmt.Sprintf("window.location.assign('%s')", jsPath(path))
}

// jsString quotes s as a JS string literal. JSON is valid JS, and the
// encoder escapes <, > and & too.
func jsString(text string) string {
	data, _ := json.Marshal(text) // a string always encodes
	return string(data)
}

// jsPath keeps a server path safe inside a single-quoted JS string.
func jsPath(path string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, " ", ` `, " ", ` `).Replace(path)
}

// Variant is a button style.
type Variant string

const (
	Primary   Variant = "primary"
	Secondary Variant = "secondary"
	Ghost     Variant = "ghost"
	Danger    Variant = "danger"
	// DangerOutline is a quieter destructive action (row actions, "Delete…").
	DangerOutline Variant = "danger-outline"
)

// Size is a button size.
type Size string

const (
	Medium Size = ""
	Small  Size = "sm"
	// Icon is a square button for a single icon (give it an aria-label).
	Icon Size = "icon"
)

// Tone colors alerts, badges and status icons.
type Tone string

const (
	Neutral Tone = "neutral"
	Info    Tone = "info"
	Success Tone = "success"
	Warning Tone = "warning"
	Error   Tone = "danger"
	Brand   Tone = "brand"
)

// ButtonClass is the class list of a button (or a link styled as one).
func ButtonClass(variant Variant, size Size, full bool) string {
	code := "inline-flex select-none items-center justify-center gap-2 whitespace-nowrap rounded-control text-sm font-medium motion-safe:transition-colors disabled:pointer-events-none disabled:opacity-60 aria-disabled:pointer-events-none aria-disabled:opacity-60"
	switch size {
	case Small:
		code += " h-8 px-3"
	case Icon:
		code += " size-9 shrink-0"
	default:
		code += " h-10 px-4"
	}
	switch variant {
	case Secondary:
		code += " border border-border bg-surface text-foreground shadow-card hover:bg-muted"
	case Ghost:
		code += " text-foreground hover:bg-muted"
	case Danger:
		code += " bg-danger text-danger-foreground shadow-card hover:opacity-90"
	case DangerOutline:
		code += " border border-danger-border bg-surface text-danger-soft-foreground hover:bg-danger-soft"
	default:
		code += " bg-primary text-primary-foreground shadow-card hover:bg-primary-hover"
	}
	if full {
		code += " w-full"
	}
	return code
}

// Shared class lists for markup that is not worth a component.
const (
	// InputClass styles text inputs, selects and textareas.
	InputClass = "block w-full rounded-control border border-input bg-surface px-3 py-2 text-sm text-foreground shadow-card placeholder:text-muted-foreground aria-invalid:border-danger disabled:cursor-not-allowed disabled:opacity-60"
	LabelClass = "block text-sm font-medium text-foreground"
	HintClass  = "mt-1.5 text-xs text-muted-foreground"
	// CheckboxClass styles a checkbox (accent-color follows the brand).
	CheckboxClass = "size-4 shrink-0 rounded border-input accent-primary"
	// LinkClass is an inline text link.
	LinkClass = "font-medium text-primary underline-offset-4 hover:underline"
	// CardClass is a surface panel; Card wraps it.
	CardClass = "rounded-card border border-border bg-surface shadow-card"
	// Table: wrap a <table class={TableClass}> in TableWrap.
	TableClass = "min-w-full divide-y divide-border text-sm"
	ThClass    = "whitespace-nowrap px-4 py-3 text-left text-xs font-semibold uppercase tracking-wide text-muted-foreground"
	TdClass    = "whitespace-nowrap px-4 py-3 text-foreground"
	TrClass    = "hover:bg-muted/60"
	// Container centers page content.
	ContainerClass = "mx-auto w-full max-w-6xl px-4 sm:px-6 lg:px-8"
)

// toneClass is the soft (background + text + border) style of a tone.
func toneClass(tone Tone) string {
	switch tone {
	case Info:
		return "border-info-border bg-info-soft text-info-soft-foreground"
	case Success:
		return "border-success-border bg-success-soft text-success-soft-foreground"
	case Warning:
		return "border-warning-border bg-warning-soft text-warning-soft-foreground"
	case Error:
		return "border-danger-border bg-danger-soft text-danger-soft-foreground"
	case Brand:
		return "border-transparent bg-accent text-accent-foreground"
	default:
		return "border-border bg-muted text-muted-foreground"
	}
}

// dotClass is the solid dot color of a tone.
func dotClass(tone Tone) string {
	switch tone {
	case Success:
		return "bg-success"
	case Warning:
		return "bg-warning"
	case Error:
		return "bg-danger"
	case Info, Brand:
		return "bg-primary"
	default:
		return "bg-muted-foreground"
	}
}

// role is "alert" for tones that need attention now, else "status".
func role(tone Tone) string {
	if tone == Error || tone == Warning {
		return "alert"
	}
	return "status"
}

// ButtonProps configure Button, LinkButton and SubmitButton.
type ButtonProps struct {
	Variant Variant
	Size    Size
	Full    bool
	// Type is the button type; "" means "button" (SubmitButton: "submit").
	Type string
	// Class is appended to the variant's classes.
	Class string
	// Attrs are extra attributes: aria-*, id, name/value, Datastar data-*.
	Attrs templ.Attributes
}

func (props ButtonProps) class() string {
	class := ButtonClass(props.Variant, props.Size, props.Full)
	if props.Class != "" {
		class += " " + props.Class
	}
	return class
}

func (props ButtonProps) typ(def string) string {
	if props.Type != "" {
		return props.Type
	}
	return def
}

// Field describes one labelled input. ID doubles as the name unless Name is set.
type Field struct {
	ID           string
	Name         string
	Type         string
	Label        string
	Placeholder  string
	Autocomplete string
	Value        string
	MinLength    int
	MaxLength    int
	// Inputmode tunes mobile keyboards ("numeric" for one-time codes).
	Inputmode string
	Hint      string
	// Error is a translated message shown under the input; it sets aria-invalid.
	Error string
	// Optional drops the required attribute (fields are required by default).
	Optional bool
	ReadOnly bool
	// Rows makes Textarea taller (default 3).
	Rows int
	// Attrs are extra attributes on the control (Datastar data-*).
	Attrs templ.Attributes
}

func (field Field) name() string {
	if field.Name != "" {
		return field.Name
	}
	return field.ID
}

func (field Field) typ() string {
	if field.Type != "" {
		return field.Type
	}
	return "text"
}

// describedBy links the control to its hint and error.
func (field Field) describedBy() string {
	var ids []string
	if field.Hint != "" {
		ids = append(ids, field.ID+"-hint")
	}
	if field.Error != "" {
		ids = append(ids, field.ID+"-error")
	}
	return strings.Join(ids, " ")
}

// Option is one <option> of a Select.
type Option struct {
	Value, Label string
}

// SelectField is a labelled <select>; Selected is the current value.
type SelectField struct {
	ID, Label, Hint string
	Options         []Option
	Selected        string
	Attrs           templ.Attributes
}

// Checkbox is a labelled checkbox.
type Checkbox struct {
	ID, Name, Value, Label, Hint string
	Checked                      bool
	Attrs                        templ.Attributes
}

// Submit configures SubmitButton: Label shows normally, Loading (with a
// spinner) while the form's Datastar indicator Signal (without "$", for
// example "_loading") is true.
type Submit struct {
	Label, Loading, Signal string
	ButtonProps
}

// NavItem is one link of the app shell's navigation.
type NavItem struct {
	Href    string
	Label   string
	Current bool
}

// Shell configures AppShell. Labels are translated by the caller.
type Shell struct {
	// Brand is the app name in the header; BrandHref where it links.
	Brand, BrandHref string
	// Nav links (empty: no nav and no mobile menu button).
	Nav []NavItem
	// NavLabel names the nav landmark; MenuLabel the mobile menu button;
	// SkipLabel the "skip to content" link.
	NavLabel, MenuLabel, SkipLabel string
	// Actions render at the right of the header on every width (theme,
	// language, sign out). MenuExtra renders at the top of the mobile menu.
	Actions, MenuExtra templ.Component
	// Footer renders under main (optional).
	Footer templ.Component
}

// Document is the <html> root: Page renders it around the app shell.
type Document struct {
	Lang, Title, Description string
	// Theme is "dark", "light" or "" (follow the system).
	Theme string
	// CSS, Script and Favicon are asset URLs (cache-busted by the app).
	CSS, Script, Favicon string
}

// LocaleOption is one entry of LocaleMenu.
type LocaleOption struct {
	Value, Label string
	Selected     bool
}

// Locales configures LocaleMenu: picking an option posts locale=<value> and
// next=<Next> to Action, a plain form the app's handler answers with a 303.
type Locales struct {
	Action, Next, Label, Current string
	Options                      []LocaleOption
}

// Confirm configures ConfirmForm. The trigger opens a modal <dialog>; its
// confirm button submits the form through Datastar. Without JS the trigger
// submits directly (the same as the handler would after confirming).
type Confirm struct {
	// ID is unique on the page; the dialog is ID+"-dialog".
	ID, Action string
	// Signal is the Datastar indicator (without "$") for the in-flight post.
	Signal                                    string
	Title, Message, ConfirmLabel, CancelLabel string
	// Trigger and Variant style the button that opens the dialog.
	Trigger ButtonProps
	Variant Variant
}

// Pager is the pagination row of a list: Summary ("showing x-y of N",
// translated) plus previous/next links (empty URL: disabled).
type Pager struct {
	Summary, PrevURL, NextURL, PrevLabel, NextLabel, Label string
}

// Stat is one figure of a StatGrid.
type Stat struct {
	Label, Value, Hint, TestID string
}

// indicator is the data-indicator attribute for signal ("" adds nothing).
func indicator(signal string) templ.Attributes {
	if signal == "" {
		return nil
	}
	return templ.Attributes{"data-indicator:" + signal: true}
}

func dialogID(confirm Confirm) string { return confirm.ID + "-dialog" }

func openDialog(id string) string {
	return "document.getElementById('" + jsPath(id) + "').showModal()"
}

// initial is the brand mark letter.
func initial(brand string) string {
	for _, char := range strings.TrimSpace(brand) {
		return strings.ToUpper(string(char))
	}
	return "·"
}
