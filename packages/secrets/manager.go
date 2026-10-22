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
	p      Provider
	o      Options
	static bool

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
	name string
	i    int // index in hooks[name]; hooks are append-only
}

// NewManager returns a Manager for p. It does no I/O; Start does the first fetch.
func NewManager(p Provider, o Options) *Manager {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Jitter <= 0 || o.Jitter > 1 {
		o.Jitter = DefaultJitter
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxStaleness <= 0 {
		o.MaxStaleness = DefaultMaxStaleness
	}
	if o.MaxBackoff <= 0 {
		// At least ~3 retries fit in the stale budget.
		o.MaxBackoff = min(DefaultMaxBackoff, o.MaxStaleness/3)
	}
	if o.StartAttempts <= 0 {
		o.StartAttempts = DefaultStartAttempts
	}
	_, static := p.(Static)
	return &Manager{p: p, o: o, static: static, hooks: map[string][]Hook{}, failed: map[hookID]bool{}}
}

// Format prints the provider name only, so a Manager in a log line or %+v
// never exposes its snapshot.
func (m *Manager) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "secrets.Manager(%s)", m.p.Name())
}

// Provider is the provider's name ("env", "doppler").
func (m *Manager) Provider() string { return m.p.Name() }

// Start fetches the first snapshot (retrying StartAttempts times with
// backoff) and, unless the provider is Static, polls until ctx ends or Stop
// is called. Without a valid first snapshot it returns the error: a fresh
// process must not serve without its secrets.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.done != nil {
		m.mu.Unlock()
		return errors.New("secrets: manager already started")
	}
	loopCtx, cancel := context.WithCancel(ctx)
	m.cancel, m.done = cancel, make(chan struct{})
	m.mu.Unlock()

	for attempt := 1; ; attempt++ { // loopCtx: a concurrent Stop interrupts the retries
		err := m.Refresh(loopCtx)
		if err == nil {
			break
		}
		if attempt >= m.o.StartAttempts || !sleep(loopCtx, m.backoff(attempt, err)) {
			cancel()
			close(m.done)
			return err
		}
		m.log().Warn("secrets: initial fetch failed, retrying", "provider", m.p.Name(), "attempt", attempt, "err", err)
	}
	st := m.Status()
	m.log().Info("secrets: loaded", "provider", st.Provider, "keys", st.Keys, "generation", st.Generation, "live", !m.static)

	if m.static {
		cancel()
		close(m.done)
		return nil
	}
	go m.loop(loopCtx)
	return nil
}

// Stop ends polling and waits for an in-flight fetch to finish. Published
// values stay readable. Safe to call more than once, or without Start.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (m *Manager) loop(ctx context.Context) {
	defer close(m.done)
	failures := 0
	t := time.NewTimer(m.jitter(m.o.Interval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := m.Refresh(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if failures > 0 {
				m.log().Info("secrets: provider recovered", "provider", m.p.Name(), "failures", failures)
			}
			failures = 0
			t.Reset(m.jitter(m.o.Interval))
			continue
		}
		failures++
		wait := m.backoff(failures, err)
		if herr := m.Healthy(); herr != nil {
			m.log().Error("secrets: refresh failed, past max staleness", "provider", m.p.Name(), "failures", failures, "retry_in", wait, "err", err, "health", herr)
		} else {
			m.log().Warn("secrets: refresh failed, serving last good snapshot", "provider", m.p.Name(), "failures", failures, "retry_in", wait, "err", err)
		}
		t.Reset(wait)
	}
}

// Refresh fetches, validates and publishes one snapshot now, then calls the
// hooks of every key whose value changed. On error the previous snapshot
// stays published. The poll loop calls it; so can a future webhook.
func (m *Manager) Refresh(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	m.mu.RLock()
	prev := m.snap
	m.mu.RUnlock()

	fctx, cancel := context.WithTimeout(ctx, m.o.Timeout)
	next, err := m.p.Fetch(fctx, prev)
	cancel()
	if err == nil {
		next = m.filter(next)
		err = m.validate(next)
	}
	now := time.Now()
	if err != nil {
		err = fmt.Errorf("secrets: refresh from %s: %w", m.p.Name(), err)
		m.mu.Lock()
		m.lastAttempt, m.lastErr = now, err
		m.failures++
		m.mu.Unlock()
		return err
	}

	changed := diff(prev, next)
	m.mu.Lock()
	m.snap, m.loaded = next, true
	m.lastSuccess, m.lastAttempt, m.lastErr, m.failures = now, now, nil, 0
	if len(changed) > 0 {
		m.gen++
	}
	gen := m.gen
	var calls []hookID
	for _, name := range changed {
		for i := range m.hooks[name] {
			calls = append(calls, hookID{name, i})
		}
	}
	for id := range m.failed {
		if !slices.Contains(changed, id.name) {
			calls = append(calls, id)
		}
	}
	hooks := make([]Hook, len(calls))
	for i, id := range calls {
		hooks[i] = m.hooks[id.name][id.i]
	}
	m.mu.Unlock()

	if len(changed) > 0 && gen > 1 {
		m.log().Info("secrets: rotated", "provider", m.p.Name(), "generation", gen, "changed", changed)
	}
	for i, id := range calls {
		v, _ := next.Get(id.name)
		m.runHook(id, hooks[i], v)
	}
	return nil
}

// OnChange registers fn for name. If a snapshot is already published, fn is
// called right away with the current value (zero if absent), so wiring code
// builds its first client the same way it rebuilds later ones.
func (m *Manager) OnChange(name string, fn Hook) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.mu.Lock()
	m.hooks[name] = append(m.hooks[name], fn)
	id := hookID{name, len(m.hooks[name]) - 1}
	loaded := m.loaded
	v, _ := m.snap.Get(name)
	m.mu.Unlock()
	if loaded {
		m.runHook(id, fn, v)
	}
}

// runHook calls fn and records whether it must be retried. Caller holds refreshMu.
func (m *Manager) runHook(id hookID, fn Hook, v Secret) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return fn(v)
	}()
	if err == nil {
		delete(m.failed, id)
		return
	}
	m.failed[id] = true
	m.mu.Lock()
	m.hookErrors++
	m.mu.Unlock()
	m.log().Error("secrets: consumer rejected change, keeping its previous client; retrying after the next fetch", "key", id.name, "err", err)
}

// Get returns the current value of name (zero if the snapshot lacks it), or
// ErrNotLoaded / ErrStale.
func (m *Manager) Get(name string) (Secret, error) {
	if err := m.Healthy(); err != nil {
		return Secret{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, _ := m.snap.Get(name)
	return v, nil
}

// Healthy is nil while a snapshot is published and younger than
// MaxStaleness, else ErrNotLoaded or an error wrapping ErrStale. Static
// providers never go stale.
func (m *Manager) Healthy() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.loaded {
		return ErrNotLoaded
	}
	if age := time.Since(m.lastSuccess); !m.static && age > m.o.MaxStaleness {
		return fmt.Errorf("%w: last good %s snapshot is %s old (limit %s)", ErrStale, m.p.Name(), age.Round(time.Second), m.o.MaxStaleness)
	}
	return nil
}

// Status returns counters and timestamps for health checks and metrics.
func (m *Manager) Status() Status {
	stale := errors.Is(m.Healthy(), ErrStale)
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{
		Provider: m.p.Name(), Loaded: m.loaded, Generation: m.gen,
		Revision: m.snap.Revision(), Keys: m.snap.Len(),
		LastSuccess: m.lastSuccess, LastAttempt: m.lastAttempt,
		ConsecutiveFailures: m.failures, HookErrors: m.hookErrors, Stale: stale,
	}
	if m.lastErr != nil {
		st.LastError = m.lastErr.Error()
	}
	return st
}

func (m *Manager) filter(s Snapshot) Snapshot {
	if len(m.o.Keys) == 0 {
		return s
	}
	out := Snapshot{values: make(map[string]Secret, len(m.o.Keys)), revision: s.revision}
	for _, k := range m.o.Keys {
		if v, ok := s.values[k]; ok {
			out.values[k] = v
		}
	}
	return out
}

func (m *Manager) validate(s Snapshot) error {
	var missing []string
	for _, k := range m.o.Required {
		if v, ok := s.Get(k); !ok || v.IsZero() {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing required %v", ErrInvalid, missing)
	}
	if m.o.Validate != nil {
		if err := m.o.Validate(s); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// backoff is the wait after the n-th consecutive failure: 5s doubling, capped
// at MaxBackoff, at least the provider's Retry-After (also capped), jittered.
func (m *Manager) backoff(n int, err error) time.Duration {
	d := min(minBackoff, m.o.Interval)
	for i := 1; i < n && d < m.o.MaxBackoff; i++ {
		d *= 2
	}
	if ra, ok := errors.AsType[RetryAfter](err); ok && ra.RetryAfter() > d {
		d = ra.RetryAfter()
	}
	return m.jitter(min(d, m.o.MaxBackoff))
}

func (m *Manager) jitter(d time.Duration) time.Duration {
	f := 1 + m.o.Jitter*(2*rand.Float64()-1)
	return time.Duration(float64(d) * f)
}

func (m *Manager) log() *slog.Logger {
	if m.o.Logger != nil {
		return m.o.Logger
	}
	return slog.Default()
}

// diff returns the sorted names whose value was added, changed or removed.
func diff(prev, next Snapshot) []string {
	var out []string
	for k, v := range next.values {
		if old, ok := prev.values[k]; !ok || !old.Equal(v) {
			out = append(out, k)
		}
	}
	for k := range prev.values {
		if _, ok := next.values[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
