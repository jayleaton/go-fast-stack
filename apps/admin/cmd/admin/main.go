// Command admin is the admin dashboard's production binary (optional; its own
// Railway service).
//
//	admin serve           run the HTTP server (default)
//	admin migrate         migrate the admin database (auth, then audit log) and seed ADMIN_BOOTSTRAP_EMAIL
//	admin invite <email>  email a set-password link to a new or pending admin
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/server"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui"
	"github.com/jayleaton/ai-patterns-go/packages/auth"
	"github.com/jayleaton/ai-patterns-go/packages/database"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo"
	"github.com/jayleaton/ai-patterns-go/packages/secrets"
)

const shutdownTimeout = 15 * time.Second

const usage = "usage: admin [serve | migrate | invite <email>]"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch {
	case cmd == "serve" || cmd == "migrate":
	case cmd == "invite" && len(args) == 2:
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

	switch cmd {
	case "migrate":
		err = migrate(ctx, cfg, log)
	case "invite":
		err = invite(ctx, cfg, log, args[1])
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
// migrate. Then it seeds ADMIN_BOOTSTRAP_EMAIL (idempotent).
func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	adminPool, err := database.Open(ctx, cfg.AdminDatabaseURL.Reveal())
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

	admins, err := newAdminAuth(cfg, adminPool, log)
	if err != nil {
		return err
	}
	in := validate.CreateAdminInvite{Email: cfg.BootstrapEmail}
	if err := in.Validate(); err != nil {
		return fmt.Errorf("ADMIN_BOOTSTRAP_EMAIL: %w", err)
	}
	svc := services.NewAdminService(server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool)), adminrepo.NewAuditLog(adminPool), policy(cfg))
	a, created, err := svc.Bootstrap(ctx, services.SystemActor("bootstrap"), in)
	if err != nil {
		return err
	}
	if created {
		log.Info("bootstrap admin created; send its set-password link with `admin invite`", "email", a.Email)
	}
	return admins.Shutdown(ctx)
}

// invite emails a set-password link (the dev mailer logs it). The operator
// running the CLI acts as services.SystemActor and is audited as "cli".
func invite(ctx context.Context, cfg config.Config, log *slog.Logger, email string) error {
	adminPool, err := database.Open(ctx, cfg.AdminDatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer adminPool.Close()
	admins, err := newAdminAuth(cfg, adminPool, log)
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

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	appPool, err := database.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("app database: %w", err)
	}
	defer appPool.Close()
	adminPool, err := database.Open(ctx, cfg.AdminDatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("admin database: %w", err)
	}
	defer adminPool.Close()

	admins, err := newAdminAuth(cfg, adminPool, log)
	if err != nil {
		return err
	}
	auditLog := adminrepo.NewAuditLog(adminPool)
	p := policy(cfg)
	users := services.NewUserService(server.NewUserDirectory(auth.NewUserAdmin(appPool)), auditLog, p)
	adminSvc := services.NewAdminService(server.NewAdminAccounts(admins, adminrepo.NewAdmins(adminPool)), auditLog, p)
	auditSvc := services.NewAuditService(auditLog, p)
	analytics := services.NewAnalyticsService(adminrepo.NewStats(appPool), p)
	callers := server.NewCallers(admins)
	events := server.NewAuthEvents(services.NewAuthEventService(adminrepo.NewAdmins(adminPool), auditLog), log)

	handler := server.New(server.Deps{
		Config: cfg,
		Log:    log,
		DBs:    map[string]server.Pinger{"app": appPool, "admin": adminPool},
		Auth:   admins,
		// No JSON API on the network: the UI calls the services in-process
		// (and audits sign-ins through NewUIAuth). To let scripts call
		// /api/admin/v1, pass api.NewUsers(users, callers.API) etc. as API;
		// the audited auth endpoints come with it, hidden from browsers.
		AuthEvents: events,
		UI: ui.New(ui.Deps{
			Auth: server.NewUIAuth(admins, events), Users: users, Admins: adminSvc, Audit: auditSvc, Analytics: analytics,
			UserFrom: callers.UI, RequireTwoFactor: cfg.RequireTwoFactor, AppName: cfg.AppName,
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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // streams still open after the timeout
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Requests are done; let invite emails queued by them go out.
	return admins.Shutdown(shutdownCtx)
}

// newAdminAuth is the admin app's own packages/auth instance over the admin
// database: its own secret, cookies (prefix "admin") and 2FA policy. Sign-up
// is off: admins are invited (AdminService.InviteAdmin).
func newAdminAuth(cfg config.Config, adminPool *pgxpool.Pool, log *slog.Logger) (*auth.Auth, error) {
	var mailer auth.Mailer = auth.NewDevMailer(log)
	if !cfg.ResendAPIKey.IsZero() {
		mailer = auth.NewResendMailer(cfg.ResendAPIKey.Reveal(), cfg.FromEmail)
	} else {
		log.Warn("RESEND_API_KEY not set: admin invite links are logged, not sent")
	}
	return auth.New(auth.Config{
		Secret:       cfg.AuthSecret.Reveal(),
		BaseURL:      cfg.PublicURL.String(),
		CookiePrefix: "admin",
		// No Better Auth clients: bind the admin cookies to this host.
		HostOnlyCookies: true,
		DisableSignUp:   true,
		AppName:         cfg.AppName,
		TrustedOrigins:  cfg.TrustedOrigins,
		LoginPath:       config.AppRoutes.Login,
		ClientIPHeader:  cfg.ClientIPHeader,
		TwoFactor:       &auth.TwoFactorConfig{Required: cfg.RequireTwoFactor},
		Logger:          log,
	}, adminPool, mailer)
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
