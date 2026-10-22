// Command preview serves every UI page with in-memory fakes, so the UI can be
// checked before the real server, auth and database are wired in.
//
//	go run ./apps/web/ui/cmd/preview            # 0.0.0.0:3001
//	go run ./apps/web/ui/cmd/preview -verify    # production-like email verification
//
// Sign in as demo@example.com / password123. Email links are logged.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/web/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui"
	"github.com/jayleaton/ai-patterns-go/apps/web/ui/uifake"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:3001", "listen address")
	verify := flag.Bool("verify", false, "require email verification (production behavior)")
	welcome := flag.Bool("welcome", true, "show the template welcome page at /")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := uifake.NewStore()
	store.RequireEmailVerification = *verify
	store.OnMail = func(m uifake.Mail) {
		log.Info("email (not sent)", "kind", m.Kind, "to", m.To, "link", m.URL)
	}
	store.AddUser("Demo User", "demo@example.com", "password123", true)

	fakeAuth := uifake.Auth{S: store}
	mux := http.NewServeMux()
	ui.New(ui.Deps{
		Auth:     fakeAuth,
		Users:    uifake.Users{S: store},
		UserFrom: fakeAuth.UserFrom,
		Features: config.Features{EmailVerification: *verify, TemplateWelcome: *welcome},
		Logger:   log,
	}).Routes(mux)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           fakeAuth.Middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("ui preview listening", "addr", *addr, "login", "demo@example.com / password123")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("preview", "err", err)
		os.Exit(1)
	}
}
