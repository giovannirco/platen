package hub

import (
	"encoding/json"
	"sync"
	"time"
)

// Event is something that happened, sent to listeners (the web UI's live updates).
type Event struct {
	// Type is one of: job, scan, scan.deleted.
	Type string    `json:"type"`
	ID   string    `json:"id,omitempty"`
	At   time.Time `json:"at"`
	// Data is the job or scan as JSON, as it was when the event happened.
	Data json.RawMessage `json:"data,omitempty"`
}

// broker fans events out to subscribers. Slow subscribers lose events rather
// than block the hub.
type broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newBroker() *broker { return &broker{subs: map[chan Event]struct{}{}} }

// publish sends an event to every subscriber. data is encoded here, in the
// caller's goroutine, so listeners never read a value that is still changing.
func (b *broker) publish(kind, id string, data any) {
	e := Event{Type: kind, ID: id, At: time.Now()}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return
		}
		e.Data = raw
	}
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
