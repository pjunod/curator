// Package maintenance coordinates reversible deployment admission separately
// from terminal application cancellation. It never cancels admitted work.
package maintenance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

var (
	ErrOwner = errors.New("maintenance owner or generation does not match")
	ErrToken = errors.New("owner token must be 32 random bytes encoded as 64 hexadecimal characters")
)

type Work struct {
	ID        uint64    `json:"id"`
	Kind      string    `json:"kind"`
	StartedAt time.Time `json:"startedAt"`
}

type Status struct {
	Phase          string     `json:"phase"`
	Generation     string     `json:"generation"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	ElapsedSeconds float64    `json:"elapsedSeconds"`
	Active         int        `json:"active"`
	Work           []Work     `json:"work"`
}

type tokenKey struct{}
type token struct {
	coordinator *Coordinator
	id          uint64
}

type Coordinator struct {
	mu         sync.Mutex
	paused     bool
	terminal   bool
	generation string
	owner      [32]byte
	started    time.Time
	next       uint64
	active     map[uint64]Work
	changed    chan struct{}
}

func New() *Coordinator {
	return &Coordinator{active: make(map[uint64]Work), changed: make(chan struct{})}
}

func (c *Coordinator) signalLocked() { close(c.changed); c.changed = make(chan struct{}) }

func (c *Coordinator) TryEnter(ctx context.Context, kind string) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enterLocked(ctx, kind)
}

func (c *Coordinator) enterLocked(ctx context.Context, kind string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	parent, _ := ctx.Value(tokenKey{}).(token)
	_, live := c.active[parent.id]
	if (c.paused || c.terminal) && !(parent.coordinator == c && live) {
		return ctx, nil, ports.ErrMaintenance
	}
	c.next++
	id := c.next
	c.active[id] = Work{ID: id, Kind: kind, StartedAt: time.Now().UTC()}
	c.signalLocked()
	return context.WithValue(ctx, tokenKey{}, token{c, id}), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.active[id]; ok {
			delete(c.active, id)
			c.signalLocked()
		}
	}, nil
}

func (c *Coordinator) Enter(ctx context.Context, kind string) (context.Context, func(), error) {
	for {
		c.mu.Lock()
		next, done, err := c.enterLocked(ctx, kind)
		changed, terminal := c.changed, c.terminal
		c.mu.Unlock()
		if err == nil || terminal || !errors.Is(err, ports.ErrMaintenance) {
			return next, done, err
		}
		select {
		case <-ctx.Done():
			return ctx, nil, ctx.Err()
		case <-changed:
		}
	}
}

// Start is idempotent for an owner, including when its first HTTP response was
// lost. The opaque generation prevents a delayed abort from reopening a new drain.
func (c *Coordinator) Start(owner string) (Status, error) {
	decoded, err := hex.DecodeString(owner)
	if err != nil || len(decoded) != 32 {
		return Status{}, ErrToken
	}
	hash := sha256.Sum256(decoded)
	var generation [16]byte
	if _, err = rand.Read(generation[:]); err != nil {
		return Status{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal {
		return c.statusLocked(), ports.ErrMaintenance
	}
	if c.paused {
		if subtle.ConstantTimeCompare(hash[:], c.owner[:]) != 1 {
			return c.statusLocked(), ErrOwner
		}
		return c.statusLocked(), nil
	}
	c.owner = hash
	c.generation = hex.EncodeToString(generation[:])
	c.started = time.Now().UTC()
	c.paused = true
	c.signalLocked()
	return c.statusLocked(), nil
}

func (c *Coordinator) Abort(owner, generation string) (Status, error) {
	decoded, err := hex.DecodeString(owner)
	if err != nil || len(decoded) != 32 {
		return Status{}, ErrToken
	}
	hash := sha256.Sum256(decoded)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal || !c.paused || c.generation != generation || subtle.ConstantTimeCompare(hash[:], c.owner[:]) != 1 {
		return c.statusLocked(), ErrOwner
	}
	c.paused = false
	c.signalLocked()
	return c.statusLocked(), nil
}

// Close permanently closes root admission. Existing owned children still join
// their parents; no abort can reopen this coordinator.
func (c *Coordinator) Close() { c.mu.Lock(); defer c.mu.Unlock(); c.terminal = true; c.signalLocked() }

func (c *Coordinator) Status() Status { c.mu.Lock(); defer c.mu.Unlock(); return c.statusLocked() }

func (c *Coordinator) statusLocked() Status {
	s := Status{Phase: "accepting", Generation: c.generation, Active: len(c.active), Work: make([]Work, 0, len(c.active))}
	if c.paused {
		s.Phase = "draining"
		if len(c.active) == 0 {
			s.Phase = "drained"
		}
	}
	if c.terminal {
		s.Phase = "stopping"
	}
	if !c.started.IsZero() {
		started := c.started
		s.StartedAt = &started
		s.ElapsedSeconds = time.Since(started).Seconds()
	}
	for _, w := range c.active {
		s.Work = append(s.Work, w)
	}
	sort.Slice(s.Work, func(i, j int) bool { return s.Work[i].ID < s.Work[j].ID })
	return s
}
