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
	const defaultWait = 15 * time.Minute // --wait: how long apply waits for deployments to settle
	fs := flag.NewFlagSet("railway-setup", flag.ContinueOnError)
	config := fs.String("config", "platform.yaml", "path to platform.yaml")
	iac := fs.String("iac", ".railway/railway.go", "path to the IaC entry point")
	destruct := fs.Bool("confirm-destructive", false, "let apply delete resources removed from platform.yaml")
	only := fs.String("env", "", "up, rotate-db: only this environment (default: all); backup: the environment (required)")
	yes := fs.Bool("yes", envBool("PLATFORM_YES"), "create the project without asking (PLATFORM_YES=1)")
	jsonOut := fs.Bool("json", envBool("PLATFORM_JSON"), "print a JSON summary on stdout, logs on stderr (PLATFORM_JSON=1)")
	wait := fs.Duration("wait", defaultWait, "apply: how long to wait for deployments to settle")
	ovs := overrides()
	vals := map[string]*string{}
	for _, ov := range ovs {
		vals[ov.flagName] = fs.String(ov.flagName, "", ov.usage+" ("+ov.env+")")
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
	fs.Visit(func(flag *flag.Flag) { set[flag.Name] = true })
	for _, ov := range ovs {
		ov.resolve(*vals[ov.flagName], set[ov.flagName])
	}

	var out io.Writer = os.Stdout
	if *jsonOut {
		out = os.Stderr
	}
	run := &setup{out: out, apply: cmd == "apply", destruct: *destruct, yes: *yes, jsonOut: *jsonOut}
	run.sum.Command = cmd
	run.backupArgs = fs.Args()
	return run.finish(run.main(cmd, *config, *iac, *only, *wait, ovs))
}

func (setup *setup) main(cmd, config, iac, only string, wait time.Duration, ovs []*override) error {
	cfgPath, err := filepath.Abs(config)
	if err != nil {
		return err
	}
	if setup.iacFile, err = filepath.Abs(iac); err != nil {
		return err
	}
	setup.cfgPath, setup.root = cfgPath, filepath.Dir(filepath.Dir(filepath.Dir(setup.iacFile)))
	if changed, err := persist(cfgPath, ovs); err != nil {
		return err
	} else if len(changed) > 0 {
		setup.say("platform.yaml: set %s (commit it)", strings.Join(changed, ", "))
	}
	if setup.cfg, err = platform.LoadResolved(cfgPath); err != nil {
		return err
	}
	if only != "" && !setup.cfg.Has(only) {
		return fmt.Errorf("-env %q is not in platform.yaml environments %v", only, []string(setup.cfg.Environments))
	}
	setup.sum.Repo = setup.cfg.Repo
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	setup.rw = newCLI()
	setup.rw.stdout = setup.out
	setup.rw.extraEnv = []string{"PLATFORM_CONFIG=" + cfgPath}
	if err := setup.rw.checkVersion(ctx); err != nil {
		if cmd == "doctor" {
			setup.report(check{Name: "railway cli", Status: "fail", Detail: err.Error(), Fix: "install or update it: https://docs.railway.com/cli (then railway login)"})
			return withCode(exitPreflight, "preflight failed")
		}
		return err
	}
	each := func(fn func(context.Context, string) error) error {
		for _, env := range setup.cfg.Environments {
			if only == "" || only == env {
				if err := fn(ctx, env); err != nil {
					return err
				}
			}
		}
		return nil
	}

	switch cmd {
	case "doctor":
		return setup.doctor(ctx)
	case "plan", "apply":
		return setup.reconcile(ctx, wait)
	case "secrets":
		setup.apply = true
		if err := setup.findProject(ctx); err != nil {
			return err
		}
		return setup.secrets(ctx)
	case "rotate-db":
		setup.apply = true
		if err := setup.findProject(ctx); err != nil {
			return err
		}
		return each(setup.rotateDatabases)
	case "up":
		if err := setup.findProject(ctx); err != nil {
			return err
		}
		return each(setup.up)
	case "doppler":
		setup.apply = true
		if err := setup.findProject(ctx); err != nil {
			return err
		}
		if err := setup.dopplerPhase(ctx, true, wait); err != nil {
			return err
		}
		return setup.secretSteps(ctx)
	case "verify":
		if err := setup.findProject(ctx); err != nil {
			return err
		}
		return setup.health(ctx, 0)
	case "backup":
		if only == "" {
			return errors.New("backup needs -env (PLATFORM_ENV=production): it reads and may restore that environment")
		}
		if err := setup.findProject(ctx); err != nil {
			return err
		}
		return setup.backupJob(ctx, only, setup.backupArgs)
	}
	return nil
}

func (setup *setup) reconcile(ctx context.Context, wait time.Duration) error {
	setup.say("platform.yaml: project %q, environments %v, admin %v, previews %v, secrets %s, repo %q",
		setup.cfg.Project, []string(setup.cfg.Environments), setup.cfg.Admin, setup.cfg.Previews, setup.cfg.Secrets.Provider, setup.cfg.Repo)
	if err := setup.requireSender(); err != nil {
		return err
	}
	if err := setup.ensureProject(ctx); err != nil {
		return err
	}
	// GitHub until proven otherwise only when Railway says it can read the
	// repo or a previous run connected it; else upload first (below).
	trying := false
	if !setup.cfg.Upload() {
		known, has := setup.repoAccess(ctx)
		if (!known || !has) && !setup.repoConnected(ctx) {
			setup.useUpload(true)
			trying = !known // unknown: connect it after the services exist
			if known {
				setup.say("GitHub: Railway cannot read %s yet; deploying by upload", setup.cfg.Repo)
			}
		}
	}
	setup.checkBranches(ctx)
	if err := setup.ensureEnvironments(ctx); err != nil {
		return err
	}
	if err := setup.ensurePreviews(ctx); err != nil {
		return err
	}
	for _, env := range setup.cfg.Environments {
		setup.say("\n== %s", env)
		if err := setup.reconcileEnv(ctx, env); err != nil {
			return err
		}
	}
	if !setup.apply {
		if trying {
			setup.say("\nGitHub: %s is not connected; apply connects it, or deploys by upload while Railway cannot read it.", setup.cfg.Repo)
		}
		if err := setup.dopplerPhase(ctx, false, 0); err != nil {
			return err
		}
		setup.sum.Pending = setup.pending
		if setup.pending > 0 {
			setup.say("\nChanges pending in %d step(s) above. Apply with: make railway-apply", setup.pending)
			return withCode(exitPending, "%d change(s) pending", setup.pending)
		}
		setup.say("\nUp to date.")
		return nil
	}

	if trying {
		if err := setup.connectRepo(ctx); err != nil {
			setup.say("\nGitHub: Railway cannot read %s yet (%v); deploying this checkout by upload instead.", setup.cfg.Repo, err)
		} else {
			setup.say("\nGitHub: connected %s; switching every service to deploy-on-push", setup.cfg.Repo)
			setup.useUpload(false)
			for _, env := range setup.cfg.Environments {
				if err := setup.iac(ctx, env); err != nil {
					return err
				}
			}
		}
	}
	setup.sum.Source = "github"
	if setup.cfg.Upload() {
		setup.sum.Source = "upload"
		for _, env := range setup.cfg.Environments {
			if err := setup.upload(ctx, env, true); err != nil {
				setup.say("  ! %v", err) // reported again by the health check below
			}
		}
		if setup.cfg.Repo != "" {
			setup.next("Install Railway's GitHub app on %s: %s (pick the repo there), then re-run make railway-apply to switch to deploy-on-push",
				setup.cfg.Repo, installURL(ctx, setup.cfg.Repo))
		} else {
			setup.next(`repo: "" never deploys on push: deploy new code with make railway-up`)
		}
	}
	setup.say("\nWaiting for deployments (up to %s)…", wait)
	if err := setup.health(ctx, wait); err != nil {
		_ = setup.secretSteps(ctx)
		return err
	}
	if err := setup.dopplerPhase(ctx, false, wait); err != nil {
		return err
	}
	return setup.secretSteps(ctx)
}

func (setup *setup) reconcileEnv(ctx context.Context, env string) error {
	for _, name := range setup.cfg.HandSet(env) {
		setup.say("left as it is (manual in platform.yaml per_environment.%s): %s", env, name)
	}
	if err := setup.iac(ctx, env); err != nil {
		return err
	}
	if setup.apply {
		if err := setup.findProject(ctx); err != nil { // services created by the apply
			return err
		}
	}
	if err := setup.clearWatchPaths(ctx, env); err != nil {
		return err
	}
	if err := setup.repairDatabases(ctx, env); err != nil {
		return err
	}
	if err := setup.volumeSnapshots(ctx, env); err != nil {
		return err
	}
	if err := setup.ensureDomains(ctx, env); err != nil {
		return err
	}
	return setup.bootstrap(ctx, env)
}

// useUpload switches the graph (here and in the CLI's evaluation of
// railway.go) between upload and GitHub source.
func (setup *setup) useUpload(on bool) {
	setup.cfg.ForceUpload = on
	env := slices.DeleteFunc(setup.rw.extraEnv, func(email string) bool { return strings.HasPrefix(email, "PLATFORM_SOURCE=") })
	if on {
		env = append(env, "PLATFORM_SOURCE=upload")
	}
	setup.rw.extraEnv = env
}

// secretSteps lists the placeholders a person still has to replace.
func (setup *setup) secretSteps(ctx context.Context) error {
	var resend, doppler []string
	for _, env := range setup.cfg.Environments {
		for _, svc := range setup.services() {
			if setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
				continue
			}
			vars, err := setup.variables(ctx, svc, env)
			if err != nil {
				return err
			}
			if value := vars["RESEND_API_KEY"]; value != nil && *value == platform.ResendPlaceholder && !setup.onDoppler(vars) {
				resend = append(resend, env+"/"+svc)
			}
			switch setup.cfg.DopplerMode(env) {
			case platform.SecretsSwitched:
				if !setup.onDoppler(vars) {
					doppler = append(doppler, env+"/"+svc)
				}
			case platform.SecretsStaged:
				if !realToken(vars) {
					doppler = append(doppler, env+"/"+svc)
				}
			}
		}
	}
	if len(doppler) > 0 && !setup.hasNext("make doppler-setup") {
		setup.next("Move secrets to Doppler (live rotation): doppler login, then make doppler-setup (%s)", strings.Join(doppler, ", "))
	}
	if len(resend) > 0 {
		setup.next("Set a real RESEND_API_KEY (sign-up and reset emails fail until then): RESEND_API_KEY_<ENV>=re_... make railway-secrets (%s)", strings.Join(resend, ", "))
	}
	if cfgs := setup.dopplerPlaceholders(ctx); len(cfgs) > 0 {
		setup.next("Set a real RESEND_API_KEY in Doppler (sign-up and reset emails fail until then): doppler secrets set RESEND_API_KEY -p %s -c <config> for %s", setup.cfg.Secrets.Project, strings.Join(cfgs, ", "))
	}
	return nil
}

func (setup *setup) hasNext(substr string) bool {
	return slices.ContainsFunc(setup.sum.NextSteps, func(name string) bool { return strings.Contains(name, substr) })
}

// dopplerPlaceholders lists the root configs whose RESEND_API_KEY is still
// the placeholder (values read into memory only, compared, dropped).
func (setup *setup) dopplerPlaceholders(ctx context.Context) []string {
	if setup.cfg.Secrets.Provider != platform.ProviderDoppler {
		return nil
	}
	cli, err := newDoppler(setup.cfg.Secrets.Project)
	if err != nil {
		return nil
	}
	var out []string
	for _, env := range setup.cfg.Environments {
		cfg := platform.DopplerConfig(env, platform.ServiceWeb)
		data, err := cli.run(ctx, nil, "secrets", "get", "RESEND_API_KEY", "-p", cli.project, "-c", cfg, "--plain")
		if err == nil && strings.TrimSpace(string(data)) == platform.ResendPlaceholder {
			out = append(out, cfg)
		}
	}
	return out
}

// senderFix is how to give the apps a real sender.
const senderFix = "set email.from in deploy/platform.yaml to a sender on your verified email domain " +
	"(or pass --email-from / PLATFORM_EMAIL_FROM; per environment: per_environment.<env>.from_email)"

// requireSender refuses plan and apply before anything changes when an
// environment's apps would get an unset or example FROM_EMAIL: they run
// with APP_ENV=production, which refuses to boot with one, so the deploy
// would end in a crash loop. apply on a terminal without --yes asks for one
// and writes it into platform.yaml.
func (setup *setup) requireSender() error {
	problems := setup.cfg.SenderProblems()
	if len(problems) == 0 {
		return nil
	}
	if setup.apply && !setup.yes && stty(context.Background(), "-g") == nil {
		fmt.Fprint(os.Stderr, "Sender address for auth emails, on your verified email domain (e.g. noreply@yourdomain.com): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		sender := strings.TrimSpace(line)
		if platform.RealSender(sender) {
			changed, err := persist(setup.cfgPath, []*override{{key: "email.from", value: &sender}})
			if err != nil {
				return err
			}
			setup.say("platform.yaml: set %s (commit it)", strings.Join(changed, ", "))
			if setup.cfg, err = platform.LoadResolved(setup.cfgPath); err != nil {
				return err
			}
			if problems = setup.cfg.SenderProblems(); len(problems) == 0 {
				return nil
			}
		}
	}
	setup.next("Set a real sender for auth emails: %s", senderFix)
	return fmt.Errorf("refusing to deploy apps that would not boot: production needs a real FROM_EMAIL (%s): %s", strings.Join(problems, "; "), senderFix)
}

// confirm asks on a terminal; without one it needs --yes.
func (setup *setup) confirm(question string) error {
	if setup.yes {
		return nil
	}
	if stty(context.Background(), "-g") != nil {
		return withCode(exitConfirm, "%s Re-run with --yes (or PLATFORM_YES=1) to confirm", question)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if address := strings.ToLower(strings.TrimSpace(line)); address == "y" || address == "yes" {
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
