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

	"github.com/jayleaton/go-fast-stack/apps/web/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/web/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/web/ui"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:3001", "listen address")
	verify := flag.Bool("verify", false, "require email verification (production behavior)")
	welcome := flag.Bool("welcome", true, "show the template welcome page at /")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := NewStore()
	store.RequireEmailVerification = *verify
	store.OnMail = func(m Mail) {
		log.Info("email (not sent)", "kind", m.Kind, "to", m.To, "link", m.URL)
	}
	demo := store.AddUser("Demo User", "demo@example.com", "password123", true)
	// Two other devices so the Account page has sessions to revoke.
	store.NewSession(demo.ID, "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1", "192.0.2.24", 26*time.Hour)
	store.NewSession(demo.ID, "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0", "198.51.100.7", 72*time.Hour)

	fakeAuth := Auth{S: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+confirmEmailPath, store.ConfirmEmail)
	mux.Handle("/", ui.New(ui.Deps{
		Auth:     fakeAuth,
		Users:    Users{S: store},
		Gate:     access.NewGate(access.FromContext(fakeAuth.Caller)),
		Features: config.Features{EmailVerification: *verify, TemplateWelcome: *welcome},
		Logger:   log,
	}).Handler())

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
