package hub

import (
	"sync"
	"time"
)

// Event is something that happened, sent to listeners (the web UI's live updates).
type Event struct {
	// Type is one of: printer, job, scan, scan.deleted.
	Type string    `json:"type"`
	ID   string    `json:"id,omitempty"`
	At   time.Time `json:"at"`
	Data any       `json:"data,omitempty"`
}

// broker fans events out to subscribers. Slow subscribers lose events rather
// than block the hub.
type broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newBroker() *broker { return &broker{subs: map[chan Event]struct{}{}} }

func (b *broker) publish(e Event) {
	e.At = time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a channel of events and a function that ends the subscription.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	h.events.mu.Lock()
	h.events.subs[ch] = struct{}{}
	h.events.mu.Unlock()
	return ch, func() {
		h.events.mu.Lock()
		delete(h.events.subs, ch)
		h.events.mu.Unlock()
	}
}
