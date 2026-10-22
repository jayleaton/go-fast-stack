# packages/auth: 2FA, admin and invite API (contract for apps/admin)

Ports of Better Auth's `twoFactor` and `admin` plugins plus an invite / set-password flow.
These signatures are stable: code against them now. Changes will be additive and listed here.
Package: `github.com/jayleaton/go-fast-stack/packages/auth`.

## Running a second instance (admin app)

```go
// Admin DB: its own pool, migrations, cookies and secret.
err := auth.MigrateTable(ctx, adminPool, "goose_auth") // any table name; Migrate(ctx, pool) == MigrateTable(ctx, pool, "goose_auth")
admins, err := auth.New(auth.Config{
    Secret:       cfg.AdminAuthSecret,            // ADMIN_AUTH_SECRET, never the app's
    BaseURL:      cfg.AdminPublicURL,
    CookiePrefix: "admin",                        // cookies admin.session_token, admin.two_factor, admin.trust_device
    AppName:      "Acme Admin",                   // TOTP issuer
    RequireEmailVerification: false,              // invites prove the address
    DisableSignUp: true,                          // admins only arrive by invite
    TwoFactor:    &auth.TwoFactorConfig{Required: cfg.AdminRequire2FA},
}, adminPool, mailer)

// App users (the app DB): user administration needs no secret or cookies.
appUsers := auth.NewUserAdmin(appPool)
```

Instances share nothing mutable: each has its own rate limiter, background-mail queue, cookies and secret.
Nesting is safe: an admin `Handler()` or `RequireUser` mounted inside the main app's `Middleware` only sees the admin
instance's own session (request data is tagged with the instance that loaded it). `UserFrom(ctx)` returns the innermost
instance's user.

## Config additions

```go
type Config struct {
    // ...existing fields...
    CookiePrefix string           // cookie name prefix, default "better-auth" ("<prefix>.session_token", "__Secure-" on https)
    AppName      string           // TOTP issuer default; default: BaseURL host
    TwoFactor    *TwoFactorConfig // nil: 2FA off (endpoints 404, sign-in unchanged)
    InviteTTL    time.Duration    // set-password links, default 72h
    DisableSignUp bool            // public sign-up off: SignUpEmail / POST /sign-up/email return ErrSignUpDisabled; invites still work
}

type TwoFactorConfig struct {
    Issuer         string        // otpauth issuer, default Config.AppName
    Required       bool          // users without 2FA must enrol before they get a session
    TrustDeviceTTL time.Duration // "trust this device" lifetime, default 30 days; negative disables it
}

func MigrateTable(ctx context.Context, pool *pgxpool.Pool, table string) error
```

## Two-factor (TOTP + backup codes)

```go
// SignInEmail (and CompleteSetPassword) return a *TwoFactorChallenge as the error
// when a second factor (or, with Required, enrolment) is needed. No session exists yet.
type TwoFactorChallenge struct {
    Token         string    // opaque; HTML: put it in the cookie with SetTwoFactorCookie
    Methods       []string  // "totp", "backup_code"; empty when SetupRequired
    SetupRequired bool      // user must enrol first: StartTwoFactorSetup, then VerifyTwoFactor
    ExpiresAt     time.Time // 10 minutes
}
func (c *TwoFactorChallenge) Error() string
func (a *Auth) SetTwoFactorCookie(w http.ResponseWriter, c *TwoFactorChallenge)

type VerifyTwoFactorInput struct {
    Code           string
    Method         string // "totp" (default) or "backup_code"
    TrustDevice    bool   // skip 2FA on this device for TwoFactorConfig.TrustDeviceTTL
    ChallengeToken string // empty: taken from the request's cookie (Middleware)
}
type TwoFactorResult struct {
    User       User
    Session    Session
    TrustToken string // set when TrustDevice was asked for: SetTrustDeviceCookie(w, TrustToken)
}
func (a *Auth) VerifyTwoFactor(ctx context.Context, in VerifyTwoFactorInput) (TwoFactorResult, error)
func (a *Auth) SetTrustDeviceCookie(w http.ResponseWriter, token string)

// Enrolment
type TOTPSetup struct {
    Secret      string   // base32, for manual entry
    URI         string   // otpauth://totp/... for a QR code
    BackupCodes []string // shown once; stored hashed
}
func (a *Auth) EnableTwoFactor(ctx context.Context, userID, password string) (TOTPSetup, error) // signed in; 2FA becomes active after ConfirmTwoFactor
func (a *Auth) ConfirmTwoFactor(ctx context.Context, userID, code string) error                 // first valid code enables 2FA
func (a *Auth) StartTwoFactorSetup(ctx context.Context, challengeToken string) (TOTPSetup, error) // SetupRequired challenge ("" = cookie); finish with VerifyTwoFactor
func (a *Auth) DisableTwoFactor(ctx context.Context, userID, password string) error
func (a *Auth) GenerateBackupCodes(ctx context.Context, userID, password string) ([]string, error) // replaces all codes
func (a *Auth) TwoFactorEnabled(ctx context.Context, userID string) (bool, error)
```

JSON endpoints (Better Auth paths, mounted with `Handler()`):
`POST /two-factor/enable {password}` → `{totpURI, backupCodes}` (with a SetupRequired challenge cookie no password is needed),
`POST /two-factor/get-totp-uri {password}` → `{totpURI}`,
`POST /two-factor/verify-totp {code, trustDevice?}` → `{token, user}` (signed in: confirms enrolment),
`POST /two-factor/verify-backup-code {code, trustDevice?}` → `{token, user}`,
`POST /two-factor/generate-backup-codes {password}` → `{status, backupCodes}`,
`POST /two-factor/disable {password}` → `{status}`.
Sign-in needing 2FA answers `200 {"twoFactorRedirect": true, "twoFactorMethods": [...], "twoFactorSetupRequired": bool}` and sets `<prefix>.two_factor`.

## Invite / set password

```go
type CreateUserInput struct {
    Name, Email string
    Role        string // default "user"
}
func (a *Auth) CreateUserWithoutPassword(ctx context.Context, in CreateUserInput) (User, error) // unverified until the invite is completed (the link proves the inbox)
func (a *Auth) IssueSetPasswordLink(ctx context.Context, userID, redirectTo string) error       // emails "Set your password"; single use, InviteTTL
func (a *Auth) CompleteSetPassword(ctx context.Context, token, newPassword string) (User, Session, error) // may return *TwoFactorChallenge{SetupRequired: true}
```

The emailed link is built like a reset link: a relative `redirectTo` (the admin app's `/set-password`) is linked directly as
`<BaseURL><redirectTo>?token=`, so the browser never touches the JSON API; an absolute or deep-link `redirectTo` gets
`GET /api/auth/reset-password/{token}?callbackURL=<redirectTo>` (redirects to `redirectTo?token=`). The page calls `CompleteSetPassword` or posts
`POST /api/auth/set-password {token, newPassword}` → `{token, user}` or the 2FA challenge body above. `ResetPassword` also accepts these tokens.
`Mailer` gains `SendSetPasswordEmail(ctx, to, url string) error`.

## User administration (admin plugin fields: role, banned, banReason, banExpires)

```go
func NewUserAdmin(pool *pgxpool.Pool) *UserAdmin
func (a *Auth) Users() *UserAdmin

type AdminUser struct {
    User
    Role             string
    Banned           bool
    BanReason        *string
    BanExpires       *time.Time // nil: permanent
    TwoFactorEnabled bool
    HasPassword      bool
}
type ListUsersQuery struct {
    Search      string // case-insensitive contains
    SearchField string // "email" (default) or "name"
    Role        string // filter, optional
    Banned      *bool  // filter, optional
    SortBy      string // "createdAt" (default), "email", "name"
    SortDesc    bool
    Limit       int    // default 25, max 100
    Offset      int
}
type UserPage struct {
    Users         []AdminUser
    Total         int
    Limit, Offset int
}
type UserDetail struct {
    User     AdminUser
    Sessions []Session // active, newest first, no tokens
}

func (u *UserAdmin) ListUsers(ctx context.Context, q ListUsersQuery) (UserPage, error)
func (u *UserAdmin) GetUserDetail(ctx context.Context, userID string) (UserDetail, error)
func (u *UserAdmin) BanUser(ctx context.Context, userID, reason string, duration time.Duration) error // 0 = permanent; revokes all sessions
func (u *UserAdmin) UnbanUser(ctx context.Context, userID string) error
func (u *UserAdmin) SetRole(ctx context.Context, userID, role string) error
func (u *UserAdmin) RevokeSession(ctx context.Context, userID, sessionID string) error
func (u *UserAdmin) RevokeUserSessions(ctx context.Context, userID string) error
func (u *UserAdmin) ResetTwoFactor(ctx context.Context, userID string) error // recovery: removes the enrolment; Required re-enrols at next sign-in
```

Banned users: sign-in (after the password matched) and every session lookup refuse them; a ban whose `banExpires` has passed lifts itself.
Admin authorisation (who may call these) belongs to the admin app's services; these methods do not check the caller.

`ADMIN_AUTH_SECRET` rotates like the app's `AUTH_SECRET`: pass the old values as `Config.PreviousSecrets` (a leaked one as
`Config.RevokedSecrets`), or call `Auth.SetSecrets(ctx, auth.Secrets{...})` from a secrets hook, and nobody is signed out. TOTP secrets and backup codes are
protected by data keys stored in the instance's own database ("authKey", per `CookiePrefix`) and only wrapped by the
secret, so a rotation rewraps them and 2FA keeps working. `UserAdmin.ResetTwoFactor` is the way back for an admin who
lost their authenticator and backup codes.
Turning `TwoFactor` off on an instance skips 2FA even for users who enrolled.

## Events (audit hooks)

`Config.Events` (an `EventHandler`, `func(ctx, Event)`) receives what happened, after it happened. Every kind is an `EventKind` constant:

| Kind | When | Notable fields |
|---|---|---|
| `sign_in` | a session was created | `Method`: `password`, `two_factor` (`Details["factor"]` totp / backup_code), `set_password`, `sign_up`, `email_verification`, `email_change`; `SessionID` |
| `sign_in_failed` | sign-in refused, or a correct second factor that still got no session (spent challenge, ban) | `Reason` (error code); `UserID`/`Email` only when the email matched an account |
| `two_factor_succeeded` / `two_factor_failed` | a second-factor or enrolment code accepted / refused | `UserID` from the pending-2FA state; `Method` totp / backup_code; `Reason`. Attempts without a valid pending sign-in are not reported (no user to name) |
| `two_factor_enabled` / `two_factor_disabled` | enrolment confirmed / removed (`Details["by"]="admin"` for `ResetTwoFactor`) | |
| `sign_out` | the user ended their session | `SessionID` |
| `session_revoked` | sessions ended other than by signing out | `Details["count"]`, `Details["by"]`: `user`, `password_change`, `password_reset` (reset and invite links), `email_verification`, `sign_up` (an unverified account reclaimed), `admin`, `ban`; `SessionID` for a single one |
| `password_changed` / `password_reset` | `ChangePassword` / a reset or invite link (`Method` reset / set_password) | |
| `email_change_requested` / `email_changed` | `ChangeEmail` (`Details["newEmail"]`, `outcome=address_taken` when silently ignored) / the link was opened (`oldEmail`, `newEmail`) | |
| `user_banned` / `user_unbanned` | `UserAdmin.BanUser` (`reason`, `expiresAt`) / `UnbanUser` | |

- **Never secrets:** events carry ids, never a password, token, TOTP secret or code. `Email` is the account's address when the event happened; where the emitter didn't have it, it is looked up before delivery. A refused sign-in for an unknown address carries no email, because people type passwords into email fields.
- **Not reported:** rate-limited attempts (`TOO_MANY_REQUESTS`) are not reported. They never reached a credential check and would crowd real events out of the queue.
- **Request metadata:** `IP` and `UserAgent` come from the instance's own request data, with `ClientIPHeader` rules. Calls outside `Middleware`, and a standalone `UserAdmin`, have none.
- **Delivery:** one worker per instance delivers events in order.
  - Each call gets a fresh context (no request values, so no session token can reach the handler) that expires after 5 s.
  - The handler cannot block or fail auth: the queue holds 1024 events, a full queue drops new ones with a warning, and a panic is recovered and logged while the worker carries on.
  - Drops are visible: `Auth.DroppedEvents()` (and `UserAdmin.DroppedEvents()`) count them for a metric or an alert, and the first drop and every 100th after it are logged at warn level with `dropped_total`.
  - Emitting never waits, even while `Shutdown` is draining or when the handler calls back into Auth.
- **Best-effort:** events still queued when the process exits are lost. `Auth.Shutdown` waits for queued events (without blocking new ones), so call it on graceful shutdown, and in tests before asserting.
- **User administration:** `a.Users()` reports to the instance's handler, and its `Shutdown` drains that queue. `NewUserAdmin(pool).WithEvents(h)` opts a standalone `UserAdmin` in. Call `WithEvents` once at startup (each call starts a worker), and call `UserAdmin.Shutdown` on graceful shutdown.
- **Attribution:** each event names its user, so audit attribution is exact however many instances run against one database. The admin app audits sign-ins, sign-outs and failed 2FA this way.

## Errors (errors.Is)

`ErrBannedUser` (403 BANNED_USER), `ErrInvalidCode` (401 INVALID_CODE), `ErrInvalidBackupCode` (401 INVALID_BACKUP_CODE),
`ErrInvalidTwoFactorCookie` (401 INVALID_TWO_FACTOR_COOKIE), `ErrTwoFactorNotEnabled` (400 TWO_FACTOR_NOT_ENABLED),
`ErrTOTPAlreadyEnabled` (400 TOTP_ALREADY_ENABLED), `ErrTwoFactorLocked` (429 ACCOUNT_TEMPORARILY_LOCKED),
`ErrInvalidRole` (400 INVALID_ROLE_TYPE), `ErrSignUpDisabled` (400 EMAIL_PASSWORD_SIGN_UP_DISABLED), plus the existing `ErrUserNotFound`, `ErrInvalidPassword`, `ErrInvalidToken`, `ErrSessionNotFound`.
