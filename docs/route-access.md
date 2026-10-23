# Route access

Every route declares who may reach it on the line that registers it, and one
gate per app enforces that for both transports. This page shows how it fits
together and how to extend it with roles or tenants without touching
middleware or handlers.

## The two layers of authorization

| Layer | Question | Lives in | Fails with |
|---|---|---|---|
| **Route access** (coarse) | May this caller reach this route at all? Signed in, has a role, belongs to the tenant in the URL. | `apps/<app>/internal/access` (policies), declared per route | 401 (API) or a redirect to sign-in (UI) when signed out; 403 when denied |
| **Resource authorization** (fine) | May this caller act on this record? Their own account, a document in their tenant. | the service method, from its `services.Actor` | `services.ErrForbidden` (403) |

Route access never replaces the service check: a route open to every signed-in
user still reaches a service that refuses another user's record. Services never
import `access` (depguard).

## Registering a route

JSON API (`apps/web/internal/api`):

```go
mux.Handle("GET "+routes.Me, Route(users.gate, access.SignedIn, users.get))

func (users *Users) get(request *http.Request, caller access.Caller) (CurrentUser, error) {
	user, err := users.svc.GetProfile(request.Context(), caller.Actor(), caller.User.ID)
	return CurrentUser{User: user, Session: caller.Session}, err
}
```

UI (`apps/web/ui/ui.go`, `Routes`):

```go
page(access.SignedIn, "GET "+config.AppRoutes.Dashboard, ui.dashboard)
action(access.SignedIn, "POST "+routes.AccountProfile, ui.updateProfile)
action(access.Public, "POST "+routes.Locale, ui.setLocale)

func (ui *UI) dashboard(writer http.ResponseWriter, request *http.Request, caller access.Caller) { ... }
```

A handler gets the caller the gate admitted; on an `access.Public` route
without a session it is the zero `Caller` (`caller.SignedIn()` is false).
Handlers never read the session themselves: forbidigo rejects `auth.UserFrom`
outside `apps/*/internal/server` and `(*access.Gate).Admit` outside the two
route handlers (`api/route.go`, `ui/route.go`), and `make lint-layering`
proves both rules bite.

| Outcome | JSON API | UI |
|---|---|---|
| admitted | handler runs | handler runs |
| `access.ErrSignedOut` | 401 `{"data":null,"error":"Unauthorized"}` | 303 to the login page (an SSE redirect for Datastar) |
| `access.ErrDenied` (wrapped is fine) | 403 `{"data":null,"error":"Forbidden"}` | 403 "You do not have access to this page" |
| resolver error | 500 `Internal server error` | 500 "Something went wrong" |

The reason a policy wraps into `ErrDenied` is logged, never sent.

## The pieces

- `access.Caller`: who sent the request (user, session). Add what your policies need here.
- `access.Resolver`: builds the caller from the request. `server.Callers` adapts `packages/auth`'s session to it; this is where you load roles or the tenant.
- `access.Policy`: `func(request *http.Request, caller access.Caller) error`. `access.Public` and `access.SignedIn` ship; add your own next to them.
- `access.Gate`: resolve, then check. Built once in `cmd/server` and given to both transports, so one gate decides every route.

The admin app (`apps/admin/internal/access`) has its own copy of the same
pieces (the apps never share code). Its business rule that every admin needs
2FA when `ADMIN_REQUIRE_2FA` is set stays in `services.Policy`.

## Worked example: roles (RBAC)

1. **Caller.** Give the caller its roles in `access/access.go`:

   ```go
   type Caller struct {
   	User    services.User
   	Session Session
   	Roles   []string
   }
   ```

2. **Resolver.** Load them where the session is adapted
   (`apps/web/internal/server/caller.go`), through a port the server owns
   (a repository method like `RolesOf(ctx, userID)`):

   ```go
   func NewCallers(roles RoleLookup) access.Resolver {
   	return func(request *http.Request) (access.Caller, error) {
   		caller, err := Callers(request)
   		if err != nil || !caller.SignedIn() {
   			return caller, err
   		}
   		caller.Roles, err = roles.RolesOf(request.Context(), caller.User.ID)
   		return caller, err // an error is a 500, never a silent sign-out
   	}
   }
   ```

   Wire it in `cmd/server`: `access.NewGate(server.NewCallers(database.NewRoleRepository(pool)))`.

3. **Policy.** Add it next to `SignedIn`:

   ```go
   // Role needs a session with role.
   func Role(role string) Policy {
   	return func(request *http.Request, caller Caller) error {
   		if err := SignedIn(request, caller); err != nil {
   			return err
   		}
   		if !slices.Contains(caller.Roles, role) {
   			return fmt.Errorf("%w: needs role %q", ErrDenied, role)
   		}
   		return nil
   	}
   }
   ```

4. **Routes.** Opt in on the registration line, in either transport:

   ```go
   mux.Handle("GET "+routes.Reports, Route(reports.gate, access.Role("admin"), reports.list))
   page(access.Role("admin"), "GET "+config.AppRoutes.Reports, ui.reportsPage)
   ```

Middleware, the other routes and the handlers stay as they are.

## Worked example: tenants

1. **Caller:** `TenantID string` (or `Tenants []string` for multi-tenant users).
2. **Resolver:** resolve the tenant from the request (subdomain, a
   `/t/{tenant}/` path segment or a header) and check membership with a
   repository port; set `caller.TenantID` only for a member.
3. **Policy:**

   ```go
   // TenantMember needs a session in the tenant the route names.
   func TenantMember(request *http.Request, caller Caller) error {
   	if err := SignedIn(request, caller); err != nil {
   		return err
   	}
   	if caller.TenantID == "" || caller.TenantID != request.PathValue("tenant") {
   		return fmt.Errorf("%w: not a member of tenant %q", ErrDenied, request.PathValue("tenant"))
   	}
   	return nil
   }
   ```

4. **Services:** pass the tenant on the `services.Actor` and scope every query
   by it. The policy keeps other tenants out of the route; the service keeps
   one tenant's records out of another's, because only it knows which tenant a
   record belongs to.

## Tests

Route access is tested at the route-handler boundary
(`apps/web/internal/api/route_test.go`, `apps/admin/internal/api/route_test.go`):
public passes signed out and in, `SignedIn` answers 401 signed out, a custom
policy written in the test answers 403, a resolver error is a 500, and a
refused handler never runs. Add a case there for each policy you add.
`apitest.Gate()` admits whoever `apitest.AsUser` / `apitest.Anonymous` says.
