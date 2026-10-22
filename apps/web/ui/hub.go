package ui

import "sync"

// userEvent is pushed to every open dashboard stream of a user.
type userEvent struct {
	User User
	// Tab is the dashboard tab that caused the change ("" when unknown).
	Tab     string
	Deleted bool
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

// subscribe returns a channel of events for userID and its cancel func.
func (h *hub) subscribe(userID string) (<-chan userEvent, func()) {
	ch := make(chan userEvent, 4)
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[chan userEvent]struct{}{}
	}
	h.subs[userID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[userID], ch)
		if len(h.subs[userID]) == 0 {
			delete(h.subs, userID)
		}
		h.mu.Unlock()
	}
}

// publish delivers ev without blocking; a stream that is behind skips it and
// catches up on the next event.
func (h *hub) publish(userID string, ev userEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[userID] {
		select {
		case ch <- ev:
		default:
		}
	}
}
