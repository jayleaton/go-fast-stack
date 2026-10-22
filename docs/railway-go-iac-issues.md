# Railway Go IaC (beta): issues found while building `deploy/`

Ready-to-file drafts for [railwayapp/railway-go-sdk](https://github.com/railwayapp/railway-go-sdk) and [railwayapp/cli](https://github.com/railwayapp/cli), collected on 2026-10-22 while writing `deploy/.railway/railway.go`. **None of them has been filed**; review and file them by hand. They describe Railway's tooling, not this template, and should be generally useful.

Versions: Railway CLI **5.71.0** (`x86_64-unknown-linux-musl`, released 2026-10-19), `github.com/railwayapp/railway-go-sdk` **v0.6.0** (commit `cef0269`), Go 1.26.8, Linux. "Verified" means reproduced locally, by running the snippet or a real `railway config plan/apply` against a throwaway project. "From source" means read in the CLI/SDK code at those versions and not reproduced.

Each entry says how this repo works around it today, so you can drop the workaround when the fix ships.

| # | Repo | Title | Severity | Verified |
|---|---|---|---|---|
| 0 | cli | **A database added to a second environment gets the literal generator text as its password** | **critical (security)** | yes |
| 1 | go-sdk | Non-`[]any`/`map[string]any`/`int` values are silently dropped | high | yes |
| 2 | go-sdk | Unknown or misspelled `ServiceConfig` keys are silently dropped | high | yes |
| 3 | cli | A file that mentions "Partial" fails to evaluate | high | yes |
| 4 | go-sdk | No way to set the project `variables` policy | medium | yes |
| 5 | cli | Only `railway.go` is compiled: helpers in other files are undefined | medium | yes |
| 6 | go-sdk | `Context.RandomString` is derivable by anyone; there is no real secret helper | medium | yes |
| 7 | cli | Go context gets no `pr` or `features` | medium | from source |
| 8 | cli | The eval wrapper is written into the source directory | low | from source |
| 9 | cli | Text output of a failed apply hides the reason | high | yes |
| 10 | cli | A failed apply leaves a partly applied environment | medium | yes |
| 11 | cli | Defaults and empty lists never converge (`restartPolicyType`, `watchPatterns: []`) | medium | yes |
| 12 | cli/go-sdk | Databases accept only a region; always get a public TCP proxy | medium | from source + observed |
| 13 | cli | Sealed variables and generated service domains cannot be declared | medium | yes |
| 14 | cli | Noise and papercuts (`RAILWAY_ENV`, watch paths vs `railway up`, cache mounts, empty `--json`, ...) | low–medium | yes |
| 15 | api/cli | A deleted database's volume comes back: IaC re-attaches it by name to the next database | high (stale data) | yes |
| 16 | api | GitHub access can't be checked with a CLI login token | medium | yes |
| 17 | api/cli | 32-character project name limit only surfaces on create | low | yes |

---

## 0. cli: a database added to a second environment gets the literal generator text as its password

**Summary.** The compiler emits Postgres's password as a variable generator, `POSTGRES_PASSWORD: {"generator": "secret(32, \"abc…XYZ\")"}` (`database_to_environment_config` in `src/iac/compiler.rs`). Railway evaluates it only in the environment where the database **service** is first created. Every other environment that gets an instance of the same service (a later `apply --environment staging`, a new `qa`) stores the **unevaluated generator string** as the value.

**Repro** (verified 2026-10-22, CLI 5.71.0):
1. `postgres("postgres")` graph; `railway config apply --environment production`.
2. Create an empty `staging` environment (`environmentCreate`, no source environment); `railway config apply --environment staging`.
3. Staging's Postgres starts, and initdb sets the superuser password to the literal 66-character string `secret(32, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")`. That string is the same for every affected database of every Railway user.

Of 6 databases in our test project, the 2 created first (production `postgres`, staging `admin-postgres`) got real 32-character passwords. The 4 added to other environments got the literal.

**Impact.**
- The password is public knowledge, and every IaC Postgres also gets a public TCP proxy (see 12), so these databases are open to the internet with a known password.
- Apps fail too: `DATABASE_URL` embeds the spaces, so pgx refuses it with `failed to parse as URL (unexpected spaces found in password)`. Our pre-deploy migration failed in qa this way.

**Expected.** Evaluate generators per environment instance, as templates do. Until then, never emit a generator for environments other than the first.

**Workaround here.** After each apply, `railway-setup` checks every database's `POSTGRES_PASSWORD` for an unevaluated `secret(`. When it finds one, it waits for the database to run and runs `ALTER USER … PASSWORD` with a fresh random hex password through `railway ssh … psql` (SQL on stdin). It then stores that password in `POSTGRES_PASSWORD`, so `DATABASE_URL` references resolve. Verified: qa's migration then succeeded over the network with the new password.

## 1. go-sdk: values of any type but `[]any`, `map[string]any` or `int` are silently dropped

**Summary.** `ServiceConfig` is `map[string]any`, and the normalizers type-assert on exact types. Idiomatic Go values (`[]string`, `map[string]string`, `int64`) fail the assertion, so the setting disappears from the graph with no error, and `railway config plan` reports nothing to change.

**Repro** (v0.6.0):

```go
show := func(c railway.ServiceConfig) { b, _ := json.Marshal(railway.ServiceNamed("web", c).Graph()); fmt.Println(string(b)) }
show(railway.ServiceConfig{"domains": []string{"app.example.com"}})   // networking.customDomains: null
show(railway.ServiceConfig{"domains": []any{"app.example.com"}})      // customDomains: {"app.example.com": {...}}
show(railway.ServiceConfig{"replicas": int64(2)})                     // no deploy.numReplicas
show(railway.ServiceConfig{"env": map[string]string{"A": "1"}})       // no variables at all
```

**Expected.** Accept any slice, map or integer kind (via `reflect`), or panic with a clear message, as `Fn` already does for bad input. Better still: typed config structs (see 2).

**Impact.** Losing every variable from `map[string]string` is the most likely first thing a Go user writes. The plan then shows "up to date", or plans deletes of existing variables.

**Workaround here.** `deploy/platform/graph.go` builds every list as `[]any` and every map as `map[string]any`. `deploy/platform/platform_test.go` asserts the resulting graph.

## 2. go-sdk: unknown or misspelled `ServiceConfig` keys are silently dropped

**Summary.** `serviceNode` copies a fixed set of keys out of `ServiceConfig` and ignores the rest. The CLI's "Unknown field" diagnostic only runs on the emitted graph, so it never sees the typo.

**Repro.** `railway.ServiceNamed("web", railway.ServiceConfig{"healtcheck": "/healthz"}).Graph()` has no `deploy.healthcheckPath`. `railway config plan` passes without a warning.

**Expected.** Panic on unknown keys, as the TypeScript types would reject them at compile time. Or offer a typed `railway.Service{Healthcheck: "/healthz", ...}` struct, so the compiler catches typos. That would be the natural Go surface for a beta that "mirrors" the TypeScript DSL.

**Workaround here.** Assertions in `deploy/platform/platform_test.go` on every key the template relies on.

## 3. cli: any `railway.go` that mentions "Partial" fails to evaluate

**Summary.** `evaluate_go` decides whether the file declares a partial with `fs::read_to_string(file).contains("Partial")` (`src/iac/eval.rs`). Any comment, string or identifier containing that word makes the generated wrapper reference an undefined `Partial` constant.

**Repro.** Take any working single-file `.railway/railway.go`, add the comment `// Partial updates are fine.`, then run `railway config plan --file .railway/railway.go`. Output:

```
./railway_iac_eval_main.go:98:54: undefined: Partial
```

**Expected.** Detect the declaration, not the word: parse with `go/parser` in the wrapper, or use `reflect`/build tags. The wrapper could also emit `partial` only when the identifier resolves, for example via a `railwayIacPartial()` helper defined in a second generated file guarded by a build tag.

**Workaround here.** `deploy/.railway/railway.go` never contains the word (it is a shim that calls `deploy/platform`); keep it that way until this is fixed.

## 4. go-sdk: `Project` cannot set the project `variables` policy

**Summary.** The CLI supports a project-level `variables: {managed, ignore}` policy (`IAC_PROJECT_FIELDS` in `src/iac/compiler.rs`). It is how a file leaves out-of-band secrets alone. `railway.ProjectNamed(name, resources)` and `Project.Graph()` emit only `name` and `resources`.

**Expected.** `railway.ProjectNamed(name, resources, railway.ProjectOptions{Variables: railway.VariablePolicy{Ignore: []string{"web/AUTH_SECRET"}}})`, or exported fields on `Project`.

**Workaround here.** `platform.Project` embeds `railway.Project` and overrides `Graph()` to add `variables`. The CLI's wrapper calls any `Graph()` method by reflection, which is also undocumented (see 5).

## 5. cli: only `railway.go` is compiled, so helpers in sibling files are undefined

**Summary.** The CLI runs `go run railway.go railway_iac_eval_main.go`. A second file in the same `package main` (for example `helpers.go` next to `railway.go`) is not compiled, so its identifiers are `undefined`. The docs say "Put a `go.mod` next to `.railway/railway.go`" but not that the rest of the package is ignored.

**Expected.** `go run .` with the wrapper, plus a build tag so the user's own `main()` (if any) does not collide. Or document that everything else must live in an importable package, and that `Railway` may return any type with a `Graph() map[string]any` method.

**Workaround here.** All logic lives in `deploy/platform`; `railway.go` is a 40-line shim.

## 6. go-sdk: `Context.RandomString` is derivable by anyone, and there is no real secret helper

**Summary.** `RandomString(label, bytes)` returns `hex(sha256("railway-iac:" + environment + ":" + label))`. For `Environment: "production"` and label `AUTH_SECRET`, `RandomString("AUTH_SECRET", 16)` returns `82f368630a7007ef45c9a0f45108acaa`, the same value `printf 'railway-iac:production:AUTH_SECRET' | sha256sum` prints. The name invites using it for secrets.

**Expected.** Rename it, or document loudly that it is a stable non-secret ID. Add `railway.Secret(n)` emitting the variable `generator` the platform already supports (`{"generator": "secret(32, ...)"}`, the shape the CLI itself uses for `POSTGRES_PASSWORD`), plus a `Sealed` flag (see 13).

**Workaround here.** Setup generates auth secrets with `crypto/rand` and writes them sealed through `environmentPatchCommit`. They are kept out of the graph with `variables.ignore`.

## 7. cli: the Go context gets no `pr` or `features`

**Summary.** `EvalContext::to_json` sends `pr: {number, branch, base}` and `features`. The Go wrapper's `railwayIacFillValue` fills only `string` and `[]string` fields, and `railway.Context` has no `PR` field. TypeScript and Python receive the whole payload.

**Expected.** `railway.Context{PR *railway.PR; Features []string}`, filled by the wrapper (decode `RAILWAY_IAC_CONTEXT` with `encoding/json`, not reflection over strings).

**Workaround here.** Previews use Railway's native PR environments. The graph does not branch on PRs.

## 8. cli: the eval wrapper is written into the user's source directory

**Summary.** `evaluate_go` writes `railway_iac_eval_main.go` next to `railway.go` and deletes it afterwards. An interrupted plan leaves it behind, where it can get committed or break the user's build. Two concurrent plans of the same checkout (a CI matrix over environments) race on the same path. A read-only checkout cannot be evaluated.

**Expected.** Generate it in a temp directory with an overlay (`go run -overlay`), or under `os.TempDir()` with a `go.work`/`replace` back to the module.

**Workaround here.** `.gitignore` lists `deploy/.railway/railway_iac_eval_main.go`. Setup plans environments one after another.

## 9. cli: text output of a failed apply hides the reason

**Summary.** Text mode prints only the change kinds:

```
Error error: Apply finished with status failed; failed changes: change, change, change, change, change, change, change
```

The actual cause appears only with `--json`:

```json
{"changes":[{"kind":"resource.update","path":"service.657241f7-…","status":"failed"}],
 "diagnostics":[{"message":"You don't have access to the GitHub repo <owner>/<repo>. Make sure the Railway GitHub App is installed on it and your GitHub account can read it."}]}
```

**Repro.** `railway config apply --yes` with a `github()` source the Railway GitHub app cannot read.

**Expected.** Print each failed change's summary or address and every diagnostic message in text mode.

**Workaround here.** `railway-setup apply` runs `config apply --json` and prints changes and diagnostics itself.

## 10. cli: a failed apply leaves a partly applied environment

**Summary.** In the repro for 9, the apply reported failure but had already created `postgres` and `web`, and applied all of web's settings except the one change shown as failed. The next plan showed only that one change. Users reading "failed" will assume nothing happened.

**Expected.** Either apply atomically (stage everything, commit once), or say "applied N of M changes" and list what was applied.

## 11. cli: defaults and empty lists never converge

**Summary.** `deploy: {restartPolicyType: "ON_FAILURE"}` is accepted, but Railway stores the environment config without it, because it is the default. Every later plan shows:

```
~ Update web deploy.restartPolicyType
  └ deploy.restartPolicyType (null → "ON_FAILURE")
```

`railway.json` users migrating with `config migrate` get exactly this field.

**Expected.** Normalize defaults on both sides of the diff, as `normalize_for_diff` already does for `builder: RAILPACK`, `dockerfilePath: Dockerfile` and `branch: main`.

The same happens with an empty list. `build: {watchPatterns: []}` (the only way to clear watch paths, since omitting a field leaves Railway's value in place) is stored as `null`, and every later plan shows `build.watchPatterns (null → [])`.

**Workaround here.** The graph does not declare `restartPolicyType`, and a test asserts it stays absent. Upload mode omits `watchPatterns`; `railway-setup` clears leftovers with an `environmentPatchCommit`.

## 12. cli + go-sdk: databases accept only a region, and always get a public TCP proxy

**Summary.** `railway.Postgres(name, opts)` passes only `region` through. The CLI's `database_to_environment_config` hardcodes `networking.tcpProxies: {"5432": {}}` and `DATABASE_PUBLIC_URL`, and reads nothing but `multiRegionConfig` from `deploy`. Consequences:
- a database's memory and CPU cannot be capped;
- its replicas and restart policy cannot be set;
- the public TCP proxy cannot be opted out of, which is internet-reachable and billed as egress.

A template that only talks to Postgres over the private network still gets a public endpoint per environment.

**Expected.** `railway.Postgres(name, railway.DatabaseOptions{Public: false, Limits: ...})`, with the compiler honouring `deploy.limitOverride` and an explicit `tcpProxies` for databases.

**Workaround here.** None. It is documented in README "Deploy to Railway". Remove the proxy in the dashboard if your policy requires it; IaC does not re-add it after creation (not verified).

## 13. cli: sealed variables and generated service domains cannot be declared

**Summary.**
- *Sealed.* The env-config variable schema has `isSealed` and `generator`, and the compiler passes literal fields through (`variable_to_config`). But no SDK helper emits them, the docs do not mention them, and the public API's `VariableUpsertInput` has no `isSealed`. The only way to seal from code is `environmentPatchCommit` with `{"variables": {"NAME": {"value": "...", "isSealed": true}}}`. That works (verified), but nothing documents it.
- *Domains.* Railway-generated domains are stripped from diffs (`serviceDomains` in `change_set.rs`). A graph whose `PUBLIC_URL` is `https://${{RAILWAY_PUBLIC_DOMAIN}}` therefore needs an out-of-band `railway domain` per service and environment.

**Expected.** `railway.Sealed(railway.Preserve())` / `railway.Secret(48)`, and `"domains": []any{railway.GeneratedDomain(3000)}`.

**Workaround here.** `railway-setup` seals bootstrap secrets through `environmentPatchCommit` and creates generated domains with `railway domain --port 3000`.

## 14. Noise and papercuts

- *`variables.ignore` warnings.* Every first plan in a new environment prints `Warning: variables.ignore pattern "web/AUTH_SECRET" matches no variable.` once per pattern. The documented use of `ignore` is exactly values set out-of-band after the first apply. Suggest staying quiet when the service itself is new.
- *`railway add` prompts without a terminal.* `railway add --service probe --json`, run without a TTY and with `RAILWAY_PROJECT_ID` set, printed the interactive prompts (`Enter a service name`, `Enter a variable`) and then failed with `No environment specified`. Expected: non-interactive with `--json`, or a clear "needs --environment" error before prompting.
- *`railway upgrade`.* `railway upgrade --yes` fails with "install method could not be detected" for the binary in `~/.local/bin` installed by the official install script, which is the documented install. Suggest falling back to replacing the running binary.
- *`serviceInstance` disagrees with environment config.* For an IaC-managed service, the GraphQL `serviceInstance { builder }` reports `RAILPACK` while `railway environment config` says `build.builder: DOCKERFILE`.
- *Docs versions.* The IaC docs say `go get github.com/railwayapp/railway-go-sdk@v0.3.0`, but the SDK README and latest tag are v0.6.0. The docs mention CLI 5.63.0 features, but nothing states a minimum CLI version for Go evaluation with a `Context` argument.
- *`RAILWAY_ENV` silently switches the API backend.* `RAILWAY_ENV=staging` (or `dev`) makes every CLI command talk to Railway's internal staging/dev API (`src/config.rs` `get_environment_id`), so a logged-in user just gets `Unauthorized. Please login with railway login`. The name collides with what projects naturally call "the Railway environment to deploy". We hit it through `make railway-up RAILWAY_ENV=staging`. Expected: a namespaced name (`RAILWAY_CLI_BACKEND`), or at least "using Railway staging API" printed on stderr. Verified.
- *Watch paths skip `railway up`.* Railway applies a service's watch paths to CLI uploads too. A `railway up` that changes no file under them ends as `SKIPPED`, even when the previous deploy failed (ours failed in pre-deploy, then three re-uploads were skipped). Watch paths are a GitHub-push filter; manual uploads should always deploy, or at least say why they were skipped.
- *Cache mounts rejected.* Railway's builder rejects `RUN --mount=type=cache,target=/go/pkg/mod` with `dockerfile invalid: flag '--mount=type=cache,target=/go/pkg/mod' is missing an id argument`. The required id format embeds the service ID, so one Dockerfile cannot build both locally and on Railway for every environment. The template's Dockerfile drops cache mounts.
- *Empty `--json` output.* `railway config apply --yes --json` on an up-to-date environment exits 0 with **empty stdout** (not `{}` or `{"changes":[]}`), so a JSON consumer cannot tell "nothing to do" from "printed nothing because it crashed". Verified on 5.71.0.
- *Empty networking.* Every Go service emits `"networking": {"customDomains": null, "tcpProxies": null}`, even with no networking configured, while TypeScript omits the block. Plans are clean today, but explicit nulls in a sparse patch are fragile.

## 15. api + cli: a deleted database's volume comes back

**Summary.** `serviceDelete` on an IaC-managed Postgres leaves its volume (`postgres-volume`) in the project. `volumeDelete(volumeId)` on that volume returns `true`, but the volume is still listed afterwards; in our test it was still there 10 minutes later. The next `railway config apply` that declares `postgres("postgres")` re-attaches that same volume (same id) to the new database service, in the environment it came from.

**Repro** (verified 2026-10-22, CLI 5.71.0):
1. Apply a `postgres` + `web` graph to production; deploy web.
2. `serviceDelete` both services, then `volumeDelete` the volume (both return true).
3. Apply the same graph again: the plan lists `resource.update volume.<old id>`.
4. The new database starts on the old data directory. A new web service with a freshly generated `AUTH_SECRET` then refuses to boot ("the two-factor-secret key is wrapped by a secret this process doesn't have"), because its keys live in the old data.

**Expected.**
- Deleting a database service deletes or detaches its volume visibly.
- `volumeDelete` either deletes the volume or says it is scheduled.
- IaC never silently adopts an orphaned volume into a new database; at least the plan should say "re-attaching existing volume with data".

**Workaround here.** None in code: a from-zero setup never has an orphaned volume. When re-creating a database, delete its old volume in the dashboard first.

## 16. api: GitHub access can't be checked with a CLI login token

**Summary.** Every GitHub query answers `Not Authorized` for the token `railway login` stores: `gitHubRepoAccessAvailable(fullRepoName)`, `githubRepos`, `githubRepoBranches` and `githubWritableScopes`. That holds even when the account has GitHub linked (`me { githubProviderId }` works), and even when the Railway app can read the repo. So a CLI tool cannot ask "can Railway build this repo?" before trying. The only probe is `serviceConnect(id, {repo, branch})`, which either connects the repo or fails with `User does not have access to the repo`.

**Expected.** Allow `gitHubRepoAccessAvailable` (read-only, own repos) for user CLI tokens, or add a `railway github status <owner/repo>` command.

**Workaround here.** `railway-setup` tries `gitHubRepoAccessAvailable` first. When the API refuses, it creates the services without a source and calls `serviceConnect` on web as the access check. If that fails it deploys by upload and prints the install link.

## 17. api + cli: the 32-character project name limit only surfaces on create

**Summary.** `projectCreate` rejects names over 32 characters (`Project names must be between 1 and 32 characters.`). Nothing documents the limit, and IaC `project("…")` accepts longer names. An automated setup finds out only when it tries to create the project.

**Workaround here.** `platform.yaml` validation enforces 32 characters, and `project: auto` truncates long repo names.
