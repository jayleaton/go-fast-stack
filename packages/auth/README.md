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
| Sign-up | `POST /api/auth/sign-up/email` `{name,email,password,image?,callbackURL?}` → `{token,user}`. Emails are lowercased. Passwords 8–128 characters. When verification is required: no session (`token: null`), a link is emailed, and a duplicate email gets the same success response (no enumeration). Otherwise a duplicate gets `422 USER_ALREADY_EXISTS_USE_ANOTHER_EMAIL`. |
| Sign-in | `POST /api/auth/sign-in/email` `{email,password,callbackURL?}` → `{redirect,token,url?,user}`. Unknown email, missing credential and wrong password all give `401 INVALID_EMAIL_OR_PASSWORD` after the same argon2id work (a dummy hash is checked for unknown emails). Unverified users get `403 EMAIL_NOT_VERIFIED` when verification is required, but only after the password matched. |
| Sessions | DB rows, 7-day TTL, extended once a day on use, checked on every request (no cookie cache, so revocation is immediate). Expired rows are deleted on read. |
| Sign-out | `POST /api/auth/sign-out` → `{success:true}`. Deletes the row and clears the cookie. |
| Get session | `GET /api/auth/get-session` → `{session,user}` or `null`, `Cache-Control: no-store`. |
| Verification | `GET /api/auth/verify-email?token=&callbackURL=`: stateless HS256 JWT (`{email,iat,exp}`, 24 h), same format as Better Auth. Verifies, signs in (autoSignInAfterVerification) and redirects to `callbackURL`. Errors redirect with `?error=CODE`, or give `401 {code}` without a callback. After the email is verified, the link no longer signs anyone in. `POST /api/auth/send-verification-email` `{email,callbackURL?}` resends it. |
| Password reset | `POST /api/auth/request-password-reset` `{email,redirectTo}` always gives the same response. The emailed link `GET /api/auth/reset-password/{token}?callbackURL=` redirects to `redirectTo?token=` (or `?error=INVALID_TOKEN`). `POST /api/auth/reset-password` `{token,newPassword}`: in one transaction, the token is consumed (single use, 1 h), every session of the user is revoked and the password is set. |
| Bearer | `Authorization: Bearer <token>` with the raw session token (response body) or the signed token (`set-auth-token` header), on the same session rows. Bearer wins over the cookie. |
| CSRF | Better Auth's origin check. State-changing requests that carry cookies need an `Origin`/`Referer` (or Expo's `expo-origin`) matching `BaseURL` or `TrustedOrigins`. Sign-in/up also use Fetch Metadata: a cross-site navigation is blocked and browser requests need a trusted origin. Cookie-less clients (mobile, bearer) cannot be forged by a browser and pass. `callbackURL`/`redirectTo` must be trusted or a safe relative path (open-redirect protection). POST bodies must be `application/json`. |
| Rate limits | Per client IP and endpoint, Better Auth's defaults: sign-in/sign-up 3 per 10 s, request-password-reset/send-verification-email 3 per 60 s, reset-password 3 per 10 s. `429 {code:"TOO_MANY_REQUESTS"}` with `X-Retry-After`. In-memory by default; pass any `RateLimiter` (e.g. Valkey) for several instances. |
| Mail | `Mailer` interface. `NewDevMailer` logs the links, `NewResendMailer(apiKey, from)` sends through the Resend HTTP API with the original templates (`templates/`). Emails are sent in the background, so response times don't reveal whether an address exists. Call `Shutdown` after the HTTP server has stopped to let them finish. |

JSON endpoints return Better Auth shapes (`{"code","message"}` on error), not the
app envelope, because the original API used Better Auth's own responses there.
`RequireUser` answers `401 {"data":null,"error":"Unauthorized"}` like the
original route wrapper, or redirects HTML requests (`Accept: text/html`) to
`Config.LoginPath`.

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
`SendVerificationEmail`) and then `SetSessionCookie` / `ClearSessionCookie`. They
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
- **Verification links** are stateless and stay valid for 24 h, but they only
  sign someone in while the email is still unverified (the update is
  atomic, so concurrent clicks yield one session). As in Better Auth, a GET
  link that signs in can be used for login CSRF into the attacker's own
  account.
- **Client IP**: set `ClientIPHeader` only behind a proxy that writes that
  header. The right-most value is used (the one the proxy appended).
  Otherwise the TCP peer is used.
- **Not ported yet**: change-email tokens (`updateTo`, rejected), `rememberMe`
  (not used by the original UI), cookie cache, secondary storage, form-encoded
  sign-in.

## Tests

```sh
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
  go test -race ./packages/auth/...
```

The suite creates and drops its own database (`auth_test_<pid>_…`) on that
server. Without a URL the database tests skip, and under `CI` they fail.
Fuzz targets: `FuzzVerifySigned`, `FuzzParseBearer`,
`FuzzParseVerificationToken` (`go test -fuzz FuzzParseBearer ./packages/auth`).
