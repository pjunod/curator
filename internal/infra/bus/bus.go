// Package bus is Monarr's in-process typed event bus: a small hand-rolled
// fan-out over channels (blueprint §5). It feeds notifiers, the SSE stream to
// the UI, and (later) the history writer. No external broker — an event bus
// is a pattern, not a deployment.
//
// Delivery semantics: Publish never blocks. Each subscriber has a buffered
// channel; if a subscriber falls behind and its buffer fills, events are
// dropped for that subscriber and a warning is logged. Consumers that must
// not miss events (e.g. the history writer) will get a durable path of their
// own — the bus is for live fan-out.
package bus

import (
	"log/slog"
	"sync"
)

// Event is anything that can be published. EventType returns a stable,
// dot-separated identifier (e.g. "health.changed", "task.completed") used
// for filtering and for the SSE wire format.
type Event interface {
	EventType() string
}

type subscriber struct {
	ch   chan Event
	once sync.Once
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.ch) })
}

// Bus fans events out to subscribers. The zero value is not usable; call New.
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]*subscriber
	nextID int
	closed bool
	log    *slog.Logger
}

// New returns a ready Bus. log may be nil.
func New(log *slog.Logger) *Bus {
	if log == nil {
		log = slog.Default()
	}
	return &Bus{subs: make(map[int]*subscriber), log: log}
}

// Publish delivers e to every current subscriber without blocking. Events
// published after Close are discarded.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, s := range b.subs {
		select {
		case s.ch <- e:
		default:
			b.log.Warn("bus: subscriber buffer full, dropping event", "event", e.EventType())
		}
	}
}

// SubscribeAll returns a channel receiving every published event, and a
// cancel function that unsubscribes and closes the channel. buffer must be
// > 0. After Close, the returned channel is already closed.
func (b *Bus) SubscribeAll(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 16
	}
	s := &subscriber{ch: make(chan Event, buffer)}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		s.close()
		return s.ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = s
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		// Safe: Publish sends only under RLock while the subscriber is still
		// in the map; once removed under the write lock, no send can race
		// this close.
		s.close()
	}
	return s.ch, cancel
}

// Subscribe returns a channel receiving only events of concrete type T.
// The forwarding goroutine exits (and closes the returned channel) when the
// subscription is canceled or the bus is closed.
func Subscribe[T Event](b *Bus, buffer int) (<-chan T, func()) {
	raw, cancel := b.SubscribeAll(buffer)
	out := make(chan T, buffer)
	go func() {
		defer close(out)
		for e := range raw {
			t, ok := e.(T)
			if !ok {
				continue
			}
			select {
			case out <- t:
			default:
				b.log.Warn("bus: typed subscriber buffer full, dropping event", "event", e.EventType())
			}
		}
	}()
	return out, cancel
}

// Close unsubscribes everyone and closes their channels. Publish becomes a
// no-op. Close is idempotent.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subs {
		delete(b.subs, id)
		s.close()
	}
}
