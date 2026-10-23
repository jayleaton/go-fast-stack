package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// The web service migrates the app database in its pre-deploy and the admin
// service deploys at the same time, so the admin can start against an app
// database a migration or two behind (or not migrated at all on a first
// deploy). serve waits for it, up to databaseWait, instead of exiting.
const (
	databaseWait     = 3 * time.Minute
	databaseRetryMin = 2 * time.Second
	databaseRetryMax = 20 * time.Second
	// retryBackoffFactor multiplies the wait after each failed attempt.
	retryBackoffFactor = 2
	// startingRetryAfter is the Retry-After (seconds) a request gets while
	// the databases are not ready yet.
	startingRetryAfter = "5"
)

// waitForDatabase calls open until it succeeds, backing off from
// databaseRetryMin to databaseRetryMax and logging each wait. It gives up
// with the last error after databaseWait, or when ctx ends.
func waitForDatabase[T any](ctx context.Context, log *slog.Logger, name string, open func(context.Context) (T, error)) (T, error) {
	deadline := time.Now().Add(databaseWait)
	delay := databaseRetryMin
	for attempt := 1; ; attempt++ {
		opened, err := open(ctx)
		if err == nil {
			if attempt > 1 {
				log.Info("database: ready", "database", name, "attempts", attempt)
			}
			return opened, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return opened, fmt.Errorf("not ready after %s (%d attempts): %w", databaseWait, attempt, err)
		}
		wait := min(delay, left)
		log.Warn("database: not ready, waiting", "database", name, "attempt", attempt,
			"retry_in", wait, "gives_up_in", left.Round(time.Second), "err", err)
		select {
		case <-ctx.Done():
			return opened, ctx.Err()
		case <-time.After(wait):
		}
		delay = min(delay*retryBackoffFactor, databaseRetryMax)
	}
}

// startingHandler answers every request (/healthz included) with a bare 503
// until ready installs the real handler, so the platform keeps routing to
// the previous deploy while this one waits for its databases.
type startingHandler struct {
	next atomic.Pointer[http.Handler]
}

// ready routes every request from now on to handler.
func (starting *startingHandler) ready(handler http.Handler) { starting.next.Store(&handler) }

func (starting *startingHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler := starting.next.Load(); handler != nil {
		(*handler).ServeHTTP(writer, request)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Retry-After", startingRetryAfter)
	http.Error(writer, "starting", http.StatusServiceUnavailable)
}
