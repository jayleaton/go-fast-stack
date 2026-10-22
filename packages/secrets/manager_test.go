package secrets_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
	"github.com/jayleaton/ai-patterns-go/packages/secrets/reveal"
)

const key = "RESEND_API_KEY"

// Every test runs in a synctest bubble: advance (time.Sleep) moves a fake
// clock once the Manager's goroutine is idle, so polling is exercised without
// real waits.

func start(t *testing.T, p secrets.Provider, o secrets.Options) *secrets.Manager {
	t.Helper()
	m := secrets.NewManager(p, o)
	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Stop)
	return m
}

func get(t *testing.T, m *secrets.Manager, name string) string {
	t.Helper()
	v, err := m.Get(name)
	if err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	return reveal.Value(v)
}

func TestRotationPickedUpWithinOneInterval(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_old", "OTHER": "x"})
		log, _ := newLogger()
		m := start(t, dopplerFor(f), secrets.Options{Keys: []string{key}, Required: []string{key}, Logger: log})
		var rec recorder
		m.OnChange(key, rec.hook) // already loaded: called right away

		if got := get(t, m, key); got != "re_old" {
			t.Fatalf("Get = %q", got)
		}
		if v, _ := m.Get("OTHER"); !v.IsZero() {
			t.Fatal("key outside Options.Keys was published")
		}

		f.set(map[string]string{key: "re_new"})
		advance(secrets.DefaultInterval + secrets.DefaultInterval/10) // one interval plus max jitter
		synctest.Wait()

		if got := get(t, m, key); got != "re_new" {
			t.Fatalf("after one interval Get = %q, want re_new", got)
		}
		if got := rec.values(); !slices.Equal(got, []string{"re_old", "re_new"}) {
			t.Fatalf("hook saw %v", got)
		}
		st := m.Status()
		if st.Generation != 2 || st.Revision != `"v2"` || st.Keys != 1 || st.ConsecutiveFailures != 0 || !st.Loaded {
			t.Fatalf("status = %+v", st)
		}

		// Unchanged config: conditional request, 304, no hook call.
		advance(2 * (secrets.DefaultInterval + secrets.DefaultInterval/10))
		synctest.Wait()
		if _, notMod := f.stats(); notMod != 2 {
			t.Fatalf("304s = %d, want 2", notMod)
		}
		if got := rec.values(); len(got) != 2 || m.Status().Generation != 2 {
			t.Fatalf("unchanged poll called hooks: %v", got)
		}
	})
}

func TestProviderDownKeepsLastGoodUntilMaxStaleness(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_good"})
		log, logs := newLogger()
		m := start(t, dopplerFor(f), secrets.Options{Required: []string{key}, Logger: log})
		f.fail(503, "")

		advance(secrets.DefaultMaxStaleness - time.Minute)
		synctest.Wait()
		if got := get(t, m, key); got != "re_good" {
			t.Fatalf("during outage Get = %q", got)
		}
		if err := m.Healthy(); err != nil {
			t.Fatalf("healthy within the stale budget: %v", err)
		}
		if st := m.Status(); st.ConsecutiveFailures < 2 || !strings.Contains(st.LastError, "503") {
			t.Fatalf("status during outage = %+v", st)
		}

		advance(2 * time.Minute)
		synctest.Wait()
		if err := m.Healthy(); !errors.Is(err, secrets.ErrStale) {
			t.Fatalf("Healthy past max staleness = %v, want ErrStale", err)
		}
		if _, err := m.Get(key); !errors.Is(err, secrets.ErrStale) {
			t.Fatalf("Get past max staleness = %v, want ErrStale", err)
		}
		if !m.Status().Stale {
			t.Fatal("staleness not reported in status")
		}
		// The next failed poll (within MaxBackoff) logs it as an error.
		advance(secrets.DefaultMaxBackoff + secrets.DefaultMaxBackoff/10)
		synctest.Wait()
		if !strings.Contains(logs.String(), "past max staleness") {
			t.Fatal("staleness not logged")
		}

		// Backoff is bounded: recovery is seen within MaxBackoff (+ jitter).
		f.recover()
		advance(secrets.DefaultMaxBackoff + secrets.DefaultMaxBackoff/10)
		synctest.Wait()
		if err := m.Healthy(); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
		if !strings.Contains(logs.String(), "provider recovered") {
			t.Fatal("recovery not logged")
		}
	})
}

func TestInvalidSnapshotRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, raw, want string
	}{
		{"not json", `<html>oops</html>`, "malformed"},
		{"array", `["re_bad"]`, "malformed"},
		{"null", `null`, "malformed"},
		{"non-string value", `{"RESEND_API_KEY": 42}`, "RESEND_API_KEY is not a string"},
		{"trailing data", `{"RESEND_API_KEY": "re_bad"} {}`, "trailing data"},
		{"missing required", `{"OTHER": "re_bad"}`, "missing required [RESEND_API_KEY]"},
		{"empty required", `{"RESEND_API_KEY": ""}`, "missing required [RESEND_API_KEY]"},
		{"custom validator", `{"RESEND_API_KEY": "bad_prefix"}`, "must start with re_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				f := newFake(map[string]string{key: "re_good"})
				log, logs := newLogger()
				m := start(t, dopplerFor(f), secrets.Options{
					Required: []string{key}, Logger: log,
					Validate: func(s secrets.Snapshot) error {
						if v, _ := s.Get(key); !strings.HasPrefix(reveal.Value(v), "re_") {
							return errors.New(key + " must start with re_")
						}
						return nil
					},
				})
				var rec recorder
				m.OnChange(key, rec.hook)

				f.setRaw(tt.raw)
				err := m.Refresh(t.Context())
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Refresh err = %v, want %q", err, tt.want)
				}
				if strings.Contains(err.Error(), "re_bad") || strings.Contains(err.Error(), "bad_prefix") {
					t.Fatalf("error leaks the value: %v", err)
				}
				if got := get(t, m, key); got != "re_good" {
					t.Fatalf("invalid snapshot was published: %q", got)
				}
				// The poll loop keeps rejecting it without publishing.
				advance(secrets.DefaultInterval + secrets.DefaultInterval/10)
				synctest.Wait()
				if got := rec.values(); len(got) != 1 || get(t, m, key) != "re_good" {
					t.Fatalf("hook saw %v", got)
				}
				if strings.Contains(logs.String(), "re_bad") {
					t.Fatal("log leaks the value")
				}
			})
		})
	}
}

func TestRetryAfterIsHonored(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_a"})
		log, _ := newLogger()
		start(t, dopplerFor(f), secrets.Options{Logger: log})
		f.fail(429, "120")

		advance(10 * time.Minute)
		synctest.Wait()
		times := f.requestTimes() // [Start, first poll (429), retry, ...]
		if len(times) < 3 {
			t.Fatalf("requests = %d", len(times))
		}
		if gap := times[2].Sub(times[1]); gap < 108*time.Second || gap > 132*time.Second {
			t.Fatalf("retry %s after a 429 with Retry-After: 120, want 120s ± 10%%", gap)
		}
	})
}

func TestBackoffIsBounded(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_a"})
		log, _ := newLogger()
		start(t, dopplerFor(f), secrets.Options{Logger: log, MaxBackoff: time.Minute, MaxStaleness: time.Hour})
		f.fail(503, "")

		advance(30 * time.Minute)
		synctest.Wait()
		times := f.requestTimes()
		var gaps []time.Duration
		for i := 2; i < len(times); i++ {
			gaps = append(gaps, times[i].Sub(times[i-1]))
		}
		// 5s doubling (± jitter) up to the 1m cap, then flat at the cap.
		if len(gaps) < 20 || gaps[0] > 6*time.Second || gaps[1] < 9*time.Second {
			t.Fatalf("gaps = %v", gaps)
		}
		for _, g := range gaps {
			if g > time.Minute+6*time.Second {
				t.Fatalf("gap %s over MaxBackoff + jitter; gaps = %v", g, gaps)
			}
		}
	})
}

func TestStartFailsWithoutValidSnapshot(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{"OTHER": "x"})
		log, _ := newLogger()
		m := secrets.NewManager(dopplerFor(f), secrets.Options{Required: []string{key}, Logger: log})
		var rec recorder
		m.OnChange(key, rec.hook) // registered before Start: not called until loaded

		err := m.Start(t.Context())
		if !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("Start = %v, want ErrInvalid", err)
		}
		if calls, _ := f.stats(); calls != secrets.DefaultStartAttempts {
			t.Fatalf("attempts = %d", calls)
		}
		if err := m.Healthy(); !errors.Is(err, secrets.ErrNotLoaded) {
			t.Fatalf("Healthy = %v", err)
		}
		if _, err := m.Get(key); !errors.Is(err, secrets.ErrNotLoaded) {
			t.Fatalf("Get = %v", err)
		}
		if len(rec.values()) != 0 {
			t.Fatal("hook called without a snapshot")
		}
		m.Stop() // safe after a failed Start
		if err := m.Start(t.Context()); err == nil {
			t.Fatal("second Start succeeded")
		}

		// A cancelled context stops the retries.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := secrets.NewManager(dopplerFor(f), secrets.Options{Required: []string{key}, Logger: log}).Start(ctx); err == nil {
			t.Fatal("Start with a cancelled context succeeded")
		}
	})
}

func TestHooks(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_1", "OPTIONAL": "o"})
		log, logs := newLogger()
		m := secrets.NewManager(dopplerFor(f), secrets.Options{Logger: log})
		var first, optional recorder
		m.OnChange(key, first.hook) // before Start: called by the first load
		m.OnChange(key, func(secrets.Secret) error { return errors.New("client rejected the key") })
		m.OnChange(key, func(secrets.Secret) error { panic("boom") })
		m.OnChange("OPTIONAL", optional.hook)
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Stop)

		f.set(map[string]string{key: "re_2"}) // OPTIONAL removed upstream
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := first.values(); !slices.Equal(got, []string{"re_1", "re_2"}) {
			t.Fatalf("hook saw %v", got)
		}
		if got := optional.values(); !slices.Equal(got, []string{"o", ""}) {
			t.Fatalf("removed key hook saw %v, want a zero Secret last", got)
		}
		if st := m.Status(); st.HookErrors != 4 {
			t.Fatalf("hook errors = %d, want 4 (error + panic, twice)", st.HookErrors)
		}
		if !strings.Contains(logs.String(), "consumer rejected change") || !strings.Contains(logs.String(), "panic: boom") {
			t.Fatal("hook failures not logged")
		}
		if !strings.Contains(logs.String(), "changed=\"[OPTIONAL RESEND_API_KEY]\"") {
			t.Fatalf("rotation log does not name the changed keys:\n%s", logs.String())
		}
	})
}

func TestStopEndsPolling(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_a"})
		log, _ := newLogger()
		m := start(t, dopplerFor(f), secrets.Options{Interval: 10 * time.Second, Logger: log})
		m.Stop()
		m.Stop() // idempotent
		if got := get(t, m, key); got != "re_a" {
			t.Fatalf("values not readable after Stop: %q", got)
		}
		calls, _ := f.stats()
		advance(time.Hour)
		synctest.Wait()
		if got, _ := f.stats(); got != calls {
			t.Fatalf("polled after Stop: %d calls, want %d", got, calls)
		}
		if _, err := m.Get(key); !errors.Is(err, secrets.ErrStale) {
			t.Fatalf("a stopped manager must go stale, got %v", err)
		}

		// Cancelling Start's context also stops the loop (synctest fails the
		// test if its goroutine leaks).
		ctx, cancel := context.WithCancel(t.Context())
		if err := secrets.NewManager(dopplerFor(f), secrets.Options{Logger: log}).Start(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
	})
}

func TestEnvIsStartupOnly(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log, _ := newLogger()
		m := secrets.NewManager(secrets.NewEnv(map[string]string{key: "re_env"}), secrets.Options{Required: []string{key}, Logger: log})
		if m.Provider() != "env" {
			t.Fatalf("provider = %s", m.Provider())
		}
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		m.Stop()
		advance(24 * time.Hour)
		if err := m.Healthy(); err != nil {
			t.Fatalf("env went stale: %v", err)
		}
		if got := get(t, m, key); got != "re_env" {
			t.Fatalf("Get = %q", got)
		}

		empty := secrets.NewManager(secrets.NewEnv(map[string]string{key: ""}), secrets.Options{Required: []string{key}, Logger: log})
		if err := empty.Start(t.Context()); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("empty required env value: %v", err)
		}
	})
}

func TestNoSecretValuesInLogs(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		values := []string{"re_first_value", "re_second_value", "re_third_value", testToken}
		f := newFake(map[string]string{key: values[0]})
		log, logs := newLogger()
		m := secrets.NewManager(dopplerFor(f), secrets.Options{Required: []string{key}, Logger: log})
		m.OnChange(key, func(v secrets.Secret) error {
			log.Info("consumer rebuilt", "key", v, "fp", v.Fingerprint()) // a careless consumer logs the Secret itself
			return nil
		})
		if err := m.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Stop)

		f.set(map[string]string{key: values[1]}) // rotation
		advance(time.Minute + 6*time.Second)
		f.setRaw(`{"RESEND_API_KEY": ["` + values[2] + `"]}`) // malformed
		advance(time.Minute + 6*time.Second)
		f.fail(500, "") // the fake echoes the config in its error body
		advance(20 * time.Minute)
		f.fail(401, "")
		advance(10 * time.Minute)
		synctest.Wait()

		out := logs.String()
		for _, v := range values {
			if strings.Contains(out, v) {
				t.Fatalf("log contains secret value %q:\n%s", v, out)
			}
		}
		for _, want := range []string{"secrets: loaded", "secrets: rotated", "not a string", "500", "token rejected", "past max staleness", "key=[redacted]", "fp=fp:"} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q", want)
			}
		}
		if st := m.Status(); strings.Contains(st.LastError, testToken) {
			t.Fatal("status leaks the token")
		}
	})
}

func TestFailedHookIsRetriedAfterTheNextFetch(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		f := newFake(map[string]string{key: "re_1"})
		log, _ := newLogger()
		m := start(t, dopplerFor(f), secrets.Options{Logger: log})
		var ok, flaky recorder
		calls := 0
		m.OnChange(key, ok.hook)
		m.OnChange(key, func(v secrets.Secret) error {
			if calls++; calls <= 2 {
				return errors.New("client not ready")
			}
			return flaky.hook(v)
		})

		advance(secrets.DefaultInterval + secrets.DefaultInterval/10) // unchanged poll: retry #1 fails
		synctest.Wait()
		advance(secrets.DefaultInterval + secrets.DefaultInterval/10) // retry #2 succeeds
		synctest.Wait()
		advance(secrets.DefaultInterval + secrets.DefaultInterval/10) // nothing left to retry
		synctest.Wait()
		if got := flaky.values(); !slices.Equal(got, []string{"re_1"}) || calls != 3 {
			t.Fatalf("flaky hook calls=%d saw %v", calls, got)
		}
		if got := ok.values(); len(got) != 1 {
			t.Fatalf("a hook that succeeded was re-run: %v", got)
		}
		if st := m.Status(); st.HookErrors != 2 {
			t.Fatalf("hook errors = %d", st.HookErrors)
		}
	})
}
