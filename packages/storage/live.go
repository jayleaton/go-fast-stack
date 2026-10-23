package storage

import (
	"context"
	"sync"
	"sync/atomic"
)

// Live is a Client whose bucket and credentials can change while the app
// runs (a key rotation, a bucket move). Swap builds the new Client and probes
// it with one real call before switching atomically; until then, and if the
// probe fails, every call goes to the old one. Safe for concurrent use.
type Live struct {
	current atomic.Pointer[Client]
	swapMu  sync.Mutex // serializes Swap
}

// NewLive starts on client (built and probed by the caller).
func NewLive(client *Client) *Live {
	live := &Live{}
	live.current.Store(client)
	return live
}

// Swap switches to cfg once a Client for it passes Probe. On any error the
// current Client stays and is returned unchanged; the error names no values.
// Requests already running finish on the old Client; its idle connections
// are closed.
func (live *Live) Swap(ctx context.Context, cfg Config) error {
	live.swapMu.Lock()
	defer live.swapMu.Unlock()
	next, err := New(cfg)
	if err != nil {
		return err
	}
	if err := next.Probe(ctx); err != nil {
		next.httpClient.CloseIdleConnections()
		return err
	}
	old := live.current.Swap(next)
	if old.httpClient != next.httpClient { // a shared Config.HTTPClient stays open
		old.httpClient.CloseIdleConnections()
	}
	return nil
}

// Put stores data under key on the current Client (see Client.Put).
func (live *Live) Put(ctx context.Context, key string, data []byte, contentType string) error {
	return live.current.Load().Put(ctx, key, data, contentType)
}

// Get opens key on the current Client (see Client.Get).
func (live *Live) Get(ctx context.Context, key string) (*Object, error) {
	return live.current.Load().Get(ctx, key)
}

// Delete removes key on the current Client (see Client.Delete).
func (live *Live) Delete(ctx context.Context, key string) error {
	return live.current.Load().Delete(ctx, key)
}

// Probe checks the current Client (see Client.Probe).
func (live *Live) Probe(ctx context.Context) error { return live.current.Load().Probe(ctx) }
