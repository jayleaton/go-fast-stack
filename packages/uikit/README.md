# packages/uikit: shared design tokens and templ components

Both app UIs (`apps/web/ui`, `apps/admin/ui`) render through this package: the document and app shell, buttons,
form fields, cards, alerts and toasts, badges, tables, confirm dialogs, empty states and skeletons. It renders
values only: components take translated strings and plain data, and depguard (`uikit` in `.golangci.yml`, probed by
`make lint-layering`) keeps it from importing `apps/`, `packages/database`, `packages/auth`, `packages/secrets`,
pgx or datastar-go. Either app can be deleted without touching it.

## Re-theme in one file

Edit `theme.css`. Every color is one line with its light and dark value:

```css
--ui-primary: light-dark(#4f46e5, #818cf8);
```

Rebuild with `make css` and both apps follow. Radius (`--ui-radius-control`, `--ui-radius-card`), shadows and the font
stacks are in the same file. `base.css` maps the tokens to Tailwind utilities (`bg-surface`, `text-muted-foreground`,
`border-input`, `bg-primary`, `bg-danger-soft`, `rounded-card`, `shadow-card`, ...) and holds the base styles (focus
ring, reduced motion); you only edit it to add a token.

Keep the pairs at WCAG AA in both columns: text on its background at 4.5:1 (`foreground`/`background`,
`muted-foreground` on `background`/`surface`/`muted`, `*-foreground` on its solid color, `*-soft-foreground` on its
`*-soft`), and `input` and `ring` at 3:1 against `surface`. The defaults pass all of them.

## Light, dark and system

`<html>` gets `class="dark"` or `class="light"` from the `theme` cookie, rendered by the server; with neither, the
system preference applies (`color-scheme: light dark` plus `light-dark()`). The first paint is already right, so no
pre-paint script is needed under the strict nonce CSP. `ThemeToggle` flips the class and writes the cookie through a
Datastar expression, which Datastar runs with the page nonce. Use `dark:` only where tokens cannot help (swapping an
icon); everything else uses the semantic utilities and needs no dark variant.

## Components

| Component | Use |
|---|---|
| `Page(Document)` | `<html>`: lang, theme class, nonce, CSS, Datastar, favicon |
| `AppShell(Shell)` | skip link, sticky header (brand, nav, actions), mobile menu, main, optional footer |
| `ThemeToggle`, `LocaleMenu`, `SignOutButton` | header actions |
| `AuthLayout(brand, title, description)` | centered card for sign-in style pages |
| `PageHeader`, `Card`, `CardHeader`, `StatGrid`, `DetailList`/`Detail` | page structure |
| `Button`, `LinkButton`, `SubmitButton`, `ButtonClass` | actions; `SubmitButton` shows a spinner while its Datastar indicator is on |
| `Input`, `Textarea`, `Select`, `CheckboxField` | labelled fields with hint and error (`aria-describedby`, `aria-invalid`) |
| `Alert` (patch target), `Callout`, `Toast` + `ToastRegion` | feedback by `Tone` |
| `Badge`, `StatusIcon`, `Spinner` | status |
| `TableWrap` + `TableClass`/`ThClass`/`TdClass`/`TrClass` | scrollable tables |
| `ConfirmForm(Confirm)` | a post form whose trigger opens a modal `<dialog>`; without JS it posts directly |
| `EmptyState`, `Skeleton`, `LineSkeleton`, `PageSkeleton` | empty and loading states |
| `PostForm`, `Post`, `Get`, `Assign`, `ConfirmPostForm` | Datastar action expressions |

Extra attributes (ids, `aria-*`, Datastar `data-*`) go in `templ.Attributes`. Components never use inline `style` or
scripts, so pages stay CSP-clean.

## Building

`make generate` runs `templ generate` here too (CI checks the output); `make css` rebuilds both apps' `app.css`, which
include this package's classes through `base.css`'s `@source` lines.
