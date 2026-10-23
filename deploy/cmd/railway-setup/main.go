// Command railway-setup takes a checkout of this template from nothing to a
// live Railway project, and keeps the project matching deploy/platform.yaml.
//
//	railway-setup doctor    check login, workspace, SSH key, GitHub, Doppler; print fixes
//	railway-setup plan      show what would change (read-only)
//	railway-setup apply     create or update everything, deploy, wait until healthy
//	railway-setup doppler   move app secrets into Doppler (also a phase of apply)
//	railway-setup secrets   add Doppler tokens / RESEND_API_KEY (sealed)
//	railway-setup up        upload this checkout and deploy it (repo: "")
//	railway-setup rotate-db new Postgres superuser passwords (after an incident)
//	railway-setup backup -env <env> list | run | restore <id> --yes [--force]
//	                        the backup job against env's bucket and databases
//	railway-setup verify    GET /healthz on every public service
//
// Flags (or PLATFORM_* env vars) set platform.yaml keys first: --project,
// --workspace, --environments, --admin, --region, --repo. --yes creates the
// project without asking; --json prints a machine-readable summary on
// stdout (logs go to stderr). Exit codes: summary.go. Run it through make
// (railway-doctor, railway-setup, railway-plan, ...). It needs a logged-in
// Railway CLI and Go; it reads no Railway token of its own.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

var commands = []string{"doctor", "plan", "apply", "doppler", "secrets", "up", "rotate-db", "verify", "backup"}

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("railway-setup", flag.ContinueOnError)
	config := fs.String("config", "platform.yaml", "path to platform.yaml")
	iac := fs.String("iac", ".railway/railway.go", "path to the IaC entry point")
	destruct := fs.Bool("confirm-destructive", false, "let apply delete resources removed from platform.yaml")
	only := fs.String("env", "", "up, rotate-db: only this environment (default: all); backup: the environment (required)")
	yes := fs.Bool("yes", envBool("PLATFORM_YES"), "create the project without asking (PLATFORM_YES=1)")
	jsonOut := fs.Bool("json", envBool("PLATFORM_JSON"), "print a JSON summary on stdout, logs on stderr (PLATFORM_JSON=1)")
	wait := fs.Duration("wait", 15*time.Minute, "apply: how long to wait for deployments to settle")
	ovs := overrides()
	vals := map[string]*string{}
	for _, o := range ovs {
		vals[o.flagName] = fs.String(o.flagName, "", o.usage+" ("+o.env+")")
	}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: railway-setup %s [flags]\n", strings.Join(commands, "|"))
		fs.PrintDefaults()
	}
	if len(os.Args) < 2 || !slices.Contains(commands, os.Args[1]) {
		fs.Usage()
		return exitError
	}
	cmd := os.Args[1]
	if err := fs.Parse(os.Args[2:]); err != nil {
		return exitError
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, o := range ovs {
		o.resolve(*vals[o.flagName], set[o.flagName])
	}

	var out io.Writer = os.Stdout
	if *jsonOut {
		out = os.Stderr
	}
	s := &setup{out: out, apply: cmd == "apply", destruct: *destruct, yes: *yes, jsonOut: *jsonOut}
	s.sum.Command = cmd
	s.backupArgs = fs.Args()
	return s.finish(s.main(cmd, *config, *iac, *only, *wait, ovs))
}

func (s *setup) main(cmd, config, iac, only string, wait time.Duration, ovs []*override) error {
	cfgPath, err := filepath.Abs(config)
	if err != nil {
		return err
	}
	if s.iacFile, err = filepath.Abs(iac); err != nil {
		return err
	}
	s.cfgPath, s.root = cfgPath, filepath.Dir(filepath.Dir(filepath.Dir(s.iacFile)))
	if changed, err := persist(cfgPath, ovs); err != nil {
		return err
	} else if len(changed) > 0 {
		s.say("platform.yaml: set %s (commit it)", strings.Join(changed, ", "))
	}
	if s.cfg, err = platform.LoadResolved(cfgPath); err != nil {
		return err
	}
	if only != "" && !s.cfg.Has(only) {
		return fmt.Errorf("-env %q is not in platform.yaml environments %v", only, []string(s.cfg.Environments))
	}
	s.sum.Repo = s.cfg.Repo
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	s.rw = newCLI()
	s.rw.stdout = s.out
	s.rw.extraEnv = []string{"PLATFORM_CONFIG=" + cfgPath}
	if err := s.rw.checkVersion(ctx); err != nil {
		if cmd == "doctor" {
			s.report(check{Name: "railway cli", Status: "fail", Detail: err.Error(), Fix: "install or update it: https://docs.railway.com/cli (then railway login)"})
			return withCode(exitPreflight, "preflight failed")
		}
		return err
	}
	each := func(f func(context.Context, string) error) error {
		for _, env := range s.cfg.Environments {
			if only == "" || only == env {
				if err := f(ctx, env); err != nil {
					return err
				}
			}
		}
		return nil
	}

	switch cmd {
	case "doctor":
		return s.doctor(ctx)
	case "plan", "apply":
		return s.reconcile(ctx, wait)
	case "secrets":
		s.apply = true
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return s.secrets(ctx)
	case "rotate-db":
		s.apply = true
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return each(s.rotateDatabases)
	case "up":
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return each(s.up)
	case "doppler":
		s.apply = true
		if err := s.findProject(ctx); err != nil {
			return err
		}
		if err := s.dopplerPhase(ctx, true, wait); err != nil {
			return err
		}
		return s.secretSteps(ctx)
	case "verify":
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return s.health(ctx, 0)
	case "backup":
		if only == "" {
			return errors.New("backup needs -env (PLATFORM_ENV=production): it reads and may restore that environment")
		}
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return s.backupJob(ctx, only, s.backupArgs)
	}
	return nil
}

func (s *setup) reconcile(ctx context.Context, wait time.Duration) error {
	s.say("platform.yaml: project %q, environments %v, admin %v, previews %v, secrets %s, repo %q",
		s.cfg.Project, []string(s.cfg.Environments), s.cfg.Admin, s.cfg.Previews, s.cfg.Secrets.Provider, s.cfg.Repo)
	if err := s.ensureProject(ctx); err != nil {
		return err
	}
	// GitHub until proven otherwise only when Railway says it can read the
	// repo or a previous run connected it; else upload first (below).
	trying := false
	if !s.cfg.Upload() {
		known, has := s.repoAccess(ctx)
		if (!known || !has) && !s.repoConnected(ctx) {
			s.useUpload(true)
			trying = !known // unknown: connect it after the services exist
			if known {
				s.say("GitHub: Railway cannot read %s yet; deploying by upload", s.cfg.Repo)
			}
		}
	}
	s.checkBranches(ctx)
	if err := s.ensureEnvironments(ctx); err != nil {
		return err
	}
	if err := s.ensurePreviews(ctx); err != nil {
		return err
	}
	for _, env := range s.cfg.Environments {
		s.say("\n== %s", env)
		if err := s.reconcileEnv(ctx, env); err != nil {
			return err
		}
	}
	if !s.apply {
		if trying {
			s.say("\nGitHub: %s is not connected; apply connects it, or deploys by upload while Railway cannot read it.", s.cfg.Repo)
		}
		if err := s.dopplerPhase(ctx, false, 0); err != nil {
			return err
		}
		s.sum.Pending = s.pending
		if s.pending > 0 {
			s.say("\nChanges pending in %d step(s) above. Apply with: make railway-apply", s.pending)
			return withCode(exitPending, "%d change(s) pending", s.pending)
		}
		s.say("\nUp to date.")
		return nil
	}

	if trying {
		if err := s.connectRepo(ctx); err != nil {
			s.say("\nGitHub: Railway cannot read %s yet (%v); deploying this checkout by upload instead.", s.cfg.Repo, err)
		} else {
			s.say("\nGitHub: connected %s; switching every service to deploy-on-push", s.cfg.Repo)
			s.useUpload(false)
			for _, env := range s.cfg.Environments {
				if err := s.iac(ctx, env); err != nil {
					return err
				}
			}
		}
	}
	s.sum.Source = "github"
	if s.cfg.Upload() {
		s.sum.Source = "upload"
		for _, env := range s.cfg.Environments {
			if err := s.upload(ctx, env, true); err != nil {
				s.say("  ! %v", err) // reported again by the health check below
			}
		}
		if s.cfg.Repo != "" {
			s.next("Install Railway's GitHub app on %s: %s (pick the repo there), then re-run make railway-apply to switch to deploy-on-push",
				s.cfg.Repo, installURL(ctx, s.cfg.Repo))
		} else {
			s.next(`repo: "" never deploys on push: deploy new code with make railway-up`)
		}
	}
	s.say("\nWaiting for deployments (up to %s)…", wait)
	if err := s.health(ctx, wait); err != nil {
		_ = s.secretSteps(ctx)
		return err
	}
	if err := s.dopplerPhase(ctx, false, wait); err != nil {
		return err
	}
	return s.secretSteps(ctx)
}

func (s *setup) reconcileEnv(ctx context.Context, env string) error {
	for _, name := range s.cfg.HandSet(env) {
		s.say("left as it is (manual in platform.yaml per_environment.%s): %s", env, name)
	}
	if err := s.iac(ctx, env); err != nil {
		return err
	}
	if s.apply {
		if err := s.findProject(ctx); err != nil { // services created by the apply
			return err
		}
	}
	if err := s.clearWatchPaths(ctx, env); err != nil {
		return err
	}
	if err := s.repairDatabases(ctx, env); err != nil {
		return err
	}
	if err := s.volumeSnapshots(ctx, env); err != nil {
		return err
	}
	if err := s.ensureDomains(ctx, env); err != nil {
		return err
	}
	return s.bootstrap(ctx, env)
}

// useUpload switches the graph (here and in the CLI's evaluation of
// railway.go) between upload and GitHub source.
func (s *setup) useUpload(on bool) {
	s.cfg.ForceUpload = on
	env := slices.DeleteFunc(s.rw.extraEnv, func(e string) bool { return strings.HasPrefix(e, "PLATFORM_SOURCE=") })
	if on {
		env = append(env, "PLATFORM_SOURCE=upload")
	}
	s.rw.extraEnv = env
}

// secretSteps lists the placeholders a person still has to replace.
func (s *setup) secretSteps(ctx context.Context) error {
	var resend, doppler []string
	for _, env := range s.cfg.Environments {
		for _, svc := range s.services() {
			if s.project.Environments[env] == "" || s.project.Services[svc] == "" {
				continue
			}
			vars, err := s.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			if v := vars["RESEND_API_KEY"]; v != nil && *v == platform.ResendPlaceholder && !s.onDoppler(vars) {
				resend = append(resend, env+"/"+svc)
			}
			switch s.cfg.DopplerMode(env) {
			case platform.SecretsSwitched:
				if !s.onDoppler(vars) {
					doppler = append(doppler, env+"/"+svc)
				}
			case platform.SecretsStaged:
				if !realToken(vars) {
					doppler = append(doppler, env+"/"+svc)
				}
			}
		}
	}
	if len(doppler) > 0 && !s.hasNext("make doppler-setup") {
		s.next("Move secrets to Doppler (live rotation): doppler login, then make doppler-setup (%s)", strings.Join(doppler, ", "))
	}
	if len(resend) > 0 {
		s.next("Set a real RESEND_API_KEY (sign-up and reset emails fail until then): RESEND_API_KEY_<ENV>=re_... make railway-secrets (%s)", strings.Join(resend, ", "))
	}
	if cfgs := s.dopplerPlaceholders(ctx); len(cfgs) > 0 {
		s.next("Set a real RESEND_API_KEY in Doppler (sign-up and reset emails fail until then): doppler secrets set RESEND_API_KEY -p %s -c <config> for %s", s.cfg.Secrets.Project, strings.Join(cfgs, ", "))
	}
	return nil
}

func (s *setup) hasNext(substr string) bool {
	return slices.ContainsFunc(s.sum.NextSteps, func(n string) bool { return strings.Contains(n, substr) })
}

// dopplerPlaceholders lists the root configs whose RESEND_API_KEY is still
// the placeholder (values read into memory only, compared, dropped).
func (s *setup) dopplerPlaceholders(ctx context.Context) []string {
	if s.cfg.Secrets.Provider != platform.ProviderDoppler {
		return nil
	}
	d, err := newDoppler(s.cfg.Secrets.Project)
	if err != nil {
		return nil
	}
	var out []string
	for _, env := range s.cfg.Environments {
		cfg := platform.DopplerConfig(env, platform.ServiceWeb)
		b, err := d.run(ctx, nil, "secrets", "get", "RESEND_API_KEY", "-p", d.project, "-c", cfg, "--plain")
		if err == nil && strings.TrimSpace(string(b)) == platform.ResendPlaceholder {
			out = append(out, cfg)
		}
	}
	return out
}

// confirm asks on a terminal; without one it needs --yes.
func (s *setup) confirm(question string) error {
	if s.yes {
		return nil
	}
	if stty(context.Background(), "-g") != nil {
		return withCode(exitConfirm, "%s Re-run with --yes (or PLATFORM_YES=1) to confirm", question)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return withCode(exitConfirm, "not confirmed")
}

func envBool(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
