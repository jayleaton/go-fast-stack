package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// retriesPerStaleBudget is how many fetch retries at the derived max backoff
// fit in MaxStaleness, so a provider outage gets several attempts before the
// values go stale.
const retriesPerStaleBudget = 3

// Defaults for Options.
const (
	DefaultInterval      = 60 * time.Second
	DefaultJitter        = 0.1
	DefaultTimeout       = 10 * time.Second
	DefaultMaxBackoff    = 5 * time.Minute
	DefaultMaxStaleness  = 15 * time.Minute
	DefaultStartAttempts = 3
	minBackoff           = 5 * time.Second
)

// Options configure a Manager. Zero values take the defaults above.
type Options struct {
	// Keys limits the snapshot to these names; anything else the provider
	// returns is dropped. Empty keeps every name.
	Keys []string
	// Required names must be present and non-empty, or the snapshot is rejected.
	Required []string
	// Validate runs after the Required check, before a snapshot is published.
	// Its error must not contain values.
	Validate func(Snapshot) error

	Interval      time.Duration // between successful polls (jittered)
	Jitter        float64       // ± fraction applied to every wait, 0..1
	Timeout       time.Duration // per fetch
	MaxBackoff    time.Duration // cap on the wait after failures and on Retry-After; default min(5m, MaxStaleness/3)
	MaxStaleness  time.Duration // last good snapshot older than this: Healthy and Get report ErrStale
	StartAttempts int           // fetches Start tries before failing boot

	Logger *slog.Logger // default slog.Default() at the time of logging
}

// Status is a point-in-time view for health checks and metrics. It holds no
// secret values.
type Status struct {
	Provider            string
	Loaded              bool
	Generation          uint64    // increments each time a published snapshot changes a value
	Revision            string    // provider revision of the published snapshot
	Keys                int       // names in the published snapshot
	LastSuccess         time.Time // last fetch that was accepted (changed or not)
	LastAttempt         time.Time
	LastError           string // last fetch or validation error, "" after a success
	ConsecutiveFailures int
	HookErrors          uint64 // change hooks that returned an error or panicked
	Stale               bool
}

// Hook is called with the new value of one key after it changes (a zero
// Secret when the key was removed). Returning an error (or panicking) keeps
// the consumer on its previous client; the error is logged and counted, and
// the hook is called again with the current value after each later
// successful fetch until it succeeds.
//
// Hooks run one at a time, in order, on the goroutine that fetched. A hook
// may call Get, Healthy and Status, but must not call OnChange, Refresh or
// Stop: they wait for the hook to return and would deadlock.
type Hook func(Secret) error

// Manager polls a Provider and publishes validated snapshots. Safe for
// concurrent use. Build it with NewManager, call Start once, Stop on shutdown.
type Manager struct {
	provider Provider
	options  Options
	static   bool

	refreshMu sync.Mutex      // serializes fetches and hook calls, so hooks see changes in order
	failed    map[hookID]bool // hooks whose last call failed; retried after each good fetch (guarded by refreshMu)

	mu          sync.RWMutex
	snap        Snapshot
	loaded      bool
	gen         uint64
	lastSuccess time.Time
	lastAttempt time.Time
	lastErr     error
	failures    int
	hookErrors  uint64
	hooks       map[string][]Hook
	cancel      context.CancelFunc
	done        chan struct{}
}

type hookID struct {
	name  string
	index int // index in hooks[name]; hooks are append-only
}

// NewManager returns a Manager for p. It does no I/O; Start does the first fetch.
func NewManager(provider Provider, options Options) *Manager {
	if options.Interval <= 0 {
		options.Interval = DefaultInterval
	}
	if options.Jitter <= 0 || options.Jitter > 1 {
		options.Jitter = DefaultJitter
	}
	if options.Timeout <= 0 {
		options.Timeout = DefaultTimeout
	}
	if options.MaxStaleness <= 0 {
		options.MaxStaleness = DefaultMaxStaleness
	}
	if options.MaxBackoff <= 0 {
		options.MaxBackoff = min(DefaultMaxBackoff, options.MaxStaleness/retriesPerStaleBudget)
	}
	if options.StartAttempts <= 0 {
		options.StartAttempts = DefaultStartAttempts
	}
	_, static := provider.(Static)
	return &Manager{provider: provider, options: options, static: static, hooks: map[string][]Hook{}, failed: map[hookID]bool{}}
}

// Format prints the provider name only, so a Manager in a log line or %+v
// never exposes its snapshot.
func (manager *Manager) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "secrets.Manager(%s)", manager.provider.Name())
}

// Provider is the provider's name ("env", "doppler").
func (manager *Manager) Provider() string { return manager.provider.Name() }

// Start fetches the first snapshot (retrying StartAttempts times with
// backoff, once for a Static provider) and, unless the provider is Static, polls until ctx ends or Stop
// is called. Without a valid first snapshot it returns the error: a fresh
// process must not serve without its secrets.
func (manager *Manager) Start(ctx context.Context) error {
	manager.mu.Lock()
	if manager.done != nil {
		manager.mu.Unlock()
		return errors.New("secrets: manager already started")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	manager.cancel, manager.done = cancel, make(chan struct{})
	manager.mu.Unlock()

	for attempt := 1; ; attempt++ { // loopCtx: a concurrent Stop interrupts the retries
		err := manager.Refresh(loopCtx)
		if err == nil {
			break
		}
		// A static provider returns the same values every time: fail at once.
		if manager.static || attempt >= manager.options.StartAttempts || !sleep(loopCtx, manager.backoff(attempt, err)) {
			cancel()
			close(manager.done)
			return err
		}
		manager.log().Warn("secrets: initial fetch failed, retrying", "provider", manager.provider.Name(), "attempt", attempt, "err", err)
	}
	st := manager.Status()
	manager.log().Info("secrets: loaded", "provider", st.Provider, "keys", st.Keys, "generation", st.Generation, "live", !manager.static)

	if manager.static {
		cancel()
		close(manager.done)
		return nil
	}
	go manager.loop(loopCtx)
	return nil
}

// Stop ends polling and waits for an in-flight fetch to finish. Published
// values stay readable. Safe to call more than once, or without Start.
func (manager *Manager) Stop() {
	manager.mu.Lock()
	cancel, done := manager.cancel, manager.done
	manager.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (manager *Manager) loop(ctx context.Context) {
	defer close(manager.done)
	failures := 0
	timer := time.NewTimer(manager.jitter(manager.options.Interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := manager.Refresh(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if failures > 0 {
				manager.log().Info("secrets: provider recovered", "provider", manager.provider.Name(), "failures", failures)
			}
			failures = 0
			timer.Reset(manager.jitter(manager.options.Interval))
			continue
		}
		failures++
		wait := manager.backoff(failures, err)
		if herr := manager.Healthy(); herr != nil {
			manager.log().Error("secrets: refresh failed, past max staleness", "provider", manager.provider.Name(), "failures", failures, "retry_in", wait, "err", err, "health", herr)
		} else {
			manager.log().Warn("secrets: refresh failed, serving last good snapshot", "provider", manager.provider.Name(), "failures", failures, "retry_in", wait, "err", err)
		}
		timer.Reset(wait)
	}
}

// Refresh fetches, validates and publishes one snapshot now, then calls the
// hooks of every key whose value changed. On error the previous snapshot
// stays published. The poll loop calls it; so can a future webhook.
func (manager *Manager) Refresh(ctx context.Context) error {
	manager.refreshMu.Lock()
	defer manager.refreshMu.Unlock()

	manager.mu.RLock()
	prev := manager.snap
	manager.mu.RUnlock()

	fctx, cancel := context.WithTimeout(ctx, manager.options.Timeout)
	next, err := manager.provider.Fetch(fctx, prev)
	cancel()
	if err == nil {
		next = manager.filter(next)
		err = manager.validate(next)
	}
	now := time.Now()
	if err != nil {
		err = fmt.Errorf("secrets: refresh from %s: %w", manager.provider.Name(), err)
		manager.mu.Lock()
		manager.lastAttempt, manager.lastErr = now, err
		manager.failures++
		manager.mu.Unlock()
		return err
	}

	changed := diff(prev, next)
	manager.mu.Lock()
	manager.snap, manager.loaded = next, true
	manager.lastSuccess, manager.lastAttempt, manager.lastErr, manager.failures = now, now, nil, 0
	if len(changed) > 0 {
		manager.gen++
	}
	gen := manager.gen
	var calls []hookID
	for _, name := range changed {
		for index := range manager.hooks[name] {
			calls = append(calls, hookID{name, index})
		}
	}
	for id := range manager.failed {
		if !slices.Contains(changed, id.name) {
			calls = append(calls, id)
		}
	}
	hooks := make([]Hook, len(calls))
	for index, id := range calls {
		hooks[index] = manager.hooks[id.name][id.index]
	}
	manager.mu.Unlock()

	if len(changed) > 0 && gen > 1 {
		manager.log().Info("secrets: rotated", "provider", manager.provider.Name(), "generation", gen, "changed", changed)
	}
	for index, id := range calls {
		secret, _ := next.Get(id.name)
		manager.runHook(id, hooks[index], secret)
	}
	return nil
}

// OnChange registers fn for name. If a snapshot is already published, fn is
// called right away with the current value (zero if absent), so wiring code
// builds its first client the same way it rebuilds later ones.
func (manager *Manager) OnChange(name string, fn Hook) {
	manager.refreshMu.Lock()
	defer manager.refreshMu.Unlock()
	manager.mu.Lock()
	manager.hooks[name] = append(manager.hooks[name], fn)
	id := hookID{name, len(manager.hooks[name]) - 1}
	loaded := manager.loaded
	secret, _ := manager.snap.Get(name)
	manager.mu.Unlock()
	if loaded {
		manager.runHook(id, fn, secret)
	}
}

// runHook calls fn and records whether it must be retried. Caller holds refreshMu.
func (manager *Manager) runHook(id hookID, fn Hook, secret Secret) {
	err := func() (err error) {
		defer func() {
			if value := recover(); value != nil {
				err = fmt.Errorf("panic: %v", value)
			}
		}()
		return fn(secret)
	}()
	if err == nil {
		delete(manager.failed, id)
		return
	}
	manager.failed[id] = true
	manager.mu.Lock()
	manager.hookErrors++
	manager.mu.Unlock()
	manager.log().Error("secrets: consumer rejected change, keeping its previous client; retrying after the next fetch", "key", id.name, "err", err)
}

// Get returns the current value of name (zero if the snapshot lacks it), or
// ErrNotLoaded / ErrStale.
func (manager *Manager) Get(name string) (Secret, error) {
	if err := manager.Healthy(); err != nil {
		return Secret{}, err
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	secret, _ := manager.snap.Get(name)
	return secret, nil
}

// Healthy is nil while a snapshot is published and younger than
// MaxStaleness, else ErrNotLoaded or an error wrapping ErrStale. Static
// providers never go stale.
func (manager *Manager) Healthy() error {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if !manager.loaded {
		return ErrNotLoaded
	}
	if age := time.Since(manager.lastSuccess); !manager.static && age > manager.options.MaxStaleness {
		return fmt.Errorf("%w: last good %s snapshot is %s old (limit %s)", ErrStale, manager.provider.Name(), age.Round(time.Second), manager.options.MaxStaleness)
	}
	return nil
}

// Status returns counters and timestamps for health checks and metrics.
func (manager *Manager) Status() Status {
	stale := errors.Is(manager.Healthy(), ErrStale)
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	st := Status{
		Provider: manager.provider.Name(), Loaded: manager.loaded, Generation: manager.gen,
		Revision: manager.snap.Revision(), Keys: manager.snap.Len(),
		LastSuccess: manager.lastSuccess, LastAttempt: manager.lastAttempt,
		ConsecutiveFailures: manager.failures, HookErrors: manager.hookErrors, Stale: stale,
	}
	if manager.lastErr != nil {
		st.LastError = manager.lastErr.Error()
	}
	return st
}

func (manager *Manager) filter(snapshot Snapshot) Snapshot {
	if len(manager.options.Keys) == 0 {
		return snapshot
	}
	out := Snapshot{values: make(map[string]Secret, len(manager.options.Keys)), revision: snapshot.revision}
	for _, key := range manager.options.Keys {
		if secret, ok := snapshot.values[key]; ok {
			out.values[key] = secret
		}
	}
	return out
}

func (manager *Manager) validate(snapshot Snapshot) error {
	var missing []string
	for _, key := range manager.options.Required {
		if secret, ok := snapshot.Get(key); !ok || secret.IsZero() {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing required %v", ErrInvalid, missing)
	}
	if manager.options.Validate != nil {
		if err := manager.options.Validate(snapshot); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// backoff is the wait after the n-th consecutive failure: 5s doubling, capped
// at MaxBackoff, at least the provider's Retry-After (also capped), jittered.
func (manager *Manager) backoff(count int, err error) time.Duration {
	duration := min(minBackoff, manager.options.Interval)
	for index := 1; index < count && duration < manager.options.MaxBackoff; index++ {
		duration *= 2
	}
	if ra, ok := errors.AsType[RetryAfter](err); ok && ra.RetryAfter() > duration {
		duration = ra.RetryAfter()
	}
	return manager.jitter(min(duration, manager.options.MaxBackoff))
}

func (manager *Manager) jitter(duration time.Duration) time.Duration {
	value := 1 + manager.options.Jitter*(2*rand.Float64()-1)
	return time.Duration(float64(duration) * value)
}

func (manager *Manager) log() *slog.Logger {
	if manager.options.Logger != nil {
		return manager.options.Logger
	}
	return slog.Default()
}

// diff returns the sorted names whose value was added, changed or removed.
func diff(prev, next Snapshot) []string {
	var out []string
	for key, value := range next.values {
		if old, ok := prev.values[key]; !ok || !old.Equal(value) {
			out = append(out, key)
		}
	}
	for key := range prev.values {
		if _, ok := next.values[key]; !ok {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
