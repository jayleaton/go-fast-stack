# Admin dashboard (optional)

`apps/admin` is a separate binary and Railway service for the people who run the app: browse, search and inspect users (sessions, verification, role, ban state), ban (reason + expiry) and unban, revoke sessions, set roles, manage admins, and read the audit log of every admin action. Admin changes fail closed: if the audit log cannot record the attempt, nothing is changed, and an attempt whose outcome was never recorded shows as "outcome unknown" until a reconciler checks the current state. Sign-ins, sign-outs and refused 2FA codes are audited best-effort after the fact (they never block authentication), so the log can miss one if it was down at that moment. An analytics page is a placeholder behind a port.

- **Isolated.** Admins are users of the admin app's own auth instance in its own database (`ADMIN_DATABASE_URL`, cookie `admin.session_token`, secret `ADMIN_AUTH_SECRET`), never rows in the app's `user` table. It reads and administers the app's users through `DATABASE_URL`. depguard keeps `apps/web` and `apps/admin` from importing each other and keeps the higher-privilege `packages/database/adminrepo` out of the web app.
- **Invite only.** There is no sign-up. `admin migrate` seeds `ADMIN_BOOTSTRAP_EMAIL` without a password; `admin invite <email>` (or `make admin-invite EMAIL=...`, or the Admins page) emails a one-time set-password link. With `ADMIN_REQUIRE_2FA=true` (the default in production) setting the password continues into TOTP enrolment with backup codes before the first session.
- **Banning** revokes every session of the user at once; the web app refuses their sign-in until the ban is lifted or expires.

```sh
make db-up                         # also creates the "admin" database on the dev Postgres
make migrate                       # the app database (web)
make admin-dev                     # migrates the admin database, seeds ADMIN_BOOTSTRAP_EMAIL, serves 0.0.0.0:3001
make admin-invite EMAIL=admin@example.com   # dev: the set-password link is printed in the log
```

Locally the admin database is a second database on the same per-checkout Postgres: no extra container, and worktrees stay isolated. Hosted, give it its own small Postgres so its credentials and backups are separate from the app's.

**Deploy:** set `admin: true` in `deploy/platform.yaml` and run `make railway-apply` (see "Deploy to Railway"). It adds the `admin` service (builds `apps/admin/Dockerfile`, runs `/admin migrate` before deploy, health-checks `/healthz`, which pings both databases) and its own `admin-postgres` to every environment. It also wires `ADMIN_DATABASE_URL`, `DATABASE_URL` (a reference to the app's Postgres) and `ADMIN_PUBLIC_URL`, and generates a sealed `ADMIN_AUTH_SECRET`. Set `admin_bootstrap_email` there for the first admin. To rotate the admin secrets live, give the admin service its own Doppler config and token (see "Secrets"). The database-password runbook applies to the admin Postgres too. Not using it? Delete `apps/admin`, `packages/database/adminrepo` and the `admin-*` make targets.
