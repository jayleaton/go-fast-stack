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

func (d *dopplerCLI) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.bin, args...)
	// The person's login, never a token from the environment this tool runs in.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "DOPPLER_TOKEN=") || strings.HasPrefix(e, "DOPPLER_PROJECT=") || strings.HasPrefix(e, "DOPPLER_CONFIG=")
	})
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("doppler %s: %w%s", strings.Join(args[:min(2, len(args))], " "), err, tail(errb.String()))
	}
	return out.Bytes(), nil
}

func (d *dopplerCLI) json(ctx context.Context, v any, args ...string) error {
	b, err := d.run(ctx, nil, append(args, "--json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// whoami returns the workplace the CLI is logged in to.
func (d *dopplerCLI) whoami(ctx context.Context) (string, error) {
	var me struct {
		Workplace struct {
			Name string `json:"name"`
		} `json:"workplace"`
	}
	if err := d.json(ctx, &me, "me"); err != nil {
		return "", fmt.Errorf("not logged in to Doppler: run doppler login (%w)", err)
	}
	return me.Workplace.Name, nil
}

func (d *dopplerCLI) version(ctx context.Context) string {
	b, err := d.run(ctx, nil, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "v")
}

func (d *dopplerCLI) names(ctx context.Context, list string, field string) (map[string]bool, error) {
	var items []map[string]any
	args := []string{list}
	if list != "projects" {
		args = append(args, "-p", d.project)
	}
	if err := d.json(ctx, &items, args...); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, it := range items {
		if s, ok := it[field].(string); ok {
			out[s] = true
		}
	}
	return out, nil
}

// secretNames lists the names (never values) in a config.
func (d *dopplerCLI) secretNames(ctx context.Context, config string) (map[string]bool, error) {
	var m map[string]any
	if err := d.json(ctx, &m, "secrets", "-p", d.project, "-c", config, "--only-names"); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out, nil
}

func (d *dopplerCLI) setSecret(ctx context.Context, config, name, value string) error {
	_, err := d.run(ctx, []byte(value), "secrets", "set", name, "-p", d.project, "-c", config, "--no-interactive", "--json")
	return err
}

type dopplerToken struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (d *dopplerCLI) tokens(ctx context.Context, config string) ([]dopplerToken, error) {
	var ts []dopplerToken
	err := d.json(ctx, &ts, "configs", "tokens", "-p", d.project, "-c", config)
	return ts, err
}

// createToken makes a read-only service token; the value stays in memory.
func (d *dopplerCLI) createToken(ctx context.Context, config, name string) (string, error) {
	b, err := d.run(ctx, nil, "configs", "tokens", "create", name, "-p", d.project, "-c", config, "--access", "read", "--json")
	if err != nil {
		return "", err
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &t); err != nil || t.Token == "" {
		return "", errors.New("doppler configs tokens create: no token in the response")
	}
	return t.Token, nil
}

func (d *dopplerCLI) revoke(ctx context.Context, config, slug string) error {
	_, err := d.run(ctx, nil, "configs", "tokens", "revoke", "--slug", slug, "-p", d.project, "-c", config, "--json")
	return err
}

// dopplerPhase moves the app secrets into Doppler (secrets.provider:
// doppler): project, environments and configs, seeded from Railway, one
// read-only service token per service and environment sealed in Railway as
// DOPPLER_TOKEN with SECRETS_PROVIDER=doppler, then the Railway copies of
// the app secrets are removed once the service runs healthy on Doppler.
// required: fail instead of leaving a next step when the CLI is missing.
func (s *setup) dopplerPhase(ctx context.Context, required bool, wait time.Duration) error {
	if s.cfg.Secrets.Provider != platform.ProviderDoppler {
		if required {
			return errors.New("secrets.provider is not doppler in platform.yaml")
		}
		return nil
	}
	d, err := newDoppler(s.cfg.Secrets.Project)
	if err == nil {
		_, err = d.whoami(ctx)
	}
	if err != nil {
		if required {
			return err
		}
		s.next("Move secrets to Doppler (live rotation): %v, then make doppler-setup", err)
		return nil
	}
	if s.project == nil {
		return errors.New("the Railway project does not exist yet: run make railway-setup first")
	}
	s.say("\n== doppler (project %s)", d.project)
	if err := s.dopplerLayout(ctx, d); err != nil {
		return err
	}
	if !s.apply {
		return nil
	}
	if err := s.dopplerLocal(ctx, d); err != nil {
		return err
	}
	var switched []switchedService
	for _, env := range s.cfg.Environments {
		for _, svc := range s.services() {
			before, ok, err := s.dopplerService(ctx, d, env, svc)
			if err != nil {
				return err
			}
			if ok {
				switched = append(switched, switchedService{env, svc, before})
			}
		}
	}
	return s.dopplerFinish(ctx, switched, wait)
}

// dopplerLayout ensures the project, one environment per Railway environment
// and the admin branch configs.
func (s *setup) dopplerLayout(ctx context.Context, d *dopplerCLI) error {
	projects, err := d.names(ctx, "projects", "name")
	if err != nil {
		return err
	}
	if !projects[d.project] {
		if !s.apply {
			s.change("doppler: create project %s", d.project)
			return nil
		}
		if _, err := d.run(ctx, nil, "projects", "create", d.project, "--json"); err != nil {
			return err
		}
		s.change("doppler: created project %s (environments dev, stg, prd)", d.project)
	}
	envs, err := d.names(ctx, "environments", "id")
	if err != nil {
		return err
	}
	configs, err := d.names(ctx, "configs", "name")
	if err != nil {
		return err
	}
	for _, env := range s.cfg.Environments {
		slug := platform.DopplerEnvironment(env)
		if !envs[slug] {
			if s.apply {
				if _, err := d.run(ctx, nil, "environments", "create", strings.ToUpper(slug[:1])+slug[1:], slug, "-p", d.project, "--json"); err != nil {
					return err
				}
			}
			s.change("doppler: environment %s", slug)
		}
		for _, svc := range s.services() {
			cfg := platform.DopplerConfig(env, svc)
			if configs[cfg] || cfg == slug {
				continue
			}
			if s.apply {
				if _, err := d.run(ctx, nil, "configs", "create", cfg, "-e", slug, "-p", d.project, "--json"); err != nil {
					return err
				}
			}
			s.change("doppler: config %s (branch of %s: inherits it, which cannot read it)", cfg, slug)
		}
	}
	return nil
}

// dopplerLocal gives the dev config an AUTH_SECRET and points this checkout
// at it (doppler setup), for USE_DOPPLER=1 make dev.
func (s *setup) dopplerLocal(ctx context.Context, d *dopplerCLI) error {
	have, err := d.secretNames(ctx, platform.DopplerLocal)
	if err != nil {
		return err
	}
	if !have["AUTH_SECRET"] {
		v, err := randomSecret()
		if err != nil {
			return err
		}
		if err := d.setSecret(ctx, platform.DopplerLocal, "AUTH_SECRET", v); err != nil {
			return err
		}
		s.change("doppler: %s AUTH_SECRET generated (local dev)", platform.DopplerLocal)
	}
	cmd := exec.CommandContext(ctx, d.bin, "setup", "-p", d.project, "-c", platform.DopplerLocal, "--no-interactive")
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("doppler setup: %w%s", err, tail(string(out)))
	}
	return nil
}

type switchedService struct{ env, svc, before string } // before: latest deployment id at the switch

// dopplerService seeds one service's config and, unless it already reads
// Doppler, seals a fresh read-only token in Railway. It reports whether the
// service was switched, with its latest deployment id from before.
func (s *setup) dopplerService(ctx context.Context, d *dopplerCLI, env, svc string) (before string, switched bool, err error) {
	if s.project.Environments[env] == "" || s.project.Services[svc] == "" {
		return "", false, nil
	}
	cfg := platform.DopplerConfig(env, svc)
	vars, err := s.variables(ctx, svc, env)
	if err != nil {
		return "", false, err
	}
	have, err := d.secretNames(ctx, cfg)
	if err != nil {
		return "", false, err
	}
	for _, seed := range platform.DopplerSeeds(svc) {
		if have[seed.Name] {
			continue
		}
		cur := vars[seed.Name]
		var value string
		from := "Railway"
		switch {
		case cur != nil && *cur != "" && *cur != platform.ResendPlaceholder:
			value = *cur
		case seed.Source == platform.RailwayOrRandom:
			if value, err = randomSecret(); err != nil {
				return "", false, err
			}
			from = "generated"
		case seed.Source == platform.RailwayOrResend:
			value, from = platform.ResendPlaceholder, "placeholder"
		default:
			return "", false, fmt.Errorf("%s/%s: %s is not readable on Railway (sealed or missing): set it in Doppler config %s, then re-run", env, svc, seed.Name, cfg)
		}
		if err := d.setSecret(ctx, cfg, seed.Name, value); err != nil {
			return "", false, err
		}
		s.change("doppler: %s %s (%s)", cfg, seed.Name, from)
	}
	if s.usesDoppler(vars) {
		return "", false, nil // already sealed and switched by an earlier run
	}
	name := platform.DopplerTokenName(s.project.Name, svc)
	old, err := d.tokens(ctx, cfg)
	if err != nil {
		return "", false, err
	}
	for _, t := range old { // a token from a run that never reached Railway
		if t.Name == name {
			if err := d.revoke(ctx, cfg, t.Slug); err != nil {
				return "", false, err
			}
		}
	}
	if before, _, err = s.latestDeployment(ctx, svc, env); err != nil {
		return "", false, err
	}
	tok, err := d.createToken(ctx, cfg, name)
	if err != nil {
		return "", false, err
	}
	err = s.commit(ctx, env, map[string]map[string]secretWrite{svc: {
		"DOPPLER_TOKEN":    {tok, true},
		"SECRETS_PROVIDER": {platform.ProviderDoppler, false},
	}})
	if err != nil {
		return "", false, err
	}
	s.change("%s/%s reads Doppler config %s: DOPPLER_TOKEN sealed, SECRETS_PROVIDER=doppler", env, svc, cfg)
	return before, true, nil
}

// dopplerFinish waits for the switched services' new deployments and, for
// every service healthy on Doppler, removes its Railway copies of the app
// secrets (staged with skipDeploys: the running deployment keeps working).
func (s *setup) dopplerFinish(ctx context.Context, switched []switchedService, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var failed []string
	for _, sw := range switched {
		s.say("Waiting for %s/%s to redeploy on Doppler…", sw.env, sw.svc)
		id, status, err := s.latestDeployment(ctx, sw.svc, sw.env)
		for err == nil && (id == sw.before || !settled(status)) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
			}
			id, status, err = s.latestDeployment(ctx, sw.svc, sw.env)
		}
		if err != nil || id == sw.before || status != "SUCCESS" {
			failed = append(failed, fmt.Sprintf("%s/%s (%s)", sw.env, sw.svc, strings.ToLower(firstOr(status, "no new deployment"))))
		}
	}
	if len(failed) > 0 {
		s.next("Not healthy on Doppler yet: %s. Check railway logs, fix the Doppler config, re-run make doppler-setup (Railway copies of the app secrets were kept)", strings.Join(failed, ", "))
		_ = s.health(ctx, 0) // fill the summary
		return withCode(exitUnhealthy, "not healthy on Doppler: %s", strings.Join(failed, ", "))
	}
	if err := s.health(ctx, 0); err != nil {
		return err
	}
	for _, env := range s.cfg.Environments {
		for _, svc := range s.services() {
			if s.project.Environments[env] == "" || s.project.Services[svc] == "" {
				continue
			}
			vars, err := s.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			if !s.usesDoppler(vars) {
				continue
			}
			var gone []string
			for _, n := range platform.RailwayCopies(svc) {
				if _, ok := vars[n]; ok {
					gone = append(gone, n)
				}
			}
			if len(gone) == 0 {
				continue
			}
			if err := s.deleteVariables(ctx, env, svc, gone); err != nil {
				return err
			}
			s.change("%s/%s removed Railway copies %s (Doppler holds them)", env, svc, strings.Join(gone, ", "))
		}
	}
	return nil
}

// deleteVariables removes variables without triggering a deploy.
func (s *setup) deleteVariables(ctx context.Context, env, svc string, names []string) error {
	vars := map[string]any{}
	for _, n := range names {
		vars[n] = nil
	}
	return s.rw.gql(ctx, `mutation($env: String!, $patch: EnvironmentConfig) { environmentPatchCommit(environmentId: $env, patch: $patch, skipDeploys: true, commitMessage: "railway-setup: app secrets live in Doppler") }`,
		map[string]any{"env": s.project.Environments[env], "patch": map[string]any{"services": map[string]any{
			s.project.Services[svc]: map[string]any{"variables": vars},
		}}}, nil)
}
