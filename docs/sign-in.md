# Google and Apple sign-in (optional)

Off until configured: each provider turns on when its client id is set, and only then do the login and sign-up pages show its "Continue with ..." button (`GET /api/auth/social-providers` lists them for other clients). It is Better Auth's Google and Apple sign-in in `packages/auth` ([packages/auth README](../packages/auth/README.md#social-sign-in-google-apple)): accounts are linked by email only when both the provider and the user have verified it, an address the provider has not verified gets no account in production, and social sign-in never skips 2FA.

**Callbacks.** The redirect URI to register is always `<PUBLIC_URL>/auth/callback/google` (or `/apple`), built from `PUBLIC_URL`, never from the request. So:
- Register one per environment (local `http://<machine-hostname>:3000`, staging, production). Google accepts `http://` only for `localhost`; for a LAN or Tailscale dev box use an https tunnel or Tailscale Serve and set `PUBLIC_URL` to it. Apple accepts https only.
- Pin `PUBLIC_URL` per environment to the domain you registered. A generated Railway domain can change with the service; the provider then refuses the callback with `redirect_uri_mismatch`.

**Google.**
1. [Google Cloud console](https://console.cloud.google.com/apis/credentials) → OAuth consent screen (app name, support email, scopes `openid email profile`), then Credentials → Create OAuth client ID → **Web application**.
2. Authorised redirect URIs: `<PUBLIC_URL>/auth/callback/google` for each environment.
3. Set `GOOGLE_CLIENT_ID` (plain env) and `GOOGLE_CLIENT_SECRET` (a live secret: `.env`, or Doppler).

**Apple** (paid Apple Developer account):
1. [Certificates, Identifiers & Profiles](https://developer.apple.com/account/resources/identifiers/list): enable *Sign in with Apple* on your App ID, then create a **Services ID** (this is `APPLE_CLIENT_ID`), enable Sign in with Apple on it and add your domain and `<PUBLIC_URL>/auth/callback/apple` as the return URL.
2. Keys → create a key with Sign in with Apple, download the `.p8` once. Its Key ID is `APPLE_KEY_ID`; your Team ID (top right) is `APPLE_TEAM_ID`.
3. Set `APPLE_CLIENT_ID`, `APPLE_TEAM_ID` (plain env), and `APPLE_PRIVATE_KEY` (the `.p8` contents; `\n` for newlines is fine) with `APPLE_KEY_ID` as live secrets. The server signs Apple's client secret from the key and renews it; there is nothing to rotate every six months.

The server refuses to boot with a client id but no credentials. With Doppler, rotating `GOOGLE_CLIENT_SECRET`, or `APPLE_PRIVATE_KEY` together with `APPLE_KEY_ID`, takes effect without a redeploy; a snapshot that would break a configured provider is refused and the old values stay.

**How it runs.** The browser only talks to the UI. The buttons post to the UI action `/auth/social/{provider}`, and the provider comes back to the UI page `/auth/callback/{provider}` (GET, or Apple's cross-site form POST; a page, not a CSRF-checked action, because the single-use OAuth state is what authenticates it). Both call the API in-process through `apiclient` (`SocialProviders`, `StartSocialSignIn`, `FinishSocialSignIn`: public `api.Endpoint`s under `/api/core/v1/auth/social/`), which go through `services.SocialSignInService` and the `server.SocialAuth` adapter to `packages/auth`. Cookies (the flow's signed state, the session) come back as data and the UI sets them.

**Delete it.** Remove `packages/auth/social*.go` and `jwt.go` (and the `Google`, `Apple`, `SocialErrorPath`, `SocialCallbackPath`, `HTTPClient` fields and five routes in `auth.go`/`handler.go`); `apps/web/internal/config/social.go` (and its uses in `config.go`/`secrets.go`, `SocialSignIn`/`SocialCallback` and `SocialProviders`/`SocialStart`/`SocialFinish` in `routes.go`); `services/social.go`, `api/social.go` (with their mocks and tests), `server/social.go`, `validate/social.go`, the social methods in `api/client.go` and `apiclient`; `socialProviders` in `cmd/server/main.go` and the `Social` wiring there and in `server.go`; and the UI's `social.go`, `socialButtons`, `socialErrors` and `pages.login.social` strings. Existing Google/Apple `account` rows can stay or be deleted (`DELETE FROM "account" WHERE "providerId" IN ('google','apple')`); users without a password can set one through "Forgot password".
