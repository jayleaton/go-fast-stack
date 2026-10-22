# apps/web/ui — HTML transport

Server-rendered [templ](https://templ.guide) pages, [Datastar](https://data-star.dev) 1.x actions over SSE, Tailwind v4.
Ported from the original `apps/web-app` (same pages, same Tailwind classes, same messages).

| Package | What |
|---|---|
| `ui` | `ui.New(deps).Routes(mux)`, the `Auth`/`Users` interfaces, handlers, auth + dashboard pages |
| `ui/view` | layout shell (header: logout, theme, language), shared inputs/buttons/alerts/skeletons |
| `ui/welcome` | template welcome landing + `/why` (feature-flagged, deletable; see its README) |
| `ui/i18n` | `messages/{en,ja}.json`, locale negotiation, `i18n.T(ctx, key)` |
| `ui/routes` | every UI path (moves to `apps/web/internal/config` at integration) |
| `ui/static` | embedded `app.css` (generated), pinned `datastar-1.0.4.js`, favicon |
| `ui/uifake` | in-memory `Auth`/`Users` for tests and the preview |
| `ui/cmd/preview` | all pages with fakes on `0.0.0.0:3001` |

## Commands (for the Makefile)

Run from the repo root. Both outputs are committed; CI regenerates them and fails on a diff.

```sh
# templ -> *_templ.go (templ is a go.mod tool directive)
go tool templ generate -path apps/web/ui

# Tailwind standalone CLI v4.3.3, downloaded once into apps/web/ui/bin/ (gitignored), SHA-256 verified
TAILWIND="$(apps/web/ui/scripts/tailwind.sh)"
"$TAILWIND" -i apps/web/ui/styles/input.css -o apps/web/ui/static/app.css --minify

# order matters: templ first, then CSS (Tailwind scans the .templ and .go files for classes)

# preview every page with fakes (demo@example.com / password123; email links are logged)
go run ./apps/web/ui/cmd/preview            # dev behavior: no email verification
go run ./apps/web/ui/cmd/preview -verify    # production behavior

go test -race ./apps/web/ui/...
```

Watch mode while editing: `go tool templ generate -path apps/web/ui -watch` and `"$TAILWIND" ... --watch`.

## Mounting (feat/foundation)

```go
mux := http.NewServeMux()
ui.New(ui.Deps{
    Auth:          uiAuth{a},           // adapter over *auth.Auth, below
    Users:         userService,         // services.Users: UpdateSettings, DeleteAccount
    Features:      ui.Features{EmailVerification: cfg.EmailVerification, TemplateWelcome: cfg.TemplateWelcome},
    SecureCookies: cfg.IsProduction,
    Logger:        log,
}).Routes(mux)
handler := a.Middleware(mux) // the auth middleware must wrap the UI so Auth.UserFrom sees the session
```

`Routes` registers `GET /{$}` and the page/action paths in `ui/routes`, `GET /static/`, and `GET /favicon.ico`.
POST actions are wrapped in `http.CrossOriginProtection` (stdlib); keep that if a global CSRF layer is added.

### Auth adapter

`ui.Auth` has the contract's method set with UI-local types (`ui.User`, `ui.Session`, inputs), so this package does
not import `packages/auth`. The adapter converts types and **wraps errors in the `ui.Err*` sentinels**
(`ErrInvalidCredentials`, `ErrEmailNotVerified`, `ErrEmailTaken`, `ErrPasswordTooShort`, `ErrInvalidToken`,
`ErrInvalidInput`); anything else shows "Something went wrong" and is logged. `UserFrom` maps to `auth.UserFrom`.

Contract gap: `SendVerificationEmail(ctx, email, callbackURL)` backs the "resend verification email" button and the
resend-on-unverified-login behavior of the original. It is not in the v0 contract; Better Auth's equivalent is
`POST /api/auth/send-verification-email`.

`redirectTo`/`callbackURL` are app paths (`/auth/reset-password`, `/auth/verify-email/success`); auth joins them with
its `BaseURL`, so no origin is hardcoded here. The verify link may carry `?token=` (the page POSTs it to
`VerifyEmail`, so link scanners doing GETs cannot burn it) or land after auth's own `GET /api/auth/verify-email`
already set the cookie (the page then just checks the session).

## Patterns

- **Forms**: real `<form method="post" action=...>` plus `data-on:submit="@post(path, {contentType: 'form'})"`.
  Datastar requests (`Datastar-Request: true`) get SSE: patched fragments by id (`#form-error`, `#settings-status`,
  ...) or a redirect. Plain posts (no JS) get the full page with the error, or a 303. Same handler, `u.patch`/`u.redirect`.
- **Loading states**: `data-indicator:_loading` on the form drives the button label and `disabled`; this replaces
  `loading.tsx`/Suspense fallbacks. The dashboard live card shows a pulse skeleton until its stream connects; the
  verify-email success page shows the spinner until its POST returns.
- **Server push**: `GET /dashboard/live` streams the clock and session expiry every second, and patches the account
  card whenever the user changes from any tab (notice shown only in the other tabs). Fan-out is an in-process hub;
  swap for Postgres `LISTEN/NOTIFY` when running more than one instance.
- **Client JS**: only Datastar attributes. Local UI state uses `_`-prefixed signals (not sent to the server). The theme
  toggle writes a `theme` cookie; the server renders `<html class="dark|light">`, and with no cookie the CSS follows
  `prefers-color-scheme`, so there is no flash.
- **i18n**: messages are flat strings with no plurals or placeholders, so `i18n` is a map lookup (no library; add
  go-i18n when a message needs plural rules). Locale = `locale` cookie, then the highest-weighted supported
  `Accept-Language`, then `en`. Keys must be string literals: `TestUsedKeysExistInEveryLocale` scans the templates
  and fails when any locale is missing one, and the render tests fail on any key that renders as itself.
  New keys vs the original (same structure, additions only): `common.home`, `components.themeToggle.*`,
  `pages.home.*`, `pages.dashboard.{settingsForm,deleteAccount,live}.*`, `errors.{emailTaken,invalidInput}`.

## Updating Datastar

Download `bundles/datastar.js` for the new tag from `github.com/starfederation/datastar`, save it as
`static/datastar-<version>.js`, and update `DatastarVersion`, `DatastarFile`, `DatastarSHA256` and the `go:embed` line in
`static/static.go`. `TestDatastarPin` checks the hash and the version header. Keep `datastar-go` on a matching 1.x.
