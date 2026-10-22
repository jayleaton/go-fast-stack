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
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/jayleaton/ai-patterns-go/deploy/platform"
)

// setup reconciles one Railway project with platform.yaml. Steps IaC covers
// (services, databases, variables, builds, healthchecks) go through
// `railway config plan|apply`; the rest (project, environments, previews,
// bootstrap secrets, generated domains) is done here, idempotently.
type setup struct {
	cfg      platform.Config
	cfgPath  string // absolute; passed to the IaC file as PLATFORM_CONFIG
	iacFile  string // absolute path of deploy/.railway/railway.go
	rw       *cli
	out      io.Writer
	apply    bool
	destruct bool

	root    string   // repository root: what railway up uploads
	ws      string   // resolved workspace id
	project *project // nil until it exists
	pending int      // changes a plan found
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

func (e edges) byName() map[string]string {
	m := map[string]string{}
	for _, n := range e.Edges {
		m[n.Node.Name] = n.Node.ID
	}
	return m
}

func (s *setup) say(format string, a ...any) { fmt.Fprintf(s.out, format+"\n", a...) }

// change records a change: done in apply mode, pending in plan mode.
func (s *setup) change(format string, a ...any) {
	if s.apply {
		s.say("  ✓ "+format, a...)
	} else {
		s.pending++
		s.say("  + "+format, a...)
	}
}

// findProject loads the project named in platform.yaml, if it exists.
// Only the configured workspace is searched (or your only one), so a
// same-named project elsewhere is never touched.
func (s *setup) findProject(ctx context.Context) error {
	ws, err := s.workspaceID(ctx)
	if err != nil {
		return err
	}
	var all []listedProject
	if err := s.rw.json(ctx, &all, "list"); err != nil {
		return err
	}
	var hits []listedProject
	for _, p := range all {
		if p.Name == s.cfg.Project && p.Workspace.ID == ws {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		s.project = nil
	case 1:
		p := hits[0]
		s.project = &project{ID: p.ID, Name: p.Name, Workspace: p.Workspace.Name,
			Environments: p.Environments.byName(), Services: p.Services.byName()}
		s.rw.projectID = p.ID
	default:
		return fmt.Errorf("%d projects named %q in this workspace: rename one", len(hits), s.cfg.Project)
	}
	return nil
}

func (s *setup) workspaceID(ctx context.Context) (string, error) {
	if s.ws != "" {
		return s.ws, nil
	}
	id, err := s.lookupWorkspace(ctx)
	s.ws = id
	return id, err
}

func (s *setup) lookupWorkspace(ctx context.Context) (string, error) {
	var me struct {
		Workspaces []struct{ ID, Name string } `json:"workspaces"`
	}
	if err := s.rw.json(ctx, &me, "whoami"); err != nil {
		return "", err
	}
	var names []string
	for _, w := range me.Workspaces {
		if s.cfg.Workspace != "" && (w.ID == s.cfg.Workspace || w.Name == s.cfg.Workspace) {
			return w.ID, nil
		}
		names = append(names, fmt.Sprintf("%q", w.Name))
	}
	if s.cfg.Workspace == "" && len(me.Workspaces) == 1 {
		return me.Workspaces[0].ID, nil
	}
	if s.cfg.Workspace != "" {
		return "", fmt.Errorf("workspace %q not found; yours: %s", s.cfg.Workspace, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("you belong to several workspaces; set workspace in platform.yaml to one of %s", strings.Join(names, ", "))
}

// ensureProject creates the project (with its production environment).
func (s *setup) ensureProject(ctx context.Context) error {
	if err := s.findProject(ctx); err != nil || s.project != nil {
		if s.project != nil {
			s.say("project %s (%s) in %s", s.project.Name, s.project.ID, s.project.Workspace)
		}
		return err
	}
	ws, err := s.workspaceID(ctx)
	if err != nil {
		return err
	}
	if !s.apply {
		s.change("create project %q", s.cfg.Project)
		return nil
	}
	var res struct {
		ProjectCreate struct{ ID string } `json:"projectCreate"`
	}
	err = s.rw.gql(ctx, `mutation($input: ProjectCreateInput!) { projectCreate(input: $input) { id } }`,
		map[string]any{"input": map[string]any{
			"name": s.cfg.Project, "workspaceId": ws, "defaultEnvironmentName": "production",
			"isMonorepo": true, "prDeploys": false,
		}}, &res)
	if err != nil {
		return err
	}
	s.change("created project %q (%s)", s.cfg.Project, res.ProjectCreate.ID)
	if err := s.findProject(ctx); err != nil {
		return err
	}
	if s.project == nil {
		return fmt.Errorf("created project %s but cannot list it yet: re-run in a minute (it will be reused, not duplicated)", res.ProjectCreate.ID)
	}
	return nil
}

// ensureEnvironments creates missing environments empty (never duplicated
// from production: no copied data or secrets) and only warns about extras.
func (s *setup) ensureEnvironments(ctx context.Context) error {
	for _, env := range s.cfg.Environments {
		if s.project != nil && s.project.Environments[env] != "" {
			continue
		}
		if s.project == nil && env == "production" {
			continue // created with the project
		}
		if !s.apply {
			s.change("create environment %s", env)
			continue
		}
		// Railway allows one environment create per 30 s per project.
		var err error
		for range 4 {
			err = s.rw.gql(ctx, `mutation($input: EnvironmentCreateInput!) { environmentCreate(input: $input) { id } }`,
				map[string]any{"input": map[string]any{"projectId": s.project.ID, "name": env, "skipInitialDeploys": true}}, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "rate limit") {
				break
			}
			s.say("  … Railway rate limit on environment create; retrying in 31s")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(31 * time.Second):
			}
		}
		if err != nil {
			return fmt.Errorf("creating environment %s: %w", env, err)
		}
		s.change("created environment %s", env)
	}
	if s.project == nil {
		return nil
	}
	for name := range s.project.Environments {
		if !s.cfg.Has(name) && !strings.Contains(name, "-pr-") {
			s.say("  ! environment %s exists on Railway but not in platform.yaml; it is left alone (delete it with: railway environment delete %s)", name, name)
		}
	}
	return s.findProject(ctx)
}

// ensurePreviews turns Railway PR environments on or off. Previews copy the
// base environment: staging when there is one, never production otherwise.
func (s *setup) ensurePreviews(ctx context.Context) error {
	if s.project == nil {
		if s.cfg.Previews {
			s.change("enable PR environments")
		}
		return nil
	}
	var res struct {
		Project struct {
			PrDeploys         bool    `json:"prDeploys"`
			BaseEnvironmentID *string `json:"baseEnvironmentId"`
		} `json:"project"`
	}
	if err := s.rw.gql(ctx, `query($id: String!) { project(id: $id) { prDeploys baseEnvironmentId } }`,
		map[string]any{"id": s.project.ID}, &res); err != nil {
		return err
	}
	input := map[string]any{}
	if res.Project.PrDeploys != s.cfg.Previews {
		input["prDeploys"] = s.cfg.Previews
	}
	if s.cfg.Previews {
		base := s.cfg.Environments[0] // staging, or production in the production-only profile
		if id := s.project.Environments[base]; id != "" && (res.Project.BaseEnvironmentID == nil || *res.Project.BaseEnvironmentID != id) {
			input["baseEnvironmentId"] = id
		}
	}
	if len(input) == 0 {
		return nil
	}
	if s.apply {
		if err := s.rw.gql(ctx, `mutation($id: String!, $input: ProjectUpdateInput!) { projectUpdate(id: $id, input: $input) { id } }`,
			map[string]any{"id": s.project.ID, "input": input}, nil); err != nil {
			return err
		}
	}
	s.change("PR environments: %v (base %s)", s.cfg.Previews, s.cfg.Environments[0])
	return nil
}

// iac runs `railway config plan|apply` for env. Plan streams the CLI's own
// diff; apply runs with --json so per-change failures and their diagnostics
// are printed (the CLI's text output only says "failed changes: change").
func (s *setup) iac(ctx context.Context, env string) error {
	if s.project == nil || s.project.Environments[env] == "" {
		s.change("apply the IaC graph to %s (new environment: everything is created)", env)
		return nil
	}
	if !s.apply {
		code, err := s.rw.stream(ctx, "config", "plan", "--file", s.iacFile, "--environment", env, "--detailed-exit-code")
		switch {
		case err != nil:
			return err
		case code == 2:
			s.pending++
		case code != 0:
			return fmt.Errorf("railway config plan for %s failed with exit code %d", env, code)
		}
		return nil
	}
	args := []string{"config", "apply", "--file", s.iacFile, "--environment", env, "--yes", "--json"}
	if s.destruct {
		args = append(args, "--confirm-destructive")
	}
	out, runErr := s.rw.output(ctx, nil, args...)
	failed := runErr != nil
	if runErr == nil || len(bytes.TrimSpace(out)) > 0 {
		failed = s.renderApply(out) || failed
	}
	if runErr != nil || failed {
		hint := ""
		if !s.destruct {
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
func (s *setup) renderApply(out []byte) bool {
	if len(bytes.TrimSpace(out)) == 0 { // the CLI prints nothing, not even {}, when nothing changes
		s.say("  no changes")
		return false
	}
	var doc any
	if json.Unmarshal(out, &doc) != nil {
		s.say("%s", tail(string(out))) // plan output: values are redacted by the CLI
		return true
	}
	failed, seen := false, map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			kind, _ := v["kind"].(string)
			status, _ := v["status"].(string)
			if kind != "" && status != "" {
				what, _ := v["summary"].(string)
				if what == "" {
					what, _ = v["path"].(string)
				}
				line := fmt.Sprintf("  %s %s %s", status, kind, what)
				if !seen[line] {
					seen[line] = true
					s.say("%s", line)
				}
				failed = failed || status == "failed"
			}
			if msg, ok := v["message"].(string); ok {
				sev, _ := v["severity"].(string)
				line := fmt.Sprintf("  %s %s", strings.ToUpper(firstOr(sev, "note")), msg)
				if !seen[line] && sev != "warning" { // warnings: "ignore pattern matches no variable" before bootstrap
					seen[line] = true
					s.say("%s", line)
				}
			}
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	if len(seen) == 0 {
		s.say("  no changes")
	}
	return failed
}

func firstOr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// up uploads this checkout to every app service in env with `railway up`
// (upload mode, repo: ""), waiting for each build. Railway builds it with
// the service's Dockerfile and runs the pre-deploy migration.
func (s *setup) up(ctx context.Context, env string) error {
	if s.project == nil || s.project.Environments[env] == "" || !s.servicesExist() {
		return fmt.Errorf("%s is not set up yet: run make railway-apply first", env)
	}
	for _, svc := range s.services() {
		s.say("== %s/%s: railway up", env, svc)
		code, err := s.rw.streamIn(ctx, s.root, "up", "--service", svc, "--environment", env, "--ci", "-m", "make railway-up")
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("railway up %s/%s failed with exit code %d", env, svc, code)
		}
	}
	return nil
}

// repairDatabases fixes a Railway IaC bug (docs/railway-go-iac-issues.md #0):
// a database added to a second environment keeps POSTGRES_PASSWORD as the
// unevaluated generator text `secret(32, "...")`, so its password is the
// same public string everywhere and DATABASE_URL does not parse (spaces).
// It sets a random password inside the running container (railway ssh,
// SQL on stdin), then stores it in POSTGRES_PASSWORD.
func (s *setup) repairDatabases(ctx context.Context, env string) error {
	if s.project == nil || s.project.Environments[env] == "" {
		return nil
	}
	for _, db := range s.databases() {
		if s.project.Services[db] == "" {
			continue
		}
		vars, err := s.variables(ctx, db, env)
		if err != nil {
			return err
		}
		pw := vars["POSTGRES_PASSWORD"]
		if pw == nil || !strings.Contains(*pw, "secret(") {
			continue
		}
		if !s.apply {
			s.change("%s/%s POSTGRES_PASSWORD is Railway's unevaluated generator: set a random one", env, db)
			continue
		}
		if err := s.waitDeployed(ctx, db, env); err != nil {
			return err
		}
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		fresh := hex.EncodeToString(b) // URL-safe: DATABASE_URL embeds it unescaped
		user := "postgres"
		if u := vars["POSTGRES_USER"]; u != nil && *u != "" {
			user = *u
		}
		sql := fmt.Sprintf("ALTER USER %q WITH PASSWORD '%s';\n", user, fresh)
		if _, err := s.rw.output(ctx, []byte(sql), "ssh", "--service", db, "--environment", env, "--", "psql", "-U", user, "-v", "ON_ERROR_STOP=1", "-qtA"); err != nil {
			return fmt.Errorf("%s/%s: setting a real password over railway ssh (needs an SSH key: railway ssh keys): %w", env, db, err)
		}
		if err := s.commit(ctx, env, map[string]map[string]secretWrite{db: {"POSTGRES_PASSWORD": {fresh, false}}}); err != nil {
			return err
		}
		s.change("%s/%s POSTGRES_PASSWORD was Railway's unevaluated generator: replaced with a random password", env, db)
	}
	return nil
}

// clearWatchPaths removes watch paths left on app services when a project
// moves from GitHub to upload mode: Railway would skip every `railway up`
// that changes nothing under them. IaC cannot express "none" (see graph.go).
func (s *setup) clearWatchPaths(ctx context.Context, env string) error {
	if !s.cfg.Upload() || s.project == nil || s.project.Environments[env] == "" {
		return nil
	}
	var cfg struct {
		Services map[string]struct {
			Build struct {
				WatchPatterns []string `json:"watchPatterns"`
			} `json:"build"`
		} `json:"services"`
	}
	if err := s.rw.json(ctx, &cfg, "environment", "config", "--environment", env); err != nil {
		return err
	}
	services := map[string]any{}
	for _, svc := range s.services() {
		id := s.project.Services[svc]
		if id == "" || len(cfg.Services[id].Build.WatchPatterns) == 0 {
			continue
		}
		services[id] = map[string]any{"build": map[string]any{"watchPatterns": []string{}}}
		s.change("%s/%s clear watch paths (upload mode)", env, svc)
	}
	if len(services) == 0 || !s.apply {
		return nil
	}
	return s.rw.gql(ctx, `mutation($env: String!, $patch: EnvironmentConfig) { environmentPatchCommit(environmentId: $env, patch: $patch, commitMessage: "railway-setup: upload mode has no watch paths") }`,
		map[string]any{"env": s.project.Environments[env], "patch": map[string]any{"services": services}}, nil)
}

func (s *setup) databases() []string {
	if s.cfg.Admin {
		return []string{platform.ServicePostgres, platform.ServiceAdminPostgres}
	}
	return []string{platform.ServicePostgres}
}

// waitDeployed waits up to 5 minutes for svc's latest deployment in env.
func (s *setup) waitDeployed(ctx context.Context, svc, env string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var deps []struct {
			Status string `json:"status"`
		}
		if err := s.rw.json(ctx, &deps, "deployment", "list", "--service", svc, "--environment", env); err != nil {
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
		case <-time.After(10 * time.Second):
		}
	}
}

// variables lists a service's variables in env: name -> value, nil when
// sealed. Values never leave this process.
func (s *setup) variables(ctx context.Context, service, env string) (map[string]*string, error) {
	vars := map[string]*string{}
	err := s.rw.json(ctx, &vars, "variable", "list", "--service", service, "--environment", env)
	return vars, err
}

type secretWrite struct {
	value  string
	sealed bool
}

// bootstrap provisions the variables IaC ignores (platform.SecretVars):
// generated auth secrets, the Doppler token placeholder, SECRETS_PROVIDER
// and an operator-supplied RESEND_API_KEY. Existing values are never
// replaced; all writes for an environment land in one commit (one deploy).
func (s *setup) bootstrap(ctx context.Context, env string) error {
	if s.project == nil || s.project.Environments[env] == "" || !s.servicesExist() {
		s.change("bootstrap secrets in %s (auth secrets generated and sealed, SECRETS_PROVIDER, Doppler token placeholder)", env)
		return nil
	}
	writes := map[string]map[string]secretWrite{} // service -> name -> value
	current := map[string]map[string]*string{}
	for _, svc := range s.services() {
		vars, err := s.variables(ctx, svc, env)
		if err != nil {
			return err
		}
		current[svc] = vars
		writes[svc] = map[string]secretWrite{}
	}
	for _, sec := range s.cfg.SecretVars() {
		_, exists := current[sec.Service][sec.Name]
		switch sec.Kind {
		case platform.Generated:
			if !exists {
				v, err := randomSecret()
				if err != nil {
					return err
				}
				writes[sec.Service][sec.Name] = secretWrite{v, true}
				s.change("%s/%s %s: generate (sealed)", env, sec.Service, sec.Name)
			}
		case platform.DopplerToken:
			if !exists && s.cfg.Secrets.Provider == platform.ProviderDoppler {
				writes[sec.Service][sec.Name] = secretWrite{platform.DopplerPlaceholder, false}
				s.change("%s/%s DOPPLER_TOKEN: placeholder (add the real token: make railway-secrets)", env, sec.Service)
			}
		case platform.Operator:
			if exists {
				continue
			}
			if v := operatorValue(sec.Name, sec.Service, env); v != "" {
				writes[sec.Service][sec.Name] = secretWrite{v, true}
				s.change("%s/%s %s: from your shell (sealed)", env, sec.Service, sec.Name)
			} else if !s.usesDoppler(current[sec.Service]) {
				s.say("  ! %s/%s %s is not set: the app refuses to boot without it. Run make railway-secrets (or add a Doppler token), or export %s_%s before make railway-apply.", env, sec.Service, sec.Name, sec.Name, strings.ToUpper(env))
			}
		}
	}
	for _, sec := range s.cfg.SecretVars() {
		if sec.Kind != platform.Provider {
			continue
		}
		want := platform.ProviderEnv
		if s.usesDoppler(current[sec.Service]) {
			want = platform.ProviderDoppler
		}
		if cur := current[sec.Service][sec.Name]; cur == nil || *cur != want {
			writes[sec.Service][sec.Name] = secretWrite{want, false}
			s.change("%s/%s SECRETS_PROVIDER=%s", env, sec.Service, want)
		}
	}
	return s.commit(ctx, env, writes)
}

// usesDoppler: the service should read Doppler when doppler is the
// provider and its token is real (sealed counts as real: only setup seals).
func (s *setup) usesDoppler(vars map[string]*string) bool {
	if s.cfg.Secrets.Provider != platform.ProviderDoppler {
		return false
	}
	tok, ok := vars["DOPPLER_TOKEN"]
	return ok && (tok == nil || (*tok != "" && *tok != platform.DopplerPlaceholder))
}

// commit writes variables through one environment patch; sealed values
// cannot be read back by anyone, including the CLI and the dashboard.
func (s *setup) commit(ctx context.Context, env string, writes map[string]map[string]secretWrite) error {
	services := map[string]any{}
	for svc, vars := range writes {
		if len(vars) == 0 {
			continue
		}
		patch := map[string]any{}
		for name, w := range vars {
			v := map[string]any{"value": w.value}
			if w.sealed {
				v["isSealed"] = true
			}
			patch[name] = v
		}
		services[s.project.Services[svc]] = map[string]any{"variables": patch}
	}
	if len(services) == 0 || !s.apply {
		return nil
	}
	return s.rw.gql(ctx, `mutation($env: String!, $patch: EnvironmentConfig) { environmentPatchCommit(environmentId: $env, patch: $patch, commitMessage: "railway-setup: bootstrap variables") }`,
		map[string]any{"env": s.project.Environments[env], "patch": map[string]any{"services": services}}, nil)
}

// ensureDomains gives each public service a Railway-generated domain, which
// PUBLIC_URL / ADMIN_PUBLIC_URL reference as ${{RAILWAY_PUBLIC_DOMAIN}}.
func (s *setup) ensureDomains(ctx context.Context, env string) error {
	for _, svc := range s.cfg.PublicServices() {
		if s.project == nil || s.project.Environments[env] == "" || s.project.Services[svc] == "" {
			s.change("generate a public domain for %s in %s", svc, env)
			continue
		}
		d, err := s.domain(ctx, svc, env)
		if err != nil {
			return err
		}
		if d != "" {
			continue
		}
		if s.apply {
			if _, err := s.rw.output(ctx, nil, "domain", "--service", svc, "--environment", env, "--port", fmt.Sprint(platform.AppPort), "--json"); err != nil {
				return err
			}
			d, _ = s.domain(ctx, svc, env)
		}
		s.change("%s/%s public domain %s", env, svc, d)
	}
	return nil
}

func (s *setup) domain(ctx context.Context, svc, env string) (string, error) {
	var res struct {
		Domains []struct {
			Domain string `json:"domain"`
			Type   string `json:"type"`
		} `json:"domains"`
	}
	if err := s.rw.json(ctx, &res, "domain", "list", "--service", svc, "--environment", env); err != nil {
		return "", err
	}
	for _, d := range res.Domains {
		if strings.HasSuffix(d.Domain, ".up.railway.app") {
			return d.Domain, nil
		}
	}
	if len(res.Domains) > 0 {
		return res.Domains[0].Domain, nil
	}
	return "", nil
}

func (s *setup) services() []string {
	if s.cfg.Admin {
		return []string{platform.ServiceWeb, platform.ServiceAdmin}
	}
	return []string{platform.ServiceWeb}
}

func (s *setup) servicesExist() bool {
	for _, svc := range s.services() {
		if s.project.Services[svc] == "" {
			return false
		}
	}
	return true
}

// secrets sets real Doppler tokens (and RESEND_API_KEY for services on the
// env provider), sealed, then points SECRETS_PROVIDER at doppler. Values come
// from DOPPLER_TOKEN_<SERVICE>_<ENV> / RESEND_API_KEY_[<SERVICE>_]<ENV> or a
// hidden prompt; empty skips.
func (s *setup) secrets(ctx context.Context) error {
	if s.project == nil {
		return errors.New("the project does not exist yet: run make railway-apply first")
	}
	in := bufio.NewReader(os.Stdin)
	for _, env := range s.cfg.Environments {
		if s.project.Environments[env] == "" {
			continue
		}
		writes := map[string]map[string]secretWrite{}
		for _, svc := range s.services() {
			if s.project.Services[svc] == "" {
				continue
			}
			writes[svc] = map[string]secretWrite{}
			current, err := s.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			if s.cfg.Secrets.Provider == platform.ProviderDoppler {
				if v := s.ask(ctx, in, "DOPPLER_TOKEN", svc, env); v != "" {
					writes[svc]["DOPPLER_TOKEN"] = secretWrite{v, true}
					writes[svc]["SECRETS_PROVIDER"] = secretWrite{platform.ProviderDoppler, false}
					s.change("%s/%s DOPPLER_TOKEN set (sealed), SECRETS_PROVIDER=doppler", env, svc)
					continue
				}
			}
			if s.usesDoppler(current) {
				continue // RESEND_API_KEY comes from Doppler
			}
			if v := s.ask(ctx, in, "RESEND_API_KEY", svc, env); v != "" {
				writes[svc]["RESEND_API_KEY"] = secretWrite{v, true}
				s.change("%s/%s RESEND_API_KEY set (sealed)", env, svc)
			}
		}
		if err := s.commit(ctx, env, writes); err != nil {
			return err
		}
	}
	return nil
}

func (s *setup) ask(ctx context.Context, in *bufio.Reader, name, svc, env string) string {
	if v := operatorValue(name, svc, env); v != "" {
		return v
	}
	if stty(ctx, "-g") != nil { // not a terminal (/dev/null is a char device too)
		return ""
	}
	fmt.Fprintf(s.out, "%s for %s/%s (input hidden, empty to skip): ", name, env, svc)
	if err := stty(ctx, "-echo"); err == nil {
		defer func() { _ = stty(context.WithoutCancel(ctx), "echo"); fmt.Fprintln(s.out) }()
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
	if v := os.Getenv(strings.ToUpper(up(name + "_" + svc + "_" + env))); v != "" {
		return v
	}
	if name == "DOPPLER_TOKEN" {
		return ""
	}
	return os.Getenv(strings.ToUpper(up(name + "_" + env)))
}

func randomSecret() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// verify checks every public service's /healthz over its public domain.
func (s *setup) verify(ctx context.Context) error {
	if s.project == nil {
		return errors.New("the project does not exist yet")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var failed []string
	for _, env := range s.cfg.Environments {
		for _, svc := range s.cfg.PublicServices() {
			if s.project.Environments[env] == "" || s.project.Services[svc] == "" {
				failed = append(failed, env+"/"+svc+": missing")
				continue
			}
			d, err := s.domain(ctx, svc, env)
			if err != nil || d == "" {
				failed = append(failed, fmt.Sprintf("%s/%s: no domain (%v)", env, svc, err))
				continue
			}
			url := "https://" + d + "/healthz"
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			resp, err := client.Do(req)
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s/%s: %v", env, svc, err))
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			_ = resp.Body.Close()
			s.say("%-11s %-6s %s -> %d %s", env, svc, url, resp.StatusCode, strings.TrimSpace(string(body)))
			if resp.StatusCode != http.StatusOK {
				failed = append(failed, fmt.Sprintf("%s/%s: HTTP %d", env, svc, resp.StatusCode))
			}
		}
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		return fmt.Errorf("unhealthy: %s", strings.Join(failed, "; "))
	}
	return nil
}
