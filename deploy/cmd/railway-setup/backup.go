package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// backupJob runs `backup list | run | restore <id> --yes [--force]` inside
// env's backup service with `railway ssh` (needs a registered SSH key, like
// rotate-db). The job uses its own variables and Railway's private network:
// no database is exposed, no secret leaves Railway, nothing is printed but
// the job's own redacted output. restore defaults to --into the database the
// backup came from (app or admin) in that environment.
func (setup *setup) backupJob(ctx context.Context, env string, args []string) error {
	if !setup.cfg.Backups.Enabled {
		return errors.New("backups are off in platform.yaml (backups.enabled: false)")
	}
	if len(args) == 0 || !slices.Contains([]string{"list", "run", "restore", "prune"}, args[0]) {
		return errors.New("usage: railway-setup backup -env <env> list | run | prune | restore <id> --yes [--force] [--into app|admin]")
	}
	if setup.project == nil || setup.project.Environments[env] == "" || setup.project.Services[platform.ServiceBackup] == "" {
		return fmt.Errorf("%s has no backup service yet: run make railway-apply first", env)
	}
	if args[0] == "restore" && !slices.ContainsFunc(args, func(address string) bool { return strings.HasPrefix(address, "--into") }) {
		// The database a backup belongs to is the first part of its id
		// (app/..., admin/...); the job refuses a mismatched --into.
		index := slices.IndexFunc(args[1:], func(address string) bool { return !strings.HasPrefix(address, "-") })
		if index < 0 {
			return errors.New("restore needs a backup id (make railway-backups)")
		}
		db, _, ok := strings.Cut(args[1+index], "/")
		if !ok || (db != "app" && db != "admin") {
			return fmt.Errorf("%q is not a backup id (app/<time>.dump.age or admin/...)", args[1+index])
		}
		args = append(args, "--into", db)
		setup.say("restore into %s's %s database", env, db)
	}
	setup.say("== %s/backup: backup %s (railway ssh)", env, strings.Join(args, " "))
	code, err := setup.rw.streamIn(ctx, setup.root, append([]string{"ssh", "--service", platform.ServiceBackup, "--environment", env, "--", "backup"}, args...)...)
	switch {
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("backup %s in %s failed (exit %d); railway ssh needs an SSH key (railway ssh keys)", args[0], env, code)
	}
	return nil
}
