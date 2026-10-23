// Command admin is the admin dashboard's production binary (optional; its own
// Railway service).
//
//	admin serve           run the HTTP server (default)
//	admin migrate         migrate the admin database (auth, then audit log) and seed ADMIN_BOOTSTRAP_EMAIL
//	admin invite <email>  email a set-password link to a new or pending admin
//	admin reset-2fa <email>  remove an admin's 2FA (the last admin lost the authenticator and backup codes)
//
// Each subcommand is this static binary alone: nothing runs a shell, so the
// same commands work in the distroless image (as the pre-deploy command) or
// copied next to a shell (README "Locked out of the admin app").
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/server"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui"
	"github.com/jayleaton/go-fast-stack/packages/auth"
	"github.com/jayleaton/go-fast-stack/packages/database"
	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo"
	"github.com/jayleaton/go-fast-stack/packages/secrets"
	"github.com/jayleaton/go-fast-stack/packages/secrets/reveal"
)

const shutdownTimeout = 15 * time.Second

// emailCommandArgs is the argument count of the subcommands that take one
// email (`invite <email>`, `reset-2fa <email>`): the command and the address.
const emailCommandArgs = 2

const usage = "usage: admin [serve | migrate | invite <email> | reset-2fa <email>]"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch {
	case cmd == "serve" || cmd == "migrate":
	case (cmd == "invite" || cmd == "reset-2fa") && len(args) == emailCommandArgs:
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := newLogger(cfg.Env)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Every command needs secrets (both database URLs, the admin auth keyring).
	if err := cfg.Secrets.Start(ctx); err != nil {
		log.Error(cmd+" failed", "err", err)
		stop()
		os.Exit(1)
	}
	defer cfg.Secrets.Stop()

	switch cmd {
	case "migrate":
		err = migrate(ctx, cfg, log)
	case "invite":
		err = invite(ctx, cfg, log, args[1])
	case "reset-2fa":
		err = resetTwoFactor(ctx, cfg, log, args[1])
	default:
		err = serve(ctx, cfg, log)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		stop()
		os.Exit(1)
	}
}

// migrate runs the admin database's migrations: the admin auth instance's
// tables, then the audit log. The app database is the web service's to
// migrate. Then it seeds ADMIN_BOOTSTRAP_EMAIL and emails its invite once
// (idempotent), so the first admin needs no shell in the image.
func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	url, err := cfg.Secrets.AdminDatabaseURL()
	if err != nil {
		return err
	}
	adminPool, err := database.Open(ctx, reveal.Value(url))
	if err != nil {
		return err
	}
	defer adminPool.Close()
	if err := auth.MigrateTable(ctx, adminPool, auth.MigrationTable); err != nil {
		return err
	}
	if err := adminrepo.Migrate(ctx, adminPool); err != nil {
		return err
	}
	log.Info("admin migrations applied")
	if cfg.BootstrapEmail == "" {
		return nil
	}

	admins, err := newAdminAuth(ctx, cfg, adminPool, nil, log)
	if err != nil {
		return err
	}
	in := validate.CreateAdminInvite{Email: cfg.BootstrapEmail}
	if err := in.Validate(); err != nil {
		return fmt.Errorf("ADMIN_BOOTSTRAP_EMAIL: %w", err)
	}
	svc := services.NewAdminService(server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool)), adminrepo.NewAuditLog(adminPool), policy(cfg))
	actor := services.SystemActor("bootstrap")
	admin, created, err := svc.Bootstrap(ctx, actor, in)
	sent := false
	if err == nil {
		sent, err = svc.InviteBootstrapAdmin(ctx, actor, admin)
	}
	// Let a queued invite email go out before the pre-deploy command exits.
	if serr := admins.Shutdown(ctx); err == nil {
		err = serr
	}
	if err != nil {
		return err
	}
	if created || sent {
		log.Info("bootstrap admin", "email", admin.Email, "created", created, "invite_sent", sent,
			"set_password_page", config.Absolute(cfg.PublicURL, config.AppRoutes.SetPassword))
	}
	return nil
}

// invite emails a set-password link (the dev mailer logs it). The operator
// running the CLI acts as services.SystemActor and is audited as "cli".
func invite(ctx context.Context, cfg config.Config, log *slog.Logger, email string) error {
	url, err := cfg.Secrets.AdminDatabaseURL()
	if err != nil {
		return err
	}
	adminPool, err := database.Open(ctx, reveal.Value(url))
	if err != nil {
		return err
	}
	defer adminPool.Close()
	admins, err := newAdminAuth(ctx, cfg, adminPool, nil, log)
	if err != nil {
		return err
	}
	in := validate.CreateAdminInvite{Email: email}
	if err := in.Validate(); err != nil {
		return err
	}
	svc := services.NewAdminService(server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool)), adminrepo.NewAuditLog(adminPool), policy(cfg))
	a, err := svc.InviteAdmin(ctx, services.SystemActor("cli"), in)
	if err != nil {
		return err
	}
	// Let the queued email go out before exiting.
	if err := admins.Shutdown(ctx); err != nil {
		return err
	}
	log.Info("invite sent", "email", a.Email, "set_password_page", config.Absolute(cfg.PublicURL, config.AppRoutes.SetPassword))
	return nil
}

// resetTwoFactor removes an admin's 2FA (TOTP, backup codes, trusted
// devices) as services.SystemActor("cli"), audited like the Admins page's
// "Reset 2FA": the recovery when no other admin can sign in to do it. It
// needs the target's exact email; the admin enrols again at the next sign-in.
func resetTwoFactor(ctx context.Context, cfg config.Config, log *slog.Logger, email string) error {
	in := validate.ResetAdminTwoFactor{Email: email}
	if err := in.Validate(); err != nil {
		return err
	}
	url, err := cfg.Secrets.AdminDatabaseURL()
	if err != nil {
		return err
	}
	adminPool, err := database.Open(ctx, reveal.Value(url))
	if err != nil {
		return err
	}
	defer adminPool.Close()
	admins, err := newAdminAuth(ctx, cfg, adminPool, nil, log)
	if err != nil {
		return err
	}
	accounts := server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool))
	svc := services.NewAdminService(accounts, adminrepo.NewAuditLog(adminPool), policy(cfg))
	admin, err := svc.ResetTwoFactorByEmail(ctx, services.SystemActor("cli"), in)
	if err != nil {
		return fmt.Errorf("reset-2fa %s: %w", in.Email, err)
	}
	log.Info("two-factor authentication removed; the admin enrols again at the next sign-in", "email", admin.Email)
	return admins.Shutdown(ctx)
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// The admin service migrates neither database at runtime: both must be at
	// least as far as this binary's migrations, before boot and after a rotation.
	appPool, err := livePool(ctx, log, "app", cfg.Secrets.AppDatabaseURL, cfg.Secrets.OnAppDatabaseURL,
		map[string]fs.FS{auth.MigrationTable: auth.Migrations, database.MigrationTable: database.Migrations})
	if err != nil {
		return fmt.Errorf("app database: %w", err)
	}
	defer appPool.Close()
	adminPool, err := livePool(ctx, log, "admin", cfg.Secrets.AdminDatabaseURL, cfg.Secrets.OnAdminDatabaseURL,
		map[string]fs.FS{auth.MigrationTable: auth.Migrations, adminrepo.MigrationTable: adminrepo.Migrations})
	if err != nil {
		return fmt.Errorf("admin database: %w", err)
	}
	defer adminPool.Close()

	auditLog := adminrepo.NewAuditLog(adminPool)
	events := server.NewAuditEvents(services.NewAuthEventService(auditLog), log)
	admins, err := newAdminAuth(ctx, cfg, adminPool, events, log)
	if err != nil {
		return err
	}
	adminPolicy := policy(cfg)
	users := services.NewUserService(server.NewUserDirectory(auth.NewUserAdmin(appPool)), auditLog, adminPolicy)
	adminSvc := services.NewAdminService(server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool)), auditLog, adminPolicy)
	auditSvc := services.NewAuditService(auditLog, adminPolicy)
	analytics := services.NewAnalyticsService(adminrepo.NewStats(appPool), adminPolicy)
	// One gate decides route access for both transports (docs/route-access.md).
	gate := access.NewGate(server.NewCallers(admins).Resolve)
	// The UI reaches data only through the API, in-process: the same
	// endpoints, policies and validation a /api/admin/v1 request would take.
	adminAPI := api.NewClient(api.NewUsers(users, gate), api.NewAdmins(adminSvc, gate), api.NewAudit(auditSvc, gate), api.NewAnalytics(analytics, gate))
	reconciler := services.NewAuditReconciler(auditLog, server.NewUserDirectory(auth.NewUserAdmin(appPool)), server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool)))
	go reconcileAudit(ctx, reconciler, cfg.AuditReconcileAfter, log)

	handler := server.New(server.Deps{
		Config:  cfg,
		Log:     log,
		DBs:     map[string]server.Pinger{"app": appPool, "admin": adminPool},
		Secrets: cfg.Secrets,
		Auth:    admins,
		// No JSON API on the network: the UI calls it in-process through
		// adminAPI (sign-ins are audited through the admin auth instance's
		// events). To let scripts call /api/admin/v1, mount the same
		// api.NewUsers(users, gate) etc. groups; the audited auth endpoints
		// come with it, hidden from browsers.
		UI: ui.New(ui.Deps{
			Auth: server.NewUIAuth(admins), API: adminAPI,
			Gate: gate, RequireTwoFactor: cfg.RequireTwoFactor, AppName: cfg.AppName,
			SecureCookies: cfg.PublicURL.Scheme == "https", Logger: log,
		}).Handler(),
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: Datastar SSE streams stay open.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("admin listening", "addr", cfg.Addr, "public_url", cfg.PublicURL.String(), "env", cfg.Env, "require_2fa", cfg.RequireTwoFactor)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if err != nil {
		_ = srv.Close() // streams still open after the timeout
	} else if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) {
		err = serveErr
	}
	// Whatever happened above, let the invite emails and audit events the
	// requests queued go out, on their own time budget.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelDrain()
	return errors.Join(err, admins.Shutdown(drainCtx))
}

// rotationTimeout bounds one live rotation (connecting a new pool, rewrapping
// auth keys). It runs on the secrets poller, never on a request.
const rotationTimeout = 30 * time.Second

// livePool opens the database named by url and swaps it in place whenever
// the URL rotates; a new URL must pass the migrations probe or the current
// pool stays.
func livePool(ctx context.Context, log *slog.Logger, name string, url func() (secrets.Secret, error), onChange func(secrets.Hook), migrated map[string]fs.FS) (*database.Pool, error) {
	u, err := url()
	if err != nil {
		return nil, err
	}
	pool, err := database.OpenPool(ctx, reveal.Value(u), database.PoolOptions{Probe: database.MigratedProbe(migrated), Logger: log})
	if err != nil {
		return nil, err
	}
	applied := u.Fingerprint()
	log.Info("database: pool opened", "database", name, "url", applied)
	onChange(func(v secrets.Secret) error {
		if v.Fingerprint() == applied {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, rotationTimeout) // shutdown aborts a rotation in progress
		defer cancel()
		if err := pool.Swap(ctx, reveal.Value(v)); err != nil {
			return err // the old pool stays; retried after the next fetch
		}
		log.Info("database: URL rotated", "database", name, "from", applied, "to", v.Fingerprint())
		applied = v.Fingerprint()
		return nil
	})
	return pool, nil
}

// reconcileInterval is how often stale pending audit entries are resolved
// (also once at startup).
const reconcileInterval = 5 * time.Minute

// reconcileAudit resolves pending audit entries (admin changes whose outcome
// was never written) from the current state, at startup and then every
// reconcileInterval, until ctx ends.
func reconcileAudit(ctx context.Context, r *services.AuditReconciler, after time.Duration, log *slog.Logger) {
	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	for {
		res, err := r.Reconcile(ctx, after)
		if err != nil && ctx.Err() == nil {
			log.Error("admin: audit reconcile incomplete; entries stay pending", "err", err, "skipped", res.Skipped)
		}
		if n := res.Succeeded + res.Failed + res.Unknown; n > 0 {
			log.Warn("admin: resolved audit entries whose outcome was never written",
				"succeeded", res.Succeeded, "failed", res.Failed, "unknown", res.Unknown)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// newAdminAuth is the admin app's own packages/auth instance over the admin
// database: its own secret keyring, cookies (prefix "admin") and 2FA policy.
// Sign-up is off: admins are invited (AdminService.InviteAdmin). The keyring
// and RESEND_API_KEY rotate live. events audits its sign-ins, sign-outs and
// failed 2FA codes (server.NewAuditEvents); nil for the CLI subcommands,
// which sign no one in.
func newAdminAuth(ctx context.Context, cfg config.Config, adminPool auth.DB, events auth.EventHandler, log *slog.Logger) (*auth.Auth, error) {
	dev := auth.NewDevMailer(log)
	mailer := auth.NewSwapMailer(dev)
	cfg.Secrets.OnResendAPIKey(func(key secrets.Secret) error {
		if key.IsZero() {
			mailer.Swap(dev)
			log.Warn("RESEND_API_KEY not set: admin invite links are logged, not sent")
			return nil
		}
		mailer.Swap(auth.NewResendMailer(reveal.Value(key), cfg.FromEmail))
		log.Info("mailer: resend client built", "resend_api_key", key.Fingerprint())
		return nil
	})
	keys, err := cfg.Secrets.AuthSecrets()
	if err != nil {
		return nil, err
	}
	current, previous, revoked := reveal.Keyring(keys)
	admins, err := auth.New(auth.Config{
		Secret:          current,
		PreviousSecrets: previous,
		RevokedSecrets:  revoked,
		SecretGrace:     cfg.AuthSecretGrace,
		BaseURL:         cfg.PublicURL.String(),
		CookiePrefix:    "admin",
		// No Better Auth clients: bind the admin cookies to this host.
		HostOnlyCookies: true,
		DisableSignUp:   true,
		AppName:         cfg.AppName,
		TrustedOrigins:  cfg.TrustedOrigins,
		LoginPath:       config.AppRoutes.Login,
		ClientIPHeader:  cfg.ClientIPHeader,
		TwoFactor:       &auth.TwoFactorConfig{Required: cfg.RequireTwoFactor},
		Events:          events,
		Logger:          log,
	}, adminPool, mailer)
	if err != nil {
		return nil, err
	}
	applied := keys.Fingerprint()
	cfg.Secrets.OnAuthSecrets(func(k secrets.Keyring) error {
		if k.Fingerprint() == applied {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, rotationTimeout)
		defer cancel()
		current, previous, revoked := reveal.Keyring(k)
		if err := admins.SetSecrets(ctx, auth.Secrets{Current: current, Previous: previous, Revoked: revoked}); err != nil {
			return err // the old keyring stays; retried after the next fetch
		}
		log.Info("auth: ADMIN_AUTH_SECRET rotated", "from", applied, "to", k.Fingerprint())
		applied = k.Fingerprint()
		return nil
	})
	return admins, nil
}

func policy(cfg config.Config) services.Policy {
	return services.Policy{RequireTwoFactor: cfg.RequireTwoFactor}
}

// newLogger redacts credentials from every record (secrets.NewRedactingHandler);
// outside production it keeps the dev mailer's links readable.
func newLogger(env config.Env) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	redact := secrets.RedactOptions{KeepDevMailLinks: env != config.Production}
	if env == config.Production {
		return slog.New(secrets.NewRedactingHandler(slog.NewJSONHandler(os.Stdout, opts), redact))
	}
	return slog.New(secrets.NewRedactingHandler(slog.NewTextHandler(os.Stdout, opts), redact))
}
