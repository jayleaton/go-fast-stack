package secrets

import "context"

// Env serves values captured from the process environment at startup
// (config.Load passes them in). It is Static: the Manager loads it once and
// never polls, because a running process's environment cannot change from
// outside. On Railway, changing an Env value means a redeploy; use a remote
// provider (Doppler) for live rotation.
type Env struct{ snap Snapshot }

var (
	_ Provider = (*Env)(nil)
	_ Static   = (*Env)(nil)
)

// NewEnv copies values. Empty values are kept, so Required still rejects them.
func NewEnv(values map[string]string) *Env { return &Env{NewSnapshot(values, "")} }

// Name returns "env".
func (*Env) Name() string { return "env" }

// Static marks Env as startup-only.
func (*Env) Static() {}

// Fetch returns the captured values.
func (e *Env) Fetch(context.Context, Snapshot) (Snapshot, error) { return e.snap, nil }
