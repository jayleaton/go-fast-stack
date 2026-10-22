# mobile-app

Expo (React Native) app for this template. It signs in against the Go
server's Better Auth–compatible endpoints (`packages/auth`, mounted at
`/api/auth/*`) and calls the JSON API (`/api/core/v1`) with its
`{ data, error }` envelope.

- **Framework**: Expo SDK 57, Expo Router (typed routes), React Compiler
- **Auth**: `better-auth` + `@better-auth/expo`, unchanged. The Go server keeps
  Better Auth's paths, JSON shapes, cookie name and signing format, and trusts
  the `expo-origin` header, so the stock Expo client works. The session cookie
  lives in the device keychain (`expo-secure-store`).
- **API**: `src/lib/api/client.ts` attaches the session cookie to requests and
  returns the envelope. Response types are in `src/lib/api/types.ts`; keep them
  in sync with `apps/web/internal/api`.

This folder is a standalone pnpm project (its own `package.json` and
`pnpm-lock.yaml`). Nothing outside it needs Node.

The planned successor is a pair of native apps (Swift for iOS, Kotlin for
Android) against the same JSON API. This app stays the reference client until
then, so keep it working when the API changes.

## Run it

```bash
# repo root: the Go server
make db-up && make migrate && make dev   # serves 0.0.0.0:3000

# this folder
pnpm install
cp .env.example .env.local               # then set EXPO_PUBLIC_API_URL
pnpm start                               # press i (iOS) or a (Android)
```

`EXPO_PUBLIC_API_URL` is the Go server's origin as the device sees it:

| Client           | URL                                                                 |
| ---------------- | ------------------------------------------------------------------- |
| iOS Simulator    | `http://localhost:3000`                                             |
| Android Emulator | `http://10.0.2.2:3000`                                              |
| Physical device  | a host the phone can reach: LAN IP or Tailscale name, e.g. `http://my-mac.tailnet.ts.net:3000` |

A phone resolves `localhost` to itself, so a physical device needs a reachable
host, and the server must listen on all interfaces (the default `0.0.0.0`).
Env vars are inlined at bundle time: restart `expo start` after changing them.

In development, email verification is off, so sign-up signs you straight in.
In production it is on: sign-up sends a link and sign-in waits until it is
clicked.

## Checks

| | |
| --- | --- |
| `pnpm typecheck` | `tsc --noEmit` |
| `pnpm lint` | `expo lint` |
| `pnpm exec expo install --check` | dependency versions match the Expo SDK |
| `pnpm test` | unit tests (`src/**/*.test.ts`) |
| `pnpm test:contract` | the app's auth and API clients against a real Go server |

### Contract test

`pnpm test:contract` builds `apps/web/cmd/server` from this repo, starts a
throwaway Postgres 18 container on a free port, migrates, serves on a free
port in development mode, and runs `contract/app.contract.test.ts`: sign-up,
get-session, sign-out, wrong password, sign-in, `GET`/`PATCH /users`
(including validation and the 403 for another user), session restore on a
cold launch, and delete account. The app's real modules run in Node; only the
native modules are stubbed (`contract/stubs/`). Server output goes to
`.contract/server.log`.

It needs Go and Docker. Overrides:

| Variable | Default |
| --- | --- |
| `GO_SERVER_SRC` | `../..` (the repo root to build the server from) |
| `CONTRACT_SERVER_BIN` | build it (set to use a prebuilt `server` binary) |
| `CONTRACT_DATABASE_URL` | start a Postgres container |
| `GO` | `go` (e.g. `GO="$(mise which go)"`) |

## Structure

```
src/
  app/            Expo Router routes ((auth) and (app) groups)
  components/     Themed UI components
  constants/      Theme colors, spacing
  lib/
    api/          apiFetch client, envelope helpers, API types
    auth/         Better Auth client (SecureStore cookies)
    config/       env.ts (EXPO_PUBLIC_API_URL) and routes.ts (AppRoutes/ApiRoutes)
    services/     userService: get, update settings, delete account
    validators/   Zod schemas, mirroring apps/web/internal/validate
contract/         contract test against the Go server
```

## Server side

`packages/auth` trusts the app's `betterstack://` scheme always and the Expo Go
/ dev-client origins (`exp://`) outside production
(`auth.MobileTrustedOrigins`). If you change `scheme` in `app.json`, change it
in `src/lib/auth/authClient.ts` and in `MobileTrustedOrigins` too.

## Notes

- Expo majors change quickly. Check the versioned docs for the SDK pinned in
  `package.json` before upgrading: https://docs.expo.dev/versions/
- Web output is intentionally not configured; this template targets iOS and
  Android with native auth flows.
