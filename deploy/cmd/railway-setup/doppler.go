package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// dopplerVersion is the CLI version pinned in mise.toml.
const dopplerVersion = "3.77.0"

// dopplerCLI runs the Doppler CLI as the logged-in person (doppler login).
// Secret values and tokens travel only on stdin/stdout of this process:
// never argv, logs or error messages.
type dopplerCLI struct {
	bin     string
	project string
}

func newDoppler(project string) (*dopplerCLI, error) {
	bin := os.Getenv("DOPPLER_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("doppler"); err != nil {
			return nil, errors.New("the Doppler CLI is not installed: mise install (mise.toml pins doppler " + dopplerVersion + ")")
		}
	}
	return &dopplerCLI{bin: bin, project: project}, nil
}

func (cli *dopplerCLI) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, cli.bin, args...)
	// The person's login, never a token from the environment this tool runs in.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(email string) bool {
		return strings.HasPrefix(email, "DOPPLER_TOKEN=") || strings.HasPrefix(email, "DOPPLER_PROJECT=") || strings.HasPrefix(email, "DOPPLER_CONFIG=")
	})
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		const callNameArgs = 2 // args that name the call ("secrets set"); the rest may carry values
		return nil, fmt.Errorf("doppler %s: %w%s", strings.Join(args[:min(callNameArgs, len(args))], " "), err, tail(errb.String()))
	}
	return out.Bytes(), nil
}

func (cli *dopplerCLI) json(ctx context.Context, value any, args ...string) error {
	data, err := cli.run(ctx, nil, append(args, "--json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// whoami returns the workplace the CLI is logged in to.
func (cli *dopplerCLI) whoami(ctx context.Context) (string, error) {
	var me struct {
		Workplace struct {
			Name string `json:"name"`
		} `json:"workplace"`
	}
	if err := cli.json(ctx, &me, "me"); err != nil {
		return "", fmt.Errorf("not logged in to Doppler: run doppler login (%w)", err)
	}
	return me.Workplace.Name, nil
}

func (cli *dopplerCLI) version(ctx context.Context) string {
	data, err := cli.run(ctx, nil, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "v")
}

func (cli *dopplerCLI) names(ctx context.Context, list string, field string) (map[string]bool, error) {
	var items []map[string]any
	args := []string{list}
	if list != "projects" {
		args = append(args, "-p", cli.project)
	}
	if err := cli.json(ctx, &items, args...); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, it := range items {
		if text, ok := it[field].(string); ok {
			out[text] = true
		}
	}
	return out, nil
}

// secretNames lists the names (never values) in a config.
func (cli *dopplerCLI) secretNames(ctx context.Context, config string) (map[string]bool, error) {
	var lookup map[string]any
	if err := cli.json(ctx, &lookup, "secrets", "-p", cli.project, "-c", config, "--only-names"); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for key := range lookup {
		out[key] = true
	}
	return out, nil
}

// values reads a config's values into memory, for comparing only: they are
// never printed, logged or returned to a caller outside this file.
func (cli *dopplerCLI) values(ctx context.Context, config string) (map[string]string, error) {
	body, err := cli.run(ctx, nil, "secrets", "download", "-p", cli.project, "-c", config, "--no-file", "--format", "json")
	if err != nil {
		return nil, err
	}
	var values map[string]string
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, errors.New("doppler secrets download: undecodable response") // never echo the body
	}
	return values, nil
}

func (cli *dopplerCLI) setSecret(ctx context.Context, config, name, value string) error {
	_, err := cli.run(ctx, []byte(value), "secrets", "set", name, "-p", cli.project, "-c", config, "--no-interactive", "--json")
	return err
}

type dopplerToken struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (cli *dopplerCLI) tokens(ctx context.Context, config string) ([]dopplerToken, error) {
	var ts []dopplerToken
	err := cli.json(ctx, &ts, "configs", "tokens", "-p", cli.project, "-c", config)
	return ts, err
}

// createToken makes a read-only service token; the value stays in memory.
func (cli *dopplerCLI) createToken(ctx context.Context, config, name string) (string, error) {
	data, err := cli.run(ctx, nil, "configs", "tokens", "create", name, "-p", cli.project, "-c", config, "--access", "read", "--json")
	if err != nil {
		return "", err
	}
	var item struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &item); err != nil || item.Token == "" {
		return "", errors.New("doppler configs tokens create: no token in the response")
	}
	return item.Token, nil
}

func (cli *dopplerCLI) revoke(ctx context.Context, config, slug string) error {
	_, err := cli.run(ctx, nil, "configs", "tokens", "revoke", "--slug", slug, "-p", cli.project, "-c", config, "--json")
	return err
}

// dopplerPhase moves the app secrets into Doppler (secrets.provider:
// doppler): project, environments and configs, seeded from Railway, one
// read-only service token per service and environment sealed in Railway as
// DOPPLER_TOKEN. A switched environment (per_environment.<env>.secrets
// doppler, the default) then gets SECRETS_PROVIDER=doppler and, once its
// new deployment is healthy, loses the Railway copies of its secrets; a
// staged one gets the token without a deploy and keeps reading Railway.
// Plan prints every step apply would take. required: fail instead of
// leaving a next step when the CLI is missing.
func (setup *setup) dopplerPhase(ctx context.Context, required bool, wait time.Duration) error {
	if setup.cfg.Secrets.Provider != platform.ProviderDoppler {
		if required {
			return errors.New("secrets.provider is not doppler in platform.yaml")
		}
		return nil
	}
	doppler, err := newDoppler(setup.cfg.Secrets.Project)
	if err == nil {
		_, err = doppler.whoami(ctx)
	}
	if err != nil {
		if required {
			return err
		}
		setup.next("Move secrets to Doppler (live rotation): %v, then make doppler-setup", err)
		return nil
	}
	if setup.project == nil && setup.apply {
		return errors.New("the Railway project does not exist yet: run make railway-setup first")
	}
	setup.say("\n== doppler (project %s)", doppler.project)
	exists, err := setup.dopplerLayout(ctx, doppler)
	if err != nil {
		return err
	}
	if setup.project == nil { // plan before the first apply: no services to read yet
		setup.change("doppler: once the services exist, for each of them: seed its config, seal DOPPLER_TOKEN, SECRETS_PROVIDER=doppler (a redeploy, unless its environment is staged), then remove its Railway copies once the new deployment is healthy")
		return nil
	}
	if setup.apply {
		if err := setup.dopplerLocal(ctx, doppler); err != nil {
			return err
		}
	}
	var switched []switchedService
	for _, env := range setup.cfg.Environments {
		for _, svc := range setup.services() {
			before, ok, err := setup.dopplerService(ctx, doppler, exists, env, svc)
			if err != nil {
				return err
			}
			if ok {
				switched = append(switched, switchedService{env, svc, before})
			}
		}
	}
	if !setup.apply {
		return setup.dopplerCopies(ctx, switched)
	}
	return setup.dopplerFinish(ctx, switched, wait)
}

// dopplerLayout ensures the project, one environment per Railway environment
// and the admin branch configs. It reports whether the project exists (in a
// plan it may not yet: then nothing can be read from it).
func (setup *setup) dopplerLayout(ctx context.Context, doppler *dopplerCLI) (bool, error) {
	projects, err := doppler.names(ctx, "projects", "name")
	if err != nil {
		return false, err
	}
	exists := projects[doppler.project]
	if !exists {
		if setup.apply {
			if _, err := doppler.run(ctx, nil, "projects", "create", doppler.project, "--json"); err != nil {
				return false, err
			}
			exists = true
		}
		setup.change("doppler: create project %s (environments dev, stg, prd)", doppler.project)
	}
	envs, configs := map[string]bool{}, map[string]bool{}
	if exists {
		if envs, err = doppler.names(ctx, "environments", "id"); err != nil {
			return false, err
		}
		if configs, err = doppler.names(ctx, "configs", "name"); err != nil {
			return false, err
		}
	} else { // a new project starts with these
		for _, slug := range []string{platform.DopplerLocal, "stg", "prd"} {
			envs[slug], configs[slug] = true, true
		}
	}
	for _, env := range setup.cfg.Environments {
		slug := platform.DopplerEnvironment(env)
		if !envs[slug] {
			if setup.apply {
				if _, err := doppler.run(ctx, nil, "environments", "create", strings.ToUpper(slug[:1])+slug[1:], slug, "-p", doppler.project, "--json"); err != nil {
					return false, err
				}
			}
			setup.change("doppler: environment %s", slug)
		}
		for _, svc := range setup.services() {
			config := platform.DopplerConfig(env, svc)
			if configs[config] || config == slug {
				continue
			}
			if setup.apply {
				if _, err := doppler.run(ctx, nil, "configs", "create", config, "-e", slug, "-p", doppler.project, "--json"); err != nil {
					return false, err
				}
			}
			setup.change("doppler: config %s (branch of %s: inherits it, which cannot read it)", config, slug)
		}
	}
	return exists, nil
}

// dopplerLocal gives the dev config an AUTH_SECRET and points this checkout
// at it (doppler setup), for USE_DOPPLER=1 make dev.
func (setup *setup) dopplerLocal(ctx context.Context, cli *dopplerCLI) error {
	have, err := cli.secretNames(ctx, platform.DopplerLocal)
	if err != nil {
		return err
	}
	if !have["AUTH_SECRET"] {
		secret, err := randomSecret()
		if err != nil {
			return err
		}
		if err := cli.setSecret(ctx, platform.DopplerLocal, "AUTH_SECRET", secret); err != nil {
			return err
		}
		setup.change("doppler: %s AUTH_SECRET generated (local dev)", platform.DopplerLocal)
	}
	cmd := exec.CommandContext(ctx, cli.bin, "setup", "-p", cli.project, "-c", platform.DopplerLocal, "--no-interactive")
	cmd.Dir = setup.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("doppler setup: %w%s", err, tail(string(out)))
	}
	return nil
}

type switchedService struct{ env, svc, before string } // before: latest deployment id at the switch

// dopplerService seeds one service's config from its Railway variables
// (names missing from Doppler only: a value in Doppler is never replaced),
// seals a fresh read-only token in Railway when it has none of ours, and in
// a switched environment sets SECRETS_PROVIDER=doppler (a redeploy). A
// staged environment's token is sealed without a deploy. project: the
// Doppler project exists (a plan before it does reads nothing). It reports
// whether the service was switched, with its latest deployment id from
// before.
func (setup *setup) dopplerService(ctx context.Context, doppler *dopplerCLI, project bool, env, svc string) (before string, switched bool, err error) {
	if setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
		return "", false, nil
	}
	config := platform.DopplerConfig(env, svc)
	vars, err := setup.variables(ctx, svc, env)
	if err != nil {
		return "", false, err
	}
	have := map[string]bool{}
	if project {
		if have, err = doppler.secretNames(ctx, config); err != nil {
			if setup.apply {
				return "", false, err
			}
			have = map[string]bool{} // plan: apply creates the config first
		}
	}
	if err := setup.dopplerSeed(ctx, doppler, env, svc, config, vars, have); err != nil {
		return "", false, err
	}
	if setup.onDoppler(vars) {
		return setup.retrySwitch(ctx, env, svc) // switched by an earlier run
	}
	if project {
		if err := setup.dopplerDrift(ctx, doppler, env, svc, config, vars, have); err != nil {
			return "", false, err
		}
	}

	writes := map[string]secretWrite{}
	name := platform.DopplerTokenName(setup.project.Name, svc)
	// A sealed token cannot be read back. When this config has no token of
	// ours, the one on Railway is a placeholder, or a hand-set one (make
	// railway-secrets) for a service not on Doppler yet: issue ours.
	issued := false
	if project && realToken(vars) {
		tokens, err := doppler.tokens(ctx, config)
		if err != nil && setup.apply {
			return "", false, err
		}
		issued = slices.ContainsFunc(tokens, func(token dopplerToken) bool { return token.Name == name })
	}
	if !issued {
		token := ""
		if setup.apply {
			if token, err = setup.issueToken(ctx, doppler, config, name); err != nil {
				return "", false, err
			}
		}
		writes["DOPPLER_TOKEN"] = secretWrite{token, true}
	}
	if setup.cfg.DopplerMode(env) == platform.SecretsStaged {
		if len(writes) == 0 {
			return "", false, nil
		}
		if setup.apply {
			if err := setup.commitStaged(ctx, env, map[string]map[string]secretWrite{svc: writes}); err != nil {
				return "", false, err
			}
		}
		setup.change("%s/%s DOPPLER_TOKEN for config %s sealed without a deploy (staged: still reads SECRETS_PROVIDER=%s and its Railway secrets)", env, svc, config, firstOr(deref(vars["SECRETS_PROVIDER"]), platform.ProviderEnv))
		return "", false, nil
	}
	writes["SECRETS_PROVIDER"] = secretWrite{platform.ProviderDoppler, false}
	if setup.apply {
		if before, _, err = setup.latestBuilt(ctx, svc, env); err != nil {
			return "", false, err
		}
		if err := setup.commit(ctx, env, map[string]map[string]secretWrite{svc: writes}); err != nil {
			return "", false, err
		}
	}
	sealed := ""
	if _, ok := writes["DOPPLER_TOKEN"]; ok {
		sealed = "DOPPLER_TOKEN sealed, "
	}
	setup.change("%s/%s reads Doppler config %s: %sSECRETS_PROVIDER=doppler (redeploys)", env, svc, config, sealed)
	return before, true, nil
}

// retrySwitch redeploys a service an earlier run switched to Doppler whose
// switch deployment failed (a bad value in Doppler, since fixed): its old
// deployment still runs on the Railway copies, and nothing else would deploy
// it again. With a GitHub source it deploys the branch's latest commit
// (--from-source: the newest deployment may be a skipped one); in upload
// mode the latest deployment again. It reports the retry like a switch, so
// dopplerFinish waits for it before removing any copy.
func (setup *setup) retrySwitch(ctx context.Context, env, svc string) (before string, retried bool, err error) {
	id, status, err := setup.latestBuilt(ctx, svc, env)
	if err != nil || (status != "FAILED" && status != "CRASHED") {
		return "", false, err
	}
	if setup.apply {
		args := []string{"redeploy", "--service", svc, "--environment", env, "--yes"}
		if !setup.cfg.Upload() {
			args = append(args, "--from-source")
		}
		if _, err := setup.rw.output(ctx, nil, args...); err != nil {
			return "", false, err
		}
	}
	setup.change("%s/%s reads Doppler but its last deployment %s: redeploy to retry", env, svc, strings.ToLower(status))
	return id, true, nil
}

// dopplerDrift warns about Doppler-owned names whose Railway value differs
// from Doppler's, for a service that still reads Railway: the switch would
// load Doppler's. Expected after a rotation made in Doppler; otherwise the
// Railway value changed after seeding (make railway-rotate-db, a hand edit)
// and Doppler needs it first. Values are compared in memory; only names are
// printed.
func (setup *setup) dopplerDrift(ctx context.Context, doppler *dopplerCLI, env, svc, config string, vars map[string]*string, have map[string]bool) error {
	var compare []string
	for _, seed := range platform.DopplerSeeds(svc) {
		if current := vars[seed.Name]; current != nil && *current != platform.ResendPlaceholder && have[seed.Name] {
			compare = append(compare, seed.Name)
		}
	}
	if len(compare) == 0 {
		return nil
	}
	inDoppler, err := doppler.values(ctx, config)
	if err != nil {
		return err
	}
	var differ []string
	for _, name := range compare {
		if value, ok := inDoppler[name]; ok && value != *vars[name] {
			differ = append(differ, name)
		}
	}
	if len(differ) > 0 {
		setup.say("  ! %s/%s: Railway and Doppler %s differ in %s; the switch uses Doppler's (fine after a rotation in Doppler, else update Doppler first)", env, svc, config, strings.Join(differ, ", "))
		setup.next("Check Doppler config %s before %s/%s reads it: %s differ from Railway", config, env, svc, strings.Join(differ, ", "))
	}
	return nil
}

// dopplerSeed copies a service's Railway values into its Doppler config, for
// the names the config lacks. A sealed Railway value cannot be read, so it
// can neither be copied nor later removed as a copy without losing it: the
// person sets it in Doppler instead.
func (setup *setup) dopplerSeed(ctx context.Context, doppler *dopplerCLI, env, svc, config string, vars map[string]*string, have map[string]bool) error {
	var added []string
	for _, seed := range platform.DopplerSeeds(svc) {
		if have[seed.Name] {
			continue
		}
		current, onRailway := vars[seed.Name]
		value, from := "", ""
		switch {
		case current != nil && *current != "" && *current != platform.ResendPlaceholder:
			value = *current
		case onRailway && current == nil:
			return fmt.Errorf("%s/%s: %s is sealed on Railway: set it in Doppler config %s by hand, then re-run", env, svc, seed.Name, config)
		case seed.Source == platform.IfOnRailway:
			continue
		case seed.Source == platform.RailwayOrRandom:
			if setup.apply {
				var err error
				if value, err = randomSecret(); err != nil {
					return err
				}
			}
			from = " (generated)"
		case seed.Source == platform.RailwayOrResend:
			value, from = platform.ResendPlaceholder, " (placeholder)"
		default:
			return fmt.Errorf("%s/%s: %s is missing on Railway: set it in Doppler config %s, then re-run", env, svc, seed.Name, config)
		}
		if setup.apply {
			if err := doppler.setSecret(ctx, config, seed.Name, value); err != nil {
				return err
			}
		}
		added = append(added, seed.Name+from)
	}
	if len(added) > 0 {
		setup.change("doppler: %s <- %s/%s Railway values: %s", config, env, svc, strings.Join(added, ", "))
	}
	return nil
}

// issueToken revokes tokens of ours from a run that never reached Railway,
// then creates a fresh read-only one; the value stays in memory.
func (setup *setup) issueToken(ctx context.Context, doppler *dopplerCLI, config, name string) (string, error) {
	old, err := doppler.tokens(ctx, config)
	if err != nil {
		return "", err
	}
	for _, token := range old {
		if token.Name == name {
			if err := doppler.revoke(ctx, config, token.Slug); err != nil {
				return "", err
			}
		}
	}
	return doppler.createToken(ctx, config, name)
}

// dopplerCopies (plan) lists the Railway copies apply would remove: now for
// services already on Doppler, after a healthy switch for the others.
func (setup *setup) dopplerCopies(ctx context.Context, switching []switchedService) error {
	for _, env := range setup.cfg.Environments {
		for _, svc := range setup.services() {
			if setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
				continue
			}
			vars, err := setup.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			later := slices.ContainsFunc(switching, func(pending switchedService) bool { return pending.env == env && pending.svc == svc })
			if left := copiesOn(vars, svc); len(left) > 0 && (setup.onDoppler(vars) || later) {
				when := "now"
				if later {
					when = "once its new deployment is healthy on Doppler"
				}
				setup.change("%s/%s remove Railway copies %s, %s (Doppler holds them)", env, svc, strings.Join(left, ", "), when)
			}
		}
	}
	return nil
}

// copiesOn lists the Doppler-owned names still set on a service's Railway.
func copiesOn(vars map[string]*string, svc string) []string {
	var out []string
	for _, name := range platform.RailwayCopies(svc) {
		if _, ok := vars[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// dopplerFinish waits for the switched services' new deployments and, only
// when every one of them is healthy on Doppler, removes the Railway copies
// of the secrets from each service that reads Doppler (with skipDeploys:
// the running deployment keeps working). A staged environment's services
// never read Doppler, so they keep theirs.
func (setup *setup) dopplerFinish(ctx context.Context, switched []switchedService, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var failed []string
	for _, pending := range switched {
		setup.say("Waiting for %s/%s to redeploy on Doppler…", pending.env, pending.svc)
		id, status, err := setup.latestBuilt(ctx, pending.svc, pending.env)
		for err == nil && (id == pending.before || !settled(status)) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(deployPollInterval):
			}
			id, status, err = setup.latestBuilt(ctx, pending.svc, pending.env)
		}
		if err != nil || id == pending.before || status != "SUCCESS" {
			failed = append(failed, fmt.Sprintf("%s/%s (%s)", pending.env, pending.svc, strings.ToLower(firstOr(status, "no new deployment"))))
		}
	}
	if len(failed) > 0 {
		setup.next("Not healthy on Doppler yet: %s. Check railway logs, fix the Doppler config, re-run make doppler-setup (Railway copies of the secrets were kept)", strings.Join(failed, ", "))
		_ = setup.health(ctx, 0) // fill the summary
		return withCode(exitUnhealthy, "not healthy on Doppler: %s", strings.Join(failed, ", "))
	}
	if err := setup.health(ctx, 0); err != nil {
		return err
	}
	for _, env := range setup.cfg.Environments {
		for _, svc := range setup.services() {
			if setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
				continue
			}
			vars, err := setup.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			if !setup.onDoppler(vars) {
				continue
			}
			gone := copiesOn(vars, svc)
			if len(gone) == 0 {
				continue
			}
			// The running deployment must be one that reads Doppler: after a
			// failed switch the old one still runs on these copies (and a
			// restart reads them again). Its variables say doppler either way.
			if _, status, err := setup.latestBuilt(ctx, svc, env); err != nil || status != "SUCCESS" {
				setup.next("%s/%s is set to read Doppler but its latest deployment is %s: fix it (railway logs), then re-run make doppler-setup; its Railway copies %s were kept", env, svc, strings.ToLower(firstOr(status, "missing")), strings.Join(gone, ", "))
				continue
			}
			if err := setup.deleteVariables(ctx, env, svc, gone); err != nil {
				return err
			}
			setup.change("%s/%s removed Railway copies %s (Doppler holds them)", env, svc, strings.Join(gone, ", "))
		}
	}
	return nil
}

// deployPollInterval is how often dopplerFinish reads a deployment's status.
const deployPollInterval = 10 * time.Second

// deleteVariables removes variables without triggering a deploy.
func (setup *setup) deleteVariables(ctx context.Context, env, svc string, names []string) error {
	vars := map[string]any{}
	for _, name := range names {
		vars[name] = nil
	}
	return setup.rw.gql(ctx, `mutation($env: String!, $patch: EnvironmentConfig) { environmentPatchCommit(environmentId: $env, patch: $patch, skipDeploys: true, commitMessage: "railway-setup: app secrets live in Doppler") }`,
		map[string]any{"env": setup.project.Environments[env], "patch": map[string]any{"services": map[string]any{
			setup.project.Services[svc]: map[string]any{"variables": vars},
		}}}, nil)
}
