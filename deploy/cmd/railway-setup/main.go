// Command railway-setup makes a Railway project match deploy/platform.yaml.
//
//	railway-setup plan      show what would change (read-only)
//	railway-setup apply     create or update the project, every environment
//	railway-setup secrets   add Doppler tokens / RESEND_API_KEY (sealed)
//	railway-setup up        upload this checkout and deploy it (repo: "")
//	railway-setup verify    GET /healthz on every public service
//
// Run it through make (railway-plan, railway-apply, railway-secrets,
// railway-verify). It needs a logged-in Railway CLI and Go; it reads no
// Railway token of its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/jayleaton/ai-patterns-go/deploy/platform"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "railway-setup:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("railway-setup", flag.ExitOnError)
	config := fs.String("config", "platform.yaml", "path to platform.yaml")
	iac := fs.String("iac", ".railway/railway.go", "path to the IaC entry point")
	destruct := fs.Bool("confirm-destructive", false, "let apply delete resources removed from platform.yaml")
	only := fs.String("env", "", "up: deploy only this environment (default: all)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: railway-setup plan|apply|secrets|up|verify [flags]")
		fs.PrintDefaults()
	}
	if len(os.Args) < 2 {
		fs.Usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	_ = fs.Parse(os.Args[2:])

	cfgPath, err := filepath.Abs(*config)
	if err != nil {
		return err
	}
	iacFile, err := filepath.Abs(*iac)
	if err != nil {
		return err
	}
	cfg, err := platform.Load(cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rw := newCLI()
	rw.extraEnv = []string{"PLATFORM_CONFIG=" + cfgPath}
	if err := rw.checkVersion(ctx); err != nil {
		return err
	}
	s := &setup{cfg: cfg, cfgPath: cfgPath, iacFile: iacFile, rw: rw, out: os.Stdout,
		root: filepath.Dir(filepath.Dir(filepath.Dir(iacFile))), apply: cmd == "apply", destruct: *destruct}

	switch cmd {
	case "plan", "apply":
		return s.reconcile(ctx)
	case "secrets":
		s.apply = true
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return s.secrets(ctx)
	case "up":
		if *only != "" && !cfg.Has(*only) {
			return fmt.Errorf("-env %q is not in platform.yaml environments %v", *only, []string(cfg.Environments))
		}
		if err := s.findProject(ctx); err != nil {
			return err
		}
		for _, env := range cfg.Environments {
			if *only == "" || *only == env {
				if err := s.up(ctx, env); err != nil {
					return err
				}
			}
		}
		return nil
	case "verify":
		if err := s.findProject(ctx); err != nil {
			return err
		}
		return s.verify(ctx)
	default:
		fs.Usage()
		os.Exit(2)
	}
	return nil
}

func (s *setup) reconcile(ctx context.Context) error {
	s.say("platform.yaml: project %q, environments %v, admin %v, previews %v, secrets %s",
		s.cfg.Project, []string(s.cfg.Environments), s.cfg.Admin, s.cfg.Previews, s.cfg.Secrets.Provider)
	if err := s.ensureProject(ctx); err != nil {
		return err
	}
	if err := s.ensureEnvironments(ctx); err != nil {
		return err
	}
	if err := s.ensurePreviews(ctx); err != nil {
		return err
	}
	for _, env := range s.cfg.Environments {
		s.say("\n== %s", env)
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
		if err := s.ensureDomains(ctx, env); err != nil {
			return err
		}
		if err := s.bootstrap(ctx, env); err != nil {
			return err
		}
	}
	switch {
	case s.apply && s.cfg.Upload():
		s.say("\nApplied. Upload mode (repo is empty): deploy the code with make railway-up, then make railway-verify")
	case s.apply:
		s.say("\nApplied. Railway is building and deploying from %s; check with: make railway-verify", s.cfg.Repo)
	case s.pending > 0:
		s.say("\nChanges pending in %d step(s) above. Apply with: make railway-apply", s.pending)
	default:
		s.say("\nUp to date.")
	}
	return nil
}
