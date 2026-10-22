# packages/auth

A Go port of the Better Auth core (1.7) used by the original Next.js template:
email + password accounts, email verification, password reset and database
sessions. It keeps Better Auth's table names and columns, endpoint paths, JSON
shapes, error codes, cookie name and signing format. Data from the original app
carries over, the Expo client keeps working, and Better Auth features can be
ported one at a time.

## What it implements

| Area | Behaviour |
|---|---|
| Tables | `user`, `session`, `account`, `verification` with Better Auth columns (`timestamptz`). Own goose migrations in `migrations/`, version table `goose_auth`, own advisory lock. |
| Sign-up | `POST /api/auth/sign-up/email` `{name,email,password,image?,callbackURL?}` → `{token,user}`. Emails are lowercased. Passwords 8–128 characters. When verification is required: no session (`token: null`), a link is emailed, and a duplicate email gets the same success response (no enumeration). A duplicate of a still-unverified account replaces its password and emails a new link, which voids the old one (see security notes). Otherwise a duplicate gets `422 USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL`. |
| Sign-in | `POST /api/auth/sign-in/email` `{email,password,callbackURL?}` → `{redirect,token,url?,user}`. Unknown email, missing credential and wrong password all give `401 INVALID_EMAIL_OR_PASSWORD` after the same argon2id work (a dummy hash is checked for unknown emails). Unverified users get `403 EMAIL_NOT_VERIFIED` when verification is required, but only after the password matched. |
| Sessions | DB rows, 7-day TTL, extended once a day on use, checked on every request (no cookie cache, so revocation is immediate). Expired rows are deleted on read. |
| Sign-out | `POST /api/auth/sign-out` → `{success:true}`. Deletes the presented session (the bearer one and the cookie one when both are sent) and clears the cookie. |
| Get session | `GET /api/auth/get-session` → `{session,user}` or `null`, `Cache-Control: no-store`. |
| Verification | `GET /api/auth/verify-email?token=&callbackURL=`: stateless HS256 JWT (`{email,iat,exp}`, 24 h), same format as Better Auth. Verifies, signs in (autoSignInAfterVerification) and redirects to `callbackURL`. Errors redirect with `?error=CODE`, or give `401 {code}` without a callback. With `Config.VerifyEmailPath` set (the web app sets its verify page), links whose `callbackURL` is relative (default `/`) go to `<BaseURL><VerifyEmailPath>?token=&callbackURL=` instead: that page spends the token with `VerifyEmail` (same behaviour as the GET endpoint, change-email links included), sets the session cookie and continues to `callbackURL`, so a browser never opens the JSON API. Absolute and deep-link callbacks (mobile) keep the GET endpoint. After the email is verified, the link no longer signs anyone in. `POST /api/auth/send-verification-email` `{email,callbackURL?}` resends it. |
| Password reset | `POST /api/auth/request-password-reset` `{email,redirectTo}` always gives the same response. `redirectTo` is required, because without it the emailed link would lead nowhere. A relative `redirectTo` (an app page) is emailed directly as `<BaseURL><redirectTo>?token=` (its query kept); the page spends the token in-process with `ResetPassword`. An absolute or deep-link `redirectTo` (mobile, other trusted origins) gets `GET /api/auth/reset-password/{token}?callbackURL=`, which redirects to `redirectTo?token=` (or `?error=INVALID_TOKEN`). `POST /api/auth/reset-password` `{token,newPassword}`: in one transaction, the token is consumed (single use, 1 h), every session and every other outstanding reset link of the user is revoked, and the password is set. |
| Change password | `POST /api/auth/change-password` `{currentPassword,newPassword,revokeOtherSessions?}` → `{token:null,user}`. Needs a session. A wrong current password gives `400 INVALID_PASSWORD`, and a user without a password (OAuth only) `400 CREDENTIAL_ACCOUNT_NOT_FOUND`. Outstanding reset links stop working. `revokeOtherSessions` signs out every other session but keeps the current one, so unlike Better Auth no new token is issued. The update is conditional on the hash just verified, so of two concurrent changes only one wins. |
| Change email | `POST /api/auth/change-email` `{newEmail,callbackURL?}` → `{status:true}`. Needs a session. A link (Better Auth's `change-email-verification` JWT, 24 h) goes to the new address and a notice to the current one. The email changes, and becomes verified, only when the link is opened (`GET /api/auth/verify-email`, or the `VerifyEmailPath` page for a relative callback), which also signs the opener in and returns the updated user. The link names its user and a one-time nonce stored as a `verification` row. Only the newest link of a user works, and only once. A password change, a reset or revoke-other-sessions cancels it. Opened in a browser signed in as someone else, it gives `INVALID_USER` instead of switching accounts. The session must be fresh (created within `FreshAge`, default 1 day), else `403 SESSION_NOT_FRESH`. Reset links sent to the old address stop working. A taken address gives `422 USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL` when verification is off. When it is required, the request succeeds silently and sends nothing, as with sign-up. The address is checked again, atomically, when the link is opened. The same address gives `400 EMAIL_IS_THE_SAME`. |
| Sessions | `GET /api/auth/list-sessions` → the caller's active sessions, newest first, **without tokens**. `POST /api/auth/revoke-session` `{id}` (or Better Auth's `{token}`) revokes one of the caller's own sessions; another user's session gives `404 SESSION_NOT_FOUND`. Revoking the current session also clears its cookie. `POST /api/auth/revoke-other-sessions` keeps only the caller's session. |
| Bearer | `Authorization: Bearer <token>` with the raw session token (response body) or the signed token (`set-auth-token` header), on the same session rows. Bearer wins over the cookie. |
| CSRF | Better Auth's origin check. State-changing requests that carry cookies need an `Origin`/`Referer` (or Expo's `expo-origin`) matching `BaseURL` or `TrustedOrigins`. Sign-in/up also use Fetch Metadata: a cross-site navigation is blocked and browser requests need a trusted origin. Cookie-less clients (mobile, bearer) cannot be forged by a browser and pass. `callbackURL`/`redirectTo` must be trusted or a safe relative path (open-redirect protection). POST bodies must be `application/json`. |
| Rate limits | Per client IP (IPv6 reduced to its /64, IPv4-mapped addresses unmapped, as Better Auth does) and endpoint, Better Auth's defaults: sign-in/sign-up 3 per 10 s, request-password-reset/send-verification-email/change-email 3 per 60 s, reset-password/change-password 3 per 10 s. change-password and change-email are also limited per user, so guessing the current password from many IPs gains nothing. `429 {code:"TOO_MANY_REQUESTS"}` with `X-Retry-After`. In-memory by default; pass any `RateLimiter` (e.g. Valkey) for several instances. |
| Mail | `Mailer` interface. `NewDevMailer` logs the links, `NewResendMailer(apiKey, from)` sends through the Resend HTTP API with the original templates (`templates/`), plus the email-change notice. `NewSwapMailer` wraps either so a rotated API key can swap in a new `ResendMailer` while the server runs. Emails are sent in the background, so response times don't reveal whether an address exists. Call `Shutdown` after the HTTP server has stopped to let them finish. |

JSON endpoints return Better Auth shapes (`{"code","message"}` on error), not the
app envelope, because the original API used Better Auth's own responses there.
`RequireUser` answers `401 {"data":null,"error":"Unauthorized"}` like the
original route wrapper, or redirects HTML requests (`Accept: text/html`) to
`Config.LoginPath`.

## Two-factor, admin and invites

Ports of Better Auth's `twoFactor` and `admin` plugins and an invite flow; the full Go API is in [ADMIN.md](ADMIN.md).

| Area | Behaviour |
|---|---|
| 2FA (opt-in: `Config.TwoFactor`) | TOTP (RFC 6238, SHA-1, 6 digits, 30 s, ±1 step) with 10 single-use backup codes. `POST /two-factor/enable {password}` → `{totpURI, secret, backupCodes}`; the first `POST /two-factor/verify-totp {code}` turns it on. Sign-in then answers `{"twoFactorRedirect": true, "twoFactorMethods": [...]}` with a `<prefix>.two_factor` cookie (10 min, single use, 5 attempts) instead of a session; `verify-totp` / `verify-backup-code {code, trustDevice?}` finish it. A TOTP code is accepted once (the last used time step is stored), 10 failures lock the second factor for 15 minutes, `trustDevice` skips 2FA on that browser for 30 days. `disable` and `generate-backup-codes` need the password. With `Required`, users without 2FA get `twoFactorSetupRequired: true` and must enrol (`enable` with the challenge cookie, then `verify-totp`) before their first session. Email links (verification, change-email) never sign in past 2FA; a reset forgets trusted devices. |
| Bans and roles | `user.role` (default `user`), `banned`, `banReason`, `banExpires`. `UserAdmin` (`a.Users()` or `NewUserAdmin(pool)`, which needs no secret): `ListUsers` (search, filter, sort, paginate), `GetUserDetail`, `BanUser` (revokes sessions), `UnbanUser`, `SetRole`, `RevokeSession(s)`, `ResetTwoFactor` (recovery). Banned users get `403 BANNED_USER` after their password matched, and their sessions stop working; expired bans lift themselves. Methods only: there are no `/admin/*` endpoints, and authorisation belongs to the caller. |
| Invites | `Config.DisableSignUp` turns public sign-up off (`400 EMAIL_PASSWORD_SIGN_UP_DISABLED`); invites keep working. `CreateUserWithoutPassword` + `IssueSetPasswordLink` email a single-use "Set your password" link (`InviteTTL`, default 72 h; a newer link replaces older ones), shaped like a reset link (relative `redirectTo` linked directly). The page posts `POST /set-password {token, newPassword}` (`CompleteSetPassword`): sets the password, verifies the email and signs in, or continues into 2FA enrolment when it is required. |
| Several instances | `CookiePrefix` (cookies `<prefix>.session_token`, `.two_factor`, `.trust_device`), own `Secret`, `MigrateTable` for a separate goose table. Request data is tagged per instance, so nested middleware never mixes sessions. |

Security notes for this part: TOTP secrets are AES-256-GCM encrypted with a key derived from `AUTH_SECRET` (user id as associated data) and backup codes are stored as keyed hashes bound to the user, so a database dump alone yields neither; rotating `AUTH_SECRET` therefore invalidates every enrolment (recover users with `UserAdmin.ResetTwoFactor`). Trusted-device cookies are not rotated on use (Better Auth rotates them); disable 2FA, reset the password or revoke other sessions to forget them. The per-challenge attempt limit uses the configured `RateLimiter` (in-memory by default); the per-user lockout is in the database.

## Usage

```go
if err := auth.Migrate(ctx, pool); err != nil { ... } // before app migrations
a, err := auth.New(auth.Config{
    Secret:                   cfg.AuthSecret,
    BaseURL:                  cfg.PublicURL,
    RequireEmailVerification: cfg.Production,
    TrustedOrigins:           auth.MobileTrustedOrigins(cfg.Production),
    ClientIPHeader:           "X-Forwarded-For", // only behind a proxy that sets it
}, pool, mailer)

mux.Handle("/api/auth/", a.Handler())          // do not strip the prefix
mux.Handle("GET /dashboard", a.RequireUser(dashboard))
srv.Handler = a.Middleware(mux)                // loads the session for UserFrom
```

HTML handlers call the service methods (`SignInEmail`, `SignUpEmail`,
`SignOut`, `RequestPasswordReset`, `ResetPassword`, `VerifyEmail`,
`SendVerificationEmail`) and then `SetSessionCookie` / `ClearSessionCookie`.
Account management (`ChangePassword`, `ChangeEmail`, `ListSessions`,
`RevokeSession`, `RevokeOtherSessions`) takes the caller's user id, and the
current session id where it matters, from `UserFrom`. These methods only ever
touch that user's rows, so passing another id cannot affect someone else's
password or sessions. They
apply the same rules as the JSON endpoints. Rate limiting and the session's
IP/user agent come from the request context, so those routes must run behind
`Middleware`. Without it, nothing is rate limited. The origin check belongs to
the transport: HTML form routes need their own CSRF protection.

`Middleware` queries the database once per request that carries a session
token. Don't wrap static file routes with it.

## Adding a Better Auth feature (2FA, passkeys, OAuth, organisations)

1. Read the feature in Better Auth's source (`better-auth/dist/plugins/<name>`,
   `api/routes/*`) and keep its table names, columns, endpoint paths, request
   and response shapes and error codes.
2. Put it in a sub-package (`packages/auth/twofactor`, ...) with its own
   migrations embedded and its own goose table (`goose_auth_twofactor`), run
   after `auth.Migrate`. Core tables change only through new migrations here.
3. Build on the core: `UserFrom` for the current user, the `Error` type for
   coded errors. Where the core is missing a hook (for example "second factor
   required" after the password check in `SignInEmail`), add a small
   exported extension point to this package instead of copying flows.
4. OAuth providers fit the existing `account` table (`providerId`,
   `accountId`, tokens). `ResetPassword` already creates a credential account
   for users who don't have one.
5. Test against real Postgres like `flows_test.go`, including the reverse
   states (disable, revoke, expired, reused).

## Security notes

- **Passwords**: argon2id, 64 MiB, t=3, p=4 (RFC 9106), PHC string format.
  Parameters live in each hash, so you can raise them without a migration.
  Hashes are compared in constant time. Unknown emails are verified against
  a dummy hash. Users imported from the original app keep Better Auth's
  scrypt hashes (`salt:key`, N=16384 r=16 p=1). Those still verify and are
  rehashed to argon2id on their next sign-in. Until then, a wrong-password
  attempt on such an account takes scrypt time instead of argon2id time.
- **Tokens**: session ids and tokens are 32 random `[a-zA-Z0-9]` characters,
  reset tokens 24, all from `crypto/rand` without modulo bias. Session tokens
  are stored in plaintext like Better Auth, so the database is
  credential-equivalent: protect backups.
- **Cookie**: `better-auth.session_token` (`__Secure-` prefixed on https).
  The value is `token.base64(HMAC-SHA256(secret, token))`, URL-encoded, the
  better-call format (known-answer tested against Node). HttpOnly,
  SameSite=Lax, Path=/, Secure on https, Max-Age until session expiry.
- **Secret**: `AUTH_SECRET` (at least 32 bytes) signs cookies and verification
  JWTs. Rotating it signs everyone out and invalidates pending verification
  links. Reset links are database rows and survive rotation.
- **Verification links** are stateless JWTs that stay valid for 24 h, but they
  only sign someone in while the email is still unverified. The update locks
  the user row, so concurrent clicks yield one session. Verifying revokes the
  account's other sessions. As in Better Auth, a GET link that signs in can be
  used for login CSRF into the attacker's own account.
- **Squatted sign-ups** (pre-account takeover): with verification required,
  someone can register another person's address with their own password.
  Every link carries a `cred` claim, a keyed fingerprint of the password hash
  it was issued for, and only verifies while that hash is current. When the
  real owner signs up, their password replaces the squatter's and the
  squatter's link stops working. One residual case remains: if the squatter
  signs up again *after* the owner, the owner's inbox gets a link for the
  squatter's password. Clicking that newest link verifies the account under a
  password the owner doesn't know, and a password reset recovers it.
- **Client IP**: set `ClientIPHeader` only behind a proxy that writes that
  header. The right-most value is used (the one the proxy appended), and an
  invalid value falls back to the TCP peer.
- **Origin `null`** is accepted only together with `Sec-Fetch-Site: same-origin`,
  as in Better Auth. Host-only wildcard origins (`*.example.com`) match
  http(s) URLs only.
- **Account changes**: change-email needs a fresh session but no password,
  like Better Auth's freshness rule. A session stolen within its first day
  can therefore still start a change to the thief's address. The old address
  gets a notice, and changing the password, resetting it or revoking other
  sessions cancels the pending link. If the thief opens the link first, the
  account moves to their address, so apps that want more can ask for the
  password in their service layer before calling `ChangeEmail`. Sessions are
  not revoked on an email change. Use `RevokeOtherSessions`, or
  `revokeOtherSessions` on a password change. `ChangeEmail` called without a
  session in the context (trusted server code) skips the freshness check.
- **Not ported yet**: Better Auth's other change-email variants
  (`change-email-confirmation` and the legacy unverified update; their tokens
  are rejected), `rememberMe` (not used by the original UI), cookie cache,
  secondary storage, form-encoded sign-in, delete-user (the app's
  `DeleteAccount` deletes the `user` row; sessions and accounts cascade).

## Tests

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
  go test -race ./packages/auth/...
```

The suite creates and drops its own database (`auth_test_<pid>_…`) on that
server. Without a URL the database tests skip, and under `CI` they fail.
Fuzz targets: `FuzzVerifySigned`, `FuzzParseBearer`,
`FuzzParseVerificationToken` (`go test -fuzz FuzzParseBearer ./packages/auth`).
