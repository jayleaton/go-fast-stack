package ui

import "sync"

// userEvent is pushed to every open dashboard stream of a user.
type userEvent struct {
	// User is set when the profile changed.
	User User
	// Tab is the dashboard tab that caused the change ("" when unknown).
	Tab     string
	Deleted bool
	// RevokedSession ends one session; KeepSession, when set, ends all others.
	RevokedSession string
	KeepSession    string
}

// revokes reports whether ev signs out the session with id.
func (ev userEvent) revokes(id string) bool {
	return (ev.RevokedSession != "" && ev.RevokedSession == id) || (ev.KeepSession != "" && ev.KeepSession != id)
}

// hub fans user events out to the live dashboard streams of this process.
// With several instances, swap it for Postgres LISTEN/NOTIFY or similar.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan userEvent]struct{}
}

func newHub() *hub {
	return &hub{subs: map[string]map[chan userEvent]struct{}{}}
}

// subscriberBuffer is how many events a slow stream may fall behind before
// publish starts skipping them for it.
const subscriberBuffer = 4

// subscribe returns a channel of events for userID and its cancel func.
func (hub *hub) subscribe(userID string) (<-chan userEvent, func()) {
	ch := make(chan userEvent, subscriberBuffer)
	hub.mu.Lock()
	if hub.subs[userID] == nil {
		hub.subs[userID] = map[chan userEvent]struct{}{}
	}
	hub.subs[userID][ch] = struct{}{}
	hub.mu.Unlock()
	return ch, func() {
		hub.mu.Lock()
		delete(hub.subs[userID], ch)
		if len(hub.subs[userID]) == 0 {
			delete(hub.subs, userID)
		}
		hub.mu.Unlock()
	}
}

// publish delivers ev without blocking; a stream that is behind skips it and
// catches up on the next event.
func (hub *hub) publish(userID string, ev userEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for ch := range hub.subs[userID] {
		select {
		case ch <- ev:
		default:
		}
	}
}
