package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// Waits between Railway CLI calls.
const (
	pollInterval = 10 * time.Second // between checks of a deployment that has not settled
	retryDelay   = 10 * time.Second // before retrying a read Railway answered with a body the CLI cannot decode
)

// setup reconciles one Railway project with platform.yaml. Steps IaC covers
// (services, databases, variables, builds, healthchecks) go through
// `railway config plan|apply`; the rest (project, environments, previews,
// bootstrap secrets, generated domains) is done here, idempotently.
type setup struct {
	backupArgs []string // backup: the job's arguments (list | run | restore <id> ...)
	cfg        platform.Config
	cfgPath    string // absolute; passed to the IaC file as PLATFORM_CONFIG
	iacFile    string // absolute path of deploy/.railway/railway.go
	rw         *cli
	out        io.Writer
	apply      bool
	destruct   bool

	yes     bool     // --yes: create the project without asking
	jsonOut bool     // --json: logs go to stderr, the summary to stdout
	root    string   // repository root: what railway up uploads
	ws      string   // resolved workspace id
	project *project // nil until it exists
	pending int      // changes a plan found
	sum     summary
}

type project struct {
	ID           string
	Name         string
	Workspace    string
	Environments map[string]string // name -> id
	Services     map[string]string // name -> id
}

type listedProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Workspace struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"workspace"`
	Environments edges `json:"environments"`
	Services     edges `json:"services"`
}

type edges struct {
	Edges []struct {
		Node struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"node"`
	} `json:"edges"`
}

func (edges edges) byName() map[string]string {
	lookup := map[string]string{}
	for _, edge := range edges.Edges {
		lookup[edge.Node.Name] = edge.Node.ID
	}
	return lookup
}

func (setup *setup) say(format string, values ...any) { fmt.Fprintf(setup.out, format+"\n", values...) }

// change records a change: done in apply mode, pending in plan mode.
func (setup *setup) change(format string, values ...any) {
	if setup.apply {
		setup.say("  ✓ "+format, values...)
	} else {
		setup.pending++
		setup.say("  + "+format, values...)
	}
}

// findProject loads the project named in platform.yaml, if it exists.
// Only the configured workspace is searched (or your only one), so a
// same-named project elsewhere is never touched.
func (setup *setup) findProject(ctx context.Context) error {
	ws, err := setup.workspaceID(ctx)
	if err != nil {
		return err
	}
	var all []listedProject
	if err := setup.rw.json(ctx, &all, "list"); err != nil {
		return err
	}
	var hits []listedProject
	for _, project := range all {
		if project.Name == setup.cfg.Project && project.Workspace.ID == ws {
			hits = append(hits, project)
		}
	}
	switch len(hits) {
	case 0:
		setup.project = nil
	case 1:
		listedProject := hits[0]
		setup.project = &project{ID: listedProject.ID, Name: listedProject.Name, Workspace: listedProject.Workspace.Name,
			Environments: listedProject.Environments.byName(), Services: listedProject.Services.byName()}
		setup.rw.projectID = listedProject.ID
	default:
		return fmt.Errorf("%d projects named %q in this workspace: rename one", len(hits), setup.cfg.Project)
	}
	return nil
}

func (setup *setup) workspaceID(ctx context.Context) (string, error) {
	if setup.ws != "" {
		return setup.ws, nil
	}
	id, err := setup.lookupWorkspace(ctx)
	setup.ws = id
	return id, err
}

func (setup *setup) lookupWorkspace(ctx context.Context) (string, error) {
	var me struct {
		Workspaces []struct{ ID, Name string } `json:"workspaces"`
	}
	if err := setup.rw.json(ctx, &me, "whoami"); err != nil {
		return "", err
	}
	var names []string
	for _, workspace := range me.Workspaces {
		if setup.cfg.Workspace != "" && (workspace.ID == setup.cfg.Workspace || workspace.Name == setup.cfg.Workspace) {
			return workspace.ID, nil
		}
		names = append(names, fmt.Sprintf("%q", workspace.Name))
	}
	if setup.cfg.Workspace == "" && len(me.Workspaces) == 1 {
		return me.Workspaces[0].ID, nil
	}
	if setup.cfg.Workspace != "" {
		return "", fmt.Errorf("workspace %q not found; yours: %s", setup.cfg.Workspace, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("you belong to several workspaces; set workspace in platform.yaml to one of %s", strings.Join(names, ", "))
}

// ensureProject creates the project (with its production environment).
func (setup *setup) ensureProject(ctx context.Context) error {
	if err := setup.findProject(ctx); err != nil || setup.project != nil {
		if setup.project != nil {
			setup.say("project %s (%s) in %s", setup.project.Name, setup.project.ID, setup.project.Workspace)
		}
		return err
	}
	ws, err := setup.workspaceID(ctx)
	if err != nil {
		return err
	}
	if !setup.apply {
		setup.change("create project %q", setup.cfg.Project)
		return nil
	}
	if err := setup.confirm(fmt.Sprintf("Create Railway project %q (billed to that workspace)?", setup.cfg.Project)); err != nil {
		return err
	}
	var res struct {
		ProjectCreate struct{ ID string } `json:"projectCreate"`
	}
	err = setup.rw.gql(ctx, `mutation($input: ProjectCreateInput!) { projectCreate(input: $input) { id } }`,
		map[string]any{"input": map[string]any{
			"name": setup.cfg.Project, "workspaceId": ws, "defaultEnvironmentName": "production",
			"isMonorepo": true, "prDeploys": false,
		}}, &res)
	if err != nil {
		return err
	}
	setup.change("created project %q (%s)", setup.cfg.Project, res.ProjectCreate.ID)
	if err := setup.findProject(ctx); err != nil {
		return err
	}
	if setup.project == nil {
		return fmt.Errorf("created project %s but cannot list it yet: re-run in a minute (it will be reused, not duplicated)", res.ProjectCreate.ID)
	}
	return nil
}

// ensureEnvironments creates missing environments empty (never duplicated
// from production: no copied data or secrets) and only warns about extras.
func (setup *setup) ensureEnvironments(ctx context.Context) error {
	for _, env := range setup.cfg.Environments {
		if setup.project != nil && setup.project.Environments[env] != "" {
			continue
		}
		if setup.project == nil && env == "production" {
			continue // created with the project
		}
		if !setup.apply {
			setup.change("create environment %s", env)
			continue
		}
		// Railway allows one environment create per 30 s per project.
		const rateLimitWait = 31 * time.Second // just past that window
		var err error
		for range 4 {
			err = setup.rw.gql(ctx, `mutation($input: EnvironmentCreateInput!) { environmentCreate(input: $input) { id } }`,
				map[string]any{"input": map[string]any{"projectId": setup.project.ID, "name": env, "skipInitialDeploys": true}}, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "rate limit") {
				break
			}
			setup.say("  … Railway rate limit on environment create; retrying in %s", rateLimitWait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(rateLimitWait):
			}
		}
		if err != nil {
			return fmt.Errorf("creating environment %s: %w", env, err)
		}
		setup.change("created environment %s", env)
	}
	if setup.project == nil {
		return nil
	}
	for name := range setup.project.Environments {
		if !setup.cfg.Has(name) && !strings.Contains(name, "-pr-") {
			setup.say("  ! environment %s exists on Railway but not in platform.yaml; it is left alone (delete it with: railway environment delete %s)", name, name)
		}
	}
	return setup.findProject(ctx)
}

// ensurePreviews turns Railway PR environments on or off. Previews copy the
// base environment: staging when there is one, never production otherwise.
func (setup *setup) ensurePreviews(ctx context.Context) error {
	if setup.project == nil {
		if setup.cfg.Previews {
			setup.change("enable PR environments")
		}
		return nil
	}
	var res struct {
		Project struct {
			PrDeploys         bool    `json:"prDeploys"`
			BaseEnvironmentID *string `json:"baseEnvironmentId"`
		} `json:"project"`
	}
	if err := setup.rw.gql(ctx, `query($id: String!) { project(id: $id) { prDeploys baseEnvironmentId } }`,
		map[string]any{"id": setup.project.ID}, &res); err != nil {
		return err
	}
	input := map[string]any{}
	if res.Project.PrDeploys != setup.cfg.Previews {
		input["prDeploys"] = setup.cfg.Previews
	}
	if setup.cfg.Previews {
		base := setup.cfg.Environments[0] // staging, or production in the production-only profile
		if id := setup.project.Environments[base]; id != "" && (res.Project.BaseEnvironmentID == nil || *res.Project.BaseEnvironmentID != id) {
			input["baseEnvironmentId"] = id
		}
	}
	if len(input) == 0 {
		return nil
	}
	if setup.apply {
		if err := setup.rw.gql(ctx, `mutation($id: String!, $input: ProjectUpdateInput!) { projectUpdate(id: $id, input: $input) { id } }`,
			map[string]any{"id": setup.project.ID, "input": input}, nil); err != nil {
			return err
		}
	}
	setup.change("PR environments: %v (base %s)", setup.cfg.Previews, setup.cfg.Environments[0])
	return nil
}

// iac runs `railway config plan|apply` for env. Plan streams the CLI's own
// diff; apply runs with --json so per-change failures and their diagnostics
// are printed (the CLI's text output only says "failed changes: change").
func (setup *setup) iac(ctx context.Context, env string) error {
	if setup.project == nil || setup.project.Environments[env] == "" {
		setup.change("apply the IaC graph to %s (new environment: everything is created)", env)
		return nil
	}
	if !setup.apply {
		const planChanges = 2 // railway config plan --detailed-exit-code: changes are pending
		code, err := setup.rw.stream(ctx, "config", "plan", "--file", setup.iacFile, "--environment", env, "--detailed-exit-code")
		switch {
		case err != nil:
			return err
		case code == planChanges:
			setup.pending++
		case code != 0:
			return fmt.Errorf("railway config plan for %s failed with exit code %d", env, code)
		}
		return nil
	}
	args := []string{"config", "apply", "--file", setup.iacFile, "--environment", env, "--yes", "--json"}
	if setup.destruct {
		args = append(args, "--confirm-destructive")
	}
	out, runErr := setup.rw.output(ctx, nil, args...)
	failed := runErr != nil
	if runErr == nil || len(bytes.TrimSpace(out)) > 0 {
		failed = setup.renderApply(out) || failed
	}
	if runErr != nil || failed {
		hint := ""
		if !setup.destruct {
			hint = " (if it deletes something you removed from platform.yaml on purpose, re-run with CONFIRM_DESTRUCTIVE=1)"
		}
		if runErr == nil {
			runErr = errors.New("some changes failed")
		}
		return fmt.Errorf("railway config apply for %s: %w%s", env, runErr, hint)
	}
	return nil
}

// renderApply prints the changes and diagnostics of `config apply --json`
// output (values are redacted by the CLI) and reports whether any failed.
func (setup *setup) renderApply(out []byte) bool {
	if len(bytes.TrimSpace(out)) == 0 { // the CLI prints nothing, not even {}, when nothing changes
		setup.say("  no changes")
		return false
	}
	var doc any
	if json.Unmarshal(out, &doc) != nil {
		setup.say("%s", tail(string(out))) // plan output: values are redacted by the CLI
		return true
	}
	failed, seen := false, map[string]bool{}
	var walk func(value any)
	walk = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, element := range value {
				walk(element)
			}
		case map[string]any:
			kind, _ := value["kind"].(string)
			status, _ := value["status"].(string)
			if kind != "" && status != "" {
				what, _ := value["summary"].(string)
				if what == "" {
					what, _ = value["path"].(string)
				}
				line := fmt.Sprintf("  %s %s %s", status, kind, what)
				if !seen[line] {
					seen[line] = true
					setup.say("%s", line)
				}
				failed = failed || status == "failed"
			}
			if msg, ok := value["message"].(string); ok {
				sev, _ := value["severity"].(string)
				line := fmt.Sprintf("  %s %s", strings.ToUpper(firstOr(sev, "note")), msg)
				if !seen[line] && sev != "warning" { // warnings: "ignore pattern matches no variable" before bootstrap
					seen[line] = true
					setup.say("%s", line)
				}
			}
			for _, element := range value {
				walk(element)
			}
		}
	}
	walk(doc)
	if len(seen) == 0 {
		setup.say("  no changes")
	}
	return failed
}

func firstOr(text, dir string) string {
	if text == "" {
		return dir
	}
	return text
}

// up uploads this checkout to every app service in env with `railway up`
// (upload mode, repo: ""), waiting for each build. Railway builds it with
// the service's Dockerfile and runs the pre-deploy migration.
func (setup *setup) up(ctx context.Context, env string) error {
	return setup.upload(ctx, env, false)
}

// upload deploys every app service in env with `railway up`; missingOnly
// skips services whose latest deployment succeeded (apply's fallback, so a
// re-run does not redeploy).
func (setup *setup) upload(ctx context.Context, env string, missingOnly bool) error {
	if setup.project == nil || setup.project.Environments[env] == "" || !setup.servicesExist() {
		return fmt.Errorf("%s is not set up yet: run make railway-apply first", env)
	}
	var errs []error // one failed service does not stop the others
	message := uploadMessage(ctx, setup.root)
	for _, svc := range setup.allServices() {
		if missingOnly {
			if st, err := setup.deploymentStatus(ctx, svc, env); err == nil && (st == "SUCCESS" || !settled(st)) {
				continue // live, or a deploy is already running
			}
		}
		setup.say("== %s/%s: railway up", env, svc)
		code, err := setup.rw.streamIn(ctx, setup.root, "up", "--service", svc, "--environment", env, "--ci", "-m", message)
		switch {
		case err != nil:
			return err
		case code != 0:
			errs = append(errs, fmt.Errorf("railway up %s/%s failed (exit %d): see railway logs --service %s --environment %s", env, svc, code, svc, env))
		}
	}
	return errors.Join(errs...)
}

// uploadMessage is the deployment message of a railway up from `root`:
// "make railway-up <commit>" for a clean checkout, so make pr-merge
// ARGS="--check-deploy <sha>" can tell which commit a service runs without a
// Deploy staging run (scripts/cilocal/verify DeployedCommit). A dirty or
// unreadable checkout records no commit: what it uploaded is no commit.
func uploadMessage(ctx context.Context, root string) string {
	const plain = "make railway-up"
	head, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return plain
	}
	dirty, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return plain
	}
	if len(bytes.TrimSpace(dirty)) > 0 {
		return plain + " (uncommitted changes on " + strings.TrimSpace(string(head)) + ")"
	}
	return plain + " " + strings.TrimSpace(string(head))
}

// repairDatabases fixes a Railway IaC bug (docs/railway-go-iac-issues.md #0):
// a database added to a second environment keeps POSTGRES_PASSWORD as the
// unevaluated generator text `secret(32, "...")`, so its password is the
// same public string everywhere and DATABASE_URL does not parse (spaces).
// It sets a random password inside the running container (railway ssh,
// SQL on stdin), then stores it in POSTGRES_PASSWORD.
func (setup *setup) repairDatabases(ctx context.Context, env string) error {
	return setup.setDatabasePasswords(ctx, env, false)
}

// rotateDatabases gives every database in env a new random superuser
// password (after a provider incident: Railway holds POSTGRES_PASSWORD),
// then redeploys the app services that read DATABASE_URL from Railway.
// Services on Doppler read it from Doppler: update it there.
func (setup *setup) rotateDatabases(ctx context.Context, env string) error {
	if setup.project == nil || setup.project.Environments[env] == "" {
		return fmt.Errorf("%s is not set up yet: run make railway-apply first", env)
	}
	if err := setup.setDatabasePasswords(ctx, env, true); err != nil {
		return err
	}
	for _, svc := range setup.allServices() {
		if setup.project.Services[svc] == "" {
			continue
		}
		vars, err := setup.variables(ctx, svc, env)
		if err != nil {
			return err
		}
		if setup.onDoppler(vars) {
			setup.say("  ! %s/%s reads DATABASE_URL from Doppler: if it uses the superuser, update it there (docs/secrets.md: rotate the database password)", env, svc)
			continue
		}
		args := []string{"redeploy", "--service", svc, "--environment", env, "--yes"}
		if !setup.cfg.Upload() {
			// The branch's latest commit: a plain redeploy repeats the latest
			// deployment's build, which may be an old one (docs/deploy.md).
			args = append(args, "--from-source")
		}
		if _, err := setup.rw.output(ctx, nil, args...); err != nil {
			return err
		}
		setup.change("%s/%s redeployed with the new DATABASE_URL", env, svc)
	}
	return nil
}

func (setup *setup) setDatabasePasswords(ctx context.Context, env string, force bool) error {
	if setup.project == nil || setup.project.Environments[env] == "" {
		return nil
	}
	for _, db := range setup.databases() {
		if setup.project.Services[db] == "" {
			continue
		}
		vars, err := setup.variables(ctx, db, env)
		if err != nil {
			return err
		}
		pw, ok := vars["POSTGRES_PASSWORD"]
		if !ok || pw == nil { // missing or sealed by hand: not ours to change
			if force {
				setup.say("  ! %s/%s has no readable POSTGRES_PASSWORD; rotate it in the dashboard", env, db)
			}
			continue
		}
		broken := strings.Contains(*pw, "secret(")
		if !force && !broken {
			continue
		}
		if !setup.apply {
			setup.change("%s/%s POSTGRES_PASSWORD is Railway's unevaluated generator: set a random one", env, db)
			continue
		}
		if err := setup.waitDeployed(ctx, db, env); err != nil {
			return err
		}
		const passwordBytes = 24 // random bytes in a Postgres password (48 hex characters)
		data := make([]byte, passwordBytes)
		if _, err := rand.Read(data); err != nil {
			return err
		}
		fresh := hex.EncodeToString(data) // URL-safe: DATABASE_URL embeds it unescaped
		user := "postgres"
		if raw := vars["POSTGRES_USER"]; raw != nil && *raw != "" {
			user = *raw
		}
		sql := fmt.Sprintf("ALTER USER %q WITH PASSWORD '%s';\n", user, fresh)
		if _, err := setup.rw.output(ctx, []byte(sql), "ssh", "--service", db, "--environment", env, "--",
			// The local socket, not PGHOST over TCP: the container's PGPASSWORD
			// is the value being replaced and may not match the database.
			"env", "-u", "PGHOST", "-u", "PGPASSWORD", "psql", "-h", "/var/run/postgresql", "-U", user, "-v", "ON_ERROR_STOP=1", "-qtA"); err != nil {
			return fmt.Errorf("%s/%s: setting a real password over railway ssh (needs an SSH key: railway ssh keys): %w", env, db, err)
		}
		if err := setup.commit(ctx, env, map[string]map[string]secretWrite{db: {"POSTGRES_PASSWORD": {fresh, false}}}); err != nil {
			return err
		}
		if broken {
			setup.change("%s/%s POSTGRES_PASSWORD was Railway's unevaluated generator: replaced with a random password", env, db)
		} else {
			setup.change("%s/%s POSTGRES_PASSWORD rotated", env, db)
		}
	}
	return nil
}

// clearWatchPaths removes watch paths left on app services when a project
// moves from GitHub to upload mode: Railway would skip every `railway up`
// that changes nothing under them. IaC cannot express "none" (see graph.go).
func (setup *setup) clearWatchPaths(ctx context.Context, env string) error {
	if !setup.cfg.Upload() || setup.project == nil || setup.project.Environments[env] == "" {
		return nil
	}
	var cfg struct {
		Services map[string]struct {
			Build struct {
				WatchPatterns []string `json:"watchPatterns"`
			} `json:"build"`
		} `json:"services"`
	}
	if err := setup.rw.json(ctx, &cfg, "environment", "config", "--environment", env); err != nil {
		return err
	}
	services := map[string]any{}
	for _, svc := range setup.allServices() {
		id := setup.project.Services[svc]
		if id == "" || len(cfg.Services[id].Build.WatchPatterns) == 0 {
			continue
		}
		services[id] = map[string]any{"build": map[string]any{"watchPatterns": []string{}}}
		setup.change("%s/%s clear watch paths (upload mode)", env, svc)
	}
	if len(services) == 0 || !setup.apply {
		return nil
	}
	return setup.rw.gql(ctx, `mutation($env: String!, $patch: EnvironmentConfig) { environmentPatchCommit(environmentId: $env, patch: $patch, commitMessage: "railway-setup: upload mode has no watch paths") }`,
		map[string]any{"env": setup.project.Environments[env], "patch": map[string]any{"services": services}}, nil)
}

func (setup *setup) databases() []string {
	if setup.cfg.Admin {
		return []string{platform.ServicePostgres, platform.ServiceAdminPostgres}
	}
	return []string{platform.ServicePostgres}
}

// waitDeployed waits up to 5 minutes for svc's latest deployment in env.
func (setup *setup) waitDeployed(ctx context.Context, svc, env string) error {
	const deployWait = 5 * time.Minute // how long a fresh database may take to come up
	deadline := time.Now().Add(deployWait)
	for {
		var deps []struct {
			Status string `json:"status"`
		}
		if err := setup.rw.json(ctx, &deps, "deployment", "list", "--service", svc, "--environment", env); err != nil {
			return err
		}
		if len(deps) > 0 && deps[0].Status == "SUCCESS" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s/%s is not running yet; re-run make railway-apply", env, svc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// variables lists a service's variables in env: name -> value, nil when
// sealed. Values never leave this process.
func (setup *setup) variables(ctx context.Context, service, env string) (map[string]*string, error) {
	var err error
	for attempt := range 3 { // reads only: Railway's API sometimes sends a body the CLI cannot decode
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		vars := map[string]*string{}
		if err = setup.rw.json(ctx, &vars, "variable", "list", "--service", service, "--environment", env); err == nil {
			return vars, nil
		}
	}
	return nil, err
}

type secretWrite struct {
	value  string
	sealed bool
}

// bootstrap provisions the variables IaC ignores (platform.SecretVars):
// generated auth secrets, the Doppler token placeholder, SECRETS_PROVIDER
// and an operator-supplied RESEND_API_KEY (Manual ones are left alone). Existing values are never
// replaced; all writes for an environment land in one commit (one deploy).
func (setup *setup) bootstrap(ctx context.Context, env string) error {
	if setup.project == nil || setup.project.Environments[env] == "" || !setup.servicesExist() {
		setup.change("bootstrap secrets in %s (auth secrets generated, SECRETS_PROVIDER, Doppler token placeholder)", env)
		return nil
	}
	writes := map[string]map[string]secretWrite{} // service -> name -> value
	current := map[string]map[string]*string{}
	for _, svc := range setup.allServices() {
		vars, err := setup.variables(ctx, svc, env)
		if err != nil {
			return err
		}
		current[svc] = vars
		writes[svc] = map[string]secretWrite{}
	}
	for _, sec := range setup.cfg.SecretVars() {
		_, exists := current[sec.Service][sec.Name]
		if setup.onDoppler(current[sec.Service]) && (sec.Kind == platform.Generated || sec.Kind == platform.Operator) {
			continue // Doppler holds them; Railway copies were deleted on purpose
		}
		switch sec.Kind {
		case platform.Generated:
			if !exists {
				secret, err := randomSecret()
				if err != nil {
					return err
				}
				writes[sec.Service][sec.Name] = secretWrite{secret, false} // not sealed: see platform.Generated
				setup.change("%s/%s %s: generate", env, sec.Service, sec.Name)
			}
		case platform.DopplerToken:
			if !exists && setup.cfg.Secrets.Provider == platform.ProviderDoppler {
				writes[sec.Service][sec.Name] = secretWrite{platform.DopplerPlaceholder, false}
				setup.change("%s/%s DOPPLER_TOKEN: placeholder (add the real token: make railway-secrets)", env, sec.Service)
			}
		case platform.Operator:
			if exists {
				continue
			}
			if value := operatorValue(sec.Name, sec.Service, env); value != "" {
				writes[sec.Service][sec.Name] = secretWrite{value, true}
				setup.change("%s/%s %s: from your shell (sealed)", env, sec.Service, sec.Name)
				continue
			}
			// Production refuses to boot without it; a placeholder gets the
			// first deploy live. Not sealed, so setup can tell it apart.
			writes[sec.Service][sec.Name] = secretWrite{platform.ResendPlaceholder, false}
			setup.change("%s/%s %s: placeholder (auth emails fail until you set a real key)", env, sec.Service, sec.Name)
		}
	}
	for _, sec := range setup.cfg.SecretVars() {
		if sec.Kind != platform.Provider {
			continue
		}
		// Only the Doppler phase switches a service to doppler: it waits for
		// the new deployment before removing the Railway copies, and leaves a
		// staged environment on env. Here: a missing value, and back to env
		// when the provider is no longer doppler.
		provider, ok := current[sec.Service][sec.Name]
		backToEnv := setup.cfg.Secrets.Provider != platform.ProviderDoppler && (provider == nil || *provider != platform.ProviderEnv)
		if !ok || backToEnv {
			writes[sec.Service][sec.Name] = secretWrite{platform.ProviderEnv, false}
			setup.change("%s/%s SECRETS_PROVIDER=%s", env, sec.Service, platform.ProviderEnv)
		}
	}
	return setup.commit(ctx, env, writes)
}

// realToken: DOPPLER_TOKEN is set and is not the placeholder (sealed counts
// as real: only setup seals).
func realToken(vars map[string]*string) bool {
	token, ok := vars["DOPPLER_TOKEN"]
	return ok && (token == nil || (*token != "" && *token != platform.DopplerPlaceholder))
}

// onDoppler: the service reads Doppler now (SECRETS_PROVIDER=doppler with a
// real token), so Railway no longer needs its secrets. A staged environment
// has a real token but still reads Railway.
func (setup *setup) onDoppler(vars map[string]*string) bool {
	provider := vars["SECRETS_PROVIDER"]
	return setup.cfg.Secrets.Provider == platform.ProviderDoppler && provider != nil && *provider == platform.ProviderDoppler && realToken(vars)
}

// commit writes variables through one environment patch (a deploy of the
// services it touches); sealed values cannot be read back by anyone,
// including the CLI and the dashboard.
func (setup *setup) commit(ctx context.Context, env string, writes map[string]map[string]secretWrite) error {
	return setup.commitVars(ctx, env, writes, false)
}

// commitStaged is commit without a deploy: the running deployments keep
// their variables until the next one.
func (setup *setup) commitStaged(ctx context.Context, env string, writes map[string]map[string]secretWrite) error {
	return setup.commitVars(ctx, env, writes, true)
}

func (setup *setup) commitVars(ctx context.Context, env string, writes map[string]map[string]secretWrite, skipDeploys bool) error {
	services := map[string]any{}
	for svc, vars := range writes {
		if len(vars) == 0 {
			continue
		}
		patch := map[string]any{}
		for name, write := range vars {
			lookup := map[string]any{"value": write.value}
			if write.sealed {
				lookup["isSealed"] = true
			}
			patch[name] = lookup
		}
		services[setup.project.Services[svc]] = map[string]any{"variables": patch}
	}
	if len(services) == 0 || !setup.apply {
		return nil
	}
	return setup.rw.gql(ctx, `mutation($env: String!, $patch: EnvironmentConfig, $skip: Boolean) { environmentPatchCommit(environmentId: $env, patch: $patch, skipDeploys: $skip, commitMessage: "railway-setup: bootstrap variables") }`,
		map[string]any{"env": setup.project.Environments[env], "patch": map[string]any{"services": services}, "skip": skipDeploys}, nil)
}

// ensureDomains gives each public service a Railway-generated domain, which
// PUBLIC_URL / ADMIN_PUBLIC_URL reference as ${{RAILWAY_PUBLIC_DOMAIN}}.
func (setup *setup) ensureDomains(ctx context.Context, env string) error {
	for _, svc := range setup.cfg.PublicServices() {
		if setup.project == nil || setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
			setup.change("generate a public domain for %s in %s", svc, env)
			continue
		}
		domain, err := setup.domain(ctx, svc, env)
		if err != nil {
			return err
		}
		if domain != "" {
			continue
		}
		if setup.apply {
			if _, err := setup.rw.output(ctx, nil, "domain", "--service", svc, "--environment", env, "--port", fmt.Sprint(platform.AppPort), "--json"); err != nil {
				return err
			}
			domain, _ = setup.domain(ctx, svc, env)
		}
		setup.change("%s/%s public domain %s", env, svc, domain)
	}
	return nil
}

func (setup *setup) domain(ctx context.Context, svc, env string) (string, error) {
	var res struct {
		Domains []struct {
			Domain string `json:"domain"`
			Type   string `json:"type"`
		} `json:"domains"`
	}
	if err := setup.rw.json(ctx, &res, "domain", "list", "--service", svc, "--environment", env); err != nil {
		return "", err
	}
	for _, domain := range res.Domains {
		if strings.HasSuffix(domain.Domain, ".up.railway.app") {
			return domain.Domain, nil
		}
	}
	if len(res.Domains) > 0 {
		return res.Domains[0].Domain, nil
	}
	return "", nil
}

func (setup *setup) services() []string {
	if setup.cfg.Admin {
		return []string{platform.ServiceWeb, platform.ServiceAdmin}
	}
	return []string{platform.ServiceWeb}
}

// allServices are services() plus the backup cron when backups are on:
// everything setup deploys, bootstraps and redeploys. Doppler prompts and
// checks use services(): the backup job reads Railway references.
func (setup *setup) allServices() []string {
	if setup.cfg.Backups.Enabled {
		return append(setup.services(), platform.ServiceBackup)
	}
	return setup.services()
}

func (setup *setup) servicesExist() bool {
	for _, svc := range setup.allServices() {
		if setup.project.Services[svc] == "" {
			return false
		}
	}
	return true
}

// secrets sets real Doppler tokens (and RESEND_API_KEY for services on the
// env provider), sealed, then points SECRETS_PROVIDER at doppler. Values come
// from DOPPLER_TOKEN_<SERVICE>_<ENV> / RESEND_API_KEY_[<SERVICE>_]<ENV> or a
// hidden prompt; empty skips.
func (setup *setup) secrets(ctx context.Context) error {
	if setup.project == nil {
		return errors.New("the project does not exist yet: run make railway-apply first")
	}
	in := bufio.NewReader(os.Stdin)
	for _, env := range setup.cfg.Environments {
		if setup.project.Environments[env] == "" {
			continue
		}
		writes, staged := map[string]map[string]secretWrite{}, map[string]map[string]secretWrite{}
		for _, svc := range setup.services() {
			if setup.project.Services[svc] == "" {
				continue
			}
			writes[svc] = map[string]secretWrite{}
			current, err := setup.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			if setup.cfg.Secrets.Provider == platform.ProviderDoppler {
				if ask := setup.ask(ctx, in, "DOPPLER_TOKEN", svc, env); ask != "" {
					if setup.cfg.DopplerMode(env) == platform.SecretsStaged {
						staged[svc] = map[string]secretWrite{"DOPPLER_TOKEN": {ask, true}}
						setup.change("%s/%s DOPPLER_TOKEN set (sealed, staged: no deploy, still reads its Railway secrets)", env, svc)
						continue
					}
					// The Railway copies stay until make doppler-setup sees it healthy.
					writes[svc]["DOPPLER_TOKEN"] = secretWrite{ask, true}
					writes[svc]["SECRETS_PROVIDER"] = secretWrite{platform.ProviderDoppler, false}
					setup.change("%s/%s DOPPLER_TOKEN set (sealed), SECRETS_PROVIDER=doppler", env, svc)
					continue
				}
			}
			if setup.onDoppler(current) {
				continue // RESEND_API_KEY comes from Doppler
			}
			if ask := setup.ask(ctx, in, "RESEND_API_KEY", svc, env); ask != "" {
				writes[svc]["RESEND_API_KEY"] = secretWrite{ask, true}
				setup.change("%s/%s RESEND_API_KEY set (sealed)", env, svc)
			}
		}
		if err := setup.commit(ctx, env, writes); err != nil {
			return err
		}
		if err := setup.commitStaged(ctx, env, staged); err != nil {
			return err
		}
	}
	return nil
}

func (setup *setup) ask(ctx context.Context, in *bufio.Reader, name, svc, env string) string {
	if value := operatorValue(name, svc, env); value != "" {
		return value
	}
	if stty(ctx, "-g") != nil { // not a terminal (/dev/null is a char device too)
		return ""
	}
	fmt.Fprintf(setup.out, "%s for %s/%s (input hidden, empty to skip): ", name, env, svc)
	if err := stty(ctx, "-echo"); err == nil {
		defer func() { _ = stty(context.WithoutCancel(ctx), "echo"); fmt.Fprintln(setup.out) }()
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func stty(ctx context.Context, arg string) error {
	cmd := exec.CommandContext(ctx, "stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// operatorValue reads NAME_<SERVICE>_<ENV>, then NAME_<ENV> (not for
// DOPPLER_TOKEN: one token per service and environment), from the shell.
// Never the bare NAME: make exports .env, and a local dev value must not
// reach Railway.
func operatorValue(name, svc, env string) string {
	up := strings.NewReplacer("-", "_").Replace
	if value := os.Getenv(strings.ToUpper(up(name + "_" + svc + "_" + env))); value != "" {
		return value
	}
	if name == "DOPPLER_TOKEN" {
		return ""
	}
	return os.Getenv(strings.ToUpper(up(name + "_" + env)))
}

func randomSecret() (string, error) {
	const secretBytes = 48 // random bytes in a generated secret (64 base64url characters)
	data := make([]byte, secretBytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
