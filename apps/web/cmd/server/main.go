// Command server is the single production binary.
//
//	server serve    run the HTTP server (default)
//	server migrate  run auth migrations, then app migrations
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
	"slices"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/api"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/server"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/web/ui"
	"github.com/jayleaton/go-fast-stack/packages/auth"
	"github.com/jayleaton/go-fast-stack/packages/database"
	"github.com/jayleaton/go-fast-stack/packages/secrets"
	"github.com/jayleaton/go-fast-stack/packages/secrets/reveal"
)

// migrations run in order: auth owns "user", app tables reference it.
var migrations = []func(context.Context, *pgxpool.Pool) error{
	auth.Migrate,
	database.Migrate,
}

const shutdownTimeout = 15 * time.Second

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd != "serve" && cmd != "migrate" {
		fmt.Fprintf(os.Stderr, "usage: server [serve|migrate]\n")
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

	if cmd == "migrate" {
		err = migrate(ctx, cfg, log)
	} else {
		err = serve(ctx, cfg, log)
	}
	if err != nil {
		log.Error(cmd+" failed", "err", err)
		stop()
		os.Exit(1)
	}
}

// migrate runs at deploy time (Railway pre-deploy), never on rotation.
func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := cfg.Secrets.Start(ctx); err != nil { // DATABASE_URL may come from the provider
		return err
	}
	defer cfg.Secrets.Stop()
	url, err := cfg.Secrets.DatabaseURL()
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, reveal.Value(url))
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, m := range migrations {
		if err := m(ctx, pool); err != nil {
			return err
		}
	}
	log.Info("migrations applied")
	return nil
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// First: no valid secrets snapshot, no server (the deploy fails visibly).
	if err := cfg.Secrets.Start(ctx); err != nil {
		return err
	}
	defer cfg.Secrets.Stop()

	pool, err := newPool(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	authn, err := newAuth(ctx, cfg, pool, log)
	if err != nil {
		return err
	}

	users := services.NewUserService(database.NewUserRepository(pool), server.NewAccountCredentials(authn))
	// One gate decides route access for both transports (docs/route-access.md).
	gate := access.NewGate(server.Callers)
	usersAPI := api.NewUsers(users, gate)
	socialAPI := api.NewSocial(services.NewSocialSignInService(server.NewSocialAuth(authn)), gate)
	images, err := newImages(ctx, cfg, gate, log)
	if err != nil {
		return err
	}
	handler := server.New(server.Deps{
		Config:  cfg,
		Log:     log,
		DB:      pool,
		Secrets: cfg.Secrets,
		Auth:    authn,
		Users:   usersAPI,
		Social:  socialAPI,
		Images:  images,
		UI: ui.New(ui.Deps{
			API: api.NewClient(usersAPI, api.NewSessions(server.NewAuthenticator(authn), gate)).WithSocial(socialAPI), Gate: gate, Features: cfg.Features, AppName: cfg.AppName,
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
	log.Info("listening", "addr", cfg.Addr, "public_url", cfg.PublicURL.String(), "env", cfg.Env)

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
	// Requests are done; let auth emails queued by them go out.
	return authn.Shutdown(shutdownCtx)
}

// rotationTimeout bounds one live rotation (connecting a new pool, rewrapping
// auth keys). It runs on the secrets poller, never on a request.
const rotationTimeout = 30 * time.Second

// newPool opens the database pool from DATABASE_URL and swaps it in place
// whenever DATABASE_URL rotates. A new URL must reach a database migrated at
// least as far as this binary, or the current pool stays.
func newPool(ctx context.Context, cfg config.Config, log *slog.Logger) (*database.Pool, error) {
	url, err := cfg.Secrets.DatabaseURL()
	if err != nil {
		return nil, err
	}
	pool, err := database.OpenPool(ctx, reveal.Value(url), database.PoolOptions{
		Probe:  database.MigratedProbe(map[string]fs.FS{auth.MigrationTable: auth.Migrations, database.MigrationTable: database.Migrations}),
		Logger: log,
	})
	if err != nil {
		return nil, err
	}
	applied := url.Fingerprint()
	log.Info("database: pool opened", "database_url", applied)
	cfg.Secrets.OnDatabaseURL(func(v secrets.Secret) error {
		if v.Fingerprint() == applied {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, rotationTimeout) // shutdown aborts a rotation in progress
		defer cancel()
		if err := pool.Swap(ctx, reveal.Value(v)); err != nil {
			return err // the old pool stays; retried after the next fetch
		}
		log.Info("database: DATABASE_URL rotated", "from", applied, "to", v.Fingerprint())
		applied = v.Fingerprint()
		return nil
	})
	return pool, nil
}

// newAuth configures packages/auth from config: Resend when RESEND_API_KEY is
// set (required in production), otherwise email links are logged. The key and
// AUTH_SECRET[_PREVIOUS] are live secrets: a rotated key rebuilds the Resend
// client, rotated auth secrets update the keyring (no one is signed out).
func newAuth(ctx context.Context, cfg config.Config, pool *database.Pool, log *slog.Logger) (*auth.Auth, error) {
	dev := auth.NewDevMailer(log)
	mailer := auth.NewSwapMailer(dev)
	cfg.Secrets.OnResendAPIKey(func(key secrets.Secret) error {
		if key.IsZero() {
			mailer.Swap(dev)
			log.Warn("RESEND_API_KEY not set: auth email links are logged, not sent")
			return nil
		}
		mailer.Swap(auth.NewResendMailer(reveal.Value(key), cfg.FromEmail))
		log.Info("mailer: resend client built", "secrets_provider", cfg.Secrets.Provider(), "resend_api_key", key.Fingerprint())
		return nil
	})
	keys, err := cfg.Secrets.AuthSecrets()
	if err != nil {
		return nil, err
	}
	current, previous, revoked := reveal.Keyring(keys)
	google, apple, err := socialProviders(cfg)
	if err != nil {
		return nil, err
	}
	authn, err := auth.New(auth.Config{
		Secret:                   current,
		PreviousSecrets:          previous,
		RevokedSecrets:           revoked,
		SecretGrace:              cfg.AuthSecretGrace,
		BaseURL:                  cfg.PublicURL.String(),
		RequireEmailVerification: cfg.Features.EmailVerification,
		TrustedOrigins:           slices.Concat(cfg.TrustedOrigins, auth.MobileTrustedOrigins(!cfg.Features.MobileDevOrigins)),
		LoginPath:                config.AppRoutes.Login,
		VerifyEmailPath:          config.AppRoutes.VerifyEmailSuccess,
		ClientIPHeader:           cfg.ClientIPHeader,
		Logger:                   log,
		Google:                   google,
		Apple:                    apple,
		SocialErrorPath:          config.AppRoutes.Login,
		SocialCallbackPath:       config.SocialCallbackPrefix,
	}, pool, mailer)
	if err != nil {
		return nil, err
	}
	if google != nil || apple != nil {
		log.Info("auth: social sign-in on", "providers", authn.SocialProviders())
		cfg.Secrets.OnSocialSecrets(func(creds config.SocialSecrets) error {
			if err := authn.SetSocialSecrets(auth.SocialSecrets{
				GoogleClientSecret: reveal.Value(creds.GoogleClientSecret),
				ApplePrivateKey:    reveal.Value(creds.ApplePrivateKey),
				AppleKeyID:         reveal.Value(creds.AppleKeyID),
			}); err != nil {
				return err // the old credentials stay; retried after the next fetch
			}
			log.Info("auth: social sign-in credentials rotated", "credentials", creds.Fingerprint())
			return nil
		})
	}
	applied := keys.Fingerprint()
	log.Info("auth: keyring loaded", "keyring", applied)
	cfg.Secrets.OnAuthSecrets(func(k secrets.Keyring) error {
		if k.Fingerprint() == applied {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, rotationTimeout) // shutdown aborts a rotation in progress
		defer cancel()
		current, previous, revoked := reveal.Keyring(k)
		if err := authn.SetSecrets(ctx, auth.Secrets{Current: current, Previous: previous, Revoked: revoked}); err != nil {
			return err // the old keyring stays; retried after the next fetch
		}
		log.Info("auth: AUTH_SECRET rotated", "from", applied, "to", k.Fingerprint())
		applied = k.Fingerprint()
		return nil
	})
	return authn, nil
}

// socialProviders builds the Google and Apple configs that are turned on
// (config.Social), with their current credentials.
func socialProviders(cfg config.Config) (*auth.GoogleConfig, *auth.AppleConfig, error) {
	social := cfg.Social
	if !social.GoogleEnabled() && !social.AppleEnabled() {
		return nil, nil, nil
	}
	creds, err := cfg.Secrets.SocialSecrets()
	if err != nil {
		return nil, nil, err
	}
	var google *auth.GoogleConfig
	var apple *auth.AppleConfig
	if social.GoogleEnabled() {
		google = &auth.GoogleConfig{ClientID: social.GoogleClientID, ClientSecret: reveal.Value(creds.GoogleClientSecret)}
	}
	if social.AppleEnabled() {
		apple = &auth.AppleConfig{ClientID: social.AppleClientID, TeamID: social.AppleTeamID, KeyID: reveal.Value(creds.AppleKeyID), PrivateKey: reveal.Value(creds.ApplePrivateKey)}
	}
	return google, apple, nil
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
