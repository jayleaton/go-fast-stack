// Command server is the single production binary.
//
//	server serve    run the HTTP server (default)
//	server migrate  run auth migrations, then app migrations
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

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/api"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/server"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/server/devauth"
	"github.com/jayleaton/ai-patterns-go/apps/web/internal/services"
	"github.com/jayleaton/ai-patterns-go/packages/database"
	"github.com/jayleaton/ai-patterns-go/packages/database/authstub"
)

// migrations run in order: auth owns "user", app tables reference it.
var migrations = []func(context.Context, *pgxpool.Pool) error{
	authstub.Migrate, // TODO(feat/auth): auth.Migrate
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

func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := database.Open(ctx, cfg.DatabaseURL)
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
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var auth server.Auth = devauth.NoAuth{}
	caller := devauth.CallerFrom
	if cfg.Features.DevAuth {
		if auth, err = devauth.New(ctx, pool); err != nil {
			return fmt.Errorf("dev auth (run `make migrate` first): %w", err)
		}
		log.Warn("dev auth fake: every request is signed in as " + devauth.UserEmail)
	}

	users := services.NewUserService(database.NewUserRepository(pool))
	handler := server.New(server.Deps{
		Config: cfg,
		Log:    log,
		DB:     pool,
		Auth:   auth,
		Users:  api.NewUsers(users, caller),
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
	return nil
}

func newLogger(env config.Env) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if env == config.Production {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
