// Command backup takes and restores encrypted Postgres backups (packages/backup).
// It runs as the nightly Railway cron service `backup` (deploy/platform.yaml
// backups:) and locally through make db-backup / db-restore.
//
//	backup serve                     run on BACKUP_SCHEDULE forever (the Railway service's command)
//	backup run                       back up the app database (and ADMIN_DATABASE_URL when set), then prune
//	backup list [app|admin]          list backup ids, newest first
//	backup restore <id> --yes [--force] [--into app|admin]
//	                                 restore into RESTORE_DATABASE_URL, or with --into into this job's own
//	                                 DATABASE_URL / ADMIN_DATABASE_URL; --force replaces a database that has tables
//	backup prune                     delete backups outside BACKUP_KEEP_DAILY / BACKUP_KEEP_WEEKLY
//
// On Railway, people run the commands inside the service (make railway-backups
// / railway-restore use `railway ssh`), so the job uses the private network and
// its own variables; no database is exposed and no secret leaves Railway.
// The image ships pg_dump/pg_restore matching the server (postgres:18-alpine).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/backup/internal/config"
	"github.com/jayleaton/go-fast-stack/packages/backup"
	"github.com/jayleaton/go-fast-stack/packages/secrets"
	"github.com/jayleaton/go-fast-stack/packages/secrets/reveal"
)

const usage = `usage: backup serve | run | list [app|admin] | restore <id> --yes [--force] [--into app|admin] | prune`

const (
	minArgs   = 2 // the program name and a command
	exitUsage = 2 // exit status for a usage error
)

// `backup list` table layout (text/tabwriter).
const (
	listTabWidth = 4
	listPadding  = 2
)

func main() {
	if len(os.Args) < minArgs {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	cmd, args := os.Args[1], os.Args[2:]

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out := os.Stdout
	if cmd == "list" {
		out = os.Stderr // stdout is the listing
	}
	log := slog.New(secrets.NewRedactingHandler(slog.NewTextHandler(out, nil), secrets.RedactOptions{}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cmd, args, cfg, log); err != nil {
		log.Error("backup "+cmd+" failed", "err", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string, cfg config.Config, log *slog.Logger) error {
	switch cmd {
	case "serve":
		return serve(ctx, cfg, log)
	case "run", "list", "restore", "prune":
	default:
		return errors.New(usage)
	}
	vals, err := cfg.Secrets.Load(ctx)
	if err != nil {
		return err
	}
	svc := newService(cfg, vals, log)
	dbs := map[string]secrets.Secret{"app": vals.DatabaseURL}
	if !vals.AdminDatabaseURL.IsZero() {
		dbs["admin"] = vals.AdminDatabaseURL
	}

	switch cmd {
	case "run":
		var errs []error
		for _, db := range []string{"app", "admin"} {
			url, ok := dbs[db]
			if !ok {
				continue
			}
			if _, err := svc.Backup(ctx, db, reveal.Value(url)); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err // prune only after every backup succeeded
		}
		for db := range dbs {
			if _, err := svc.Prune(ctx, db); err != nil {
				return err
			}
		}
		return nil
	case "prune":
		for db := range dbs {
			if _, err := svc.Prune(ctx, db); err != nil {
				return err
			}
		}
		return nil
	case "list":
		db := ""
		if len(args) > 0 {
			db = args[0]
		}
		objs, err := svc.List(ctx, db)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, listTabWidth, listPadding, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tBYTES")
		for _, o := range objs {
			_, _ = fmt.Fprintf(w, "%s\t%d\n", o.Key, o.Size)
		}
		return w.Flush()
	default: // restore
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "confirm: the restore replaces data in RESTORE_DATABASE_URL")
		force := fs.Bool("force", false, "restore over a database that already has tables")
		into := fs.String("into", "", "restore into this job's own database: app (DATABASE_URL) or admin (ADMIN_DATABASE_URL)")
		id := ""
		if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
			id, args = args[0], args[1:]
		}
		if err := fs.Parse(args); err != nil {
			return err
		}
		if id == "" && fs.NArg() > 0 {
			id = fs.Arg(0)
		}
		if id == "" {
			return errors.New("restore needs a backup id (backup list)")
		}
		target := cfg.RestoreURL
		switch {
		case *into != "" && !cfg.RestoreURL.IsZero():
			return errors.New("use --into or RESTORE_DATABASE_URL, not both")
		case *into != "":
			// The databases share auth's tables: an admin backup would load
			// into the app database (and replace its users with --force).
			if db, _, err := backup.ParseKey(id); err == nil && db != *into {
				return fmt.Errorf("%s is a backup of the %s database: refusing to restore it --into %s", id, db, *into)
			}
			u, ok := dbs[*into]
			if !ok {
				return fmt.Errorf("--into %q: this job has no %s database (app, or admin when ADMIN_DATABASE_URL is set)", *into, *into)
			}
			target = u
		case target.IsZero():
			return errors.New("name the target: --into app|admin, or RESTORE_DATABASE_URL (never defaulted)")
		}
		log.Info("backup: restoring", "id", id, "into", or(*into, "RESTORE_DATABASE_URL"), "target", target.Fingerprint())
		return svc.Restore(ctx, id, reveal.Value(target), backup.RestoreOptions{Yes: *yes, Force: *force})
	}
}

// serve runs a backup and prune at every BACKUP_SCHEDULE time until ctx
// ends. Each run reads the secrets afresh, so rotated credentials apply to
// the next run without a redeploy. A failed run is logged and retried at the
// next scheduled time, never in a loop.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	for {
		next := cfg.Schedule.Next(time.Now())
		if next.IsZero() {
			return fmt.Errorf("BACKUP_SCHEDULE %q never matches", cfg.Schedule)
		}
		log.Info("backup: next run", "at", next.Format(time.RFC3339), "schedule", cfg.Schedule.String())
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if err := run(ctx, "run", nil, cfg, log); err != nil {
			log.Error("backup: scheduled run failed; retrying at the next scheduled time", "err", err)
		}
	}
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func newService(cfg config.Config, v config.Values, log *slog.Logger) *backup.Service {
	var store backup.Store
	if cfg.Store.Dir != "" {
		store = backup.Dir(cfg.Store.Dir)
	} else {
		store = &backup.S3{
			Endpoint: cfg.Store.Endpoint, Bucket: cfg.Store.Bucket, Region: cfg.Store.Region, PathStyle: cfg.Store.PathStyle,
			AccessKeyID: reveal.Value(v.AccessKeyID), SecretAccessKey: reveal.Value(v.SecretAccessKey),
		}
	}
	current, previous, _ := reveal.Keyring(v.Encryption)
	return &backup.Service{
		Store: store, Postgres: backup.PgTools{}, Retention: cfg.Retention, Logger: log,
		Keys: backup.Keys{Current: current, Previous: previous},
	}
}
