# Template welcome (safe to delete)

This folder is the student starter landing page (`/`) and the "why these patterns" page (`/why`).

It is **not** part of the real app. Login, signup, dashboard, auth and the database stay if you remove it.

## Remove it

1. Set `TemplateWelcome` to `false` in the routes config (`apps/web/internal/config`; `ui.Features` until integration).
2. Delete this folder: `apps/web/ui/welcome`.
3. Delete the two `welcome.` references in `apps/web/ui/handlers.go` (the compiler points at them).

After that, `/` shows a small "your app is running" screen instead of this guide.
