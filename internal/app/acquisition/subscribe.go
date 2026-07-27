package acquisition

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// The push channel (nzbd/docs/INTEGRATION_PLAN.md, master plan §5.2).
//
// A client in `push` mode gets a goroutine holding its event stream open,
// so a finished download is acted on the moment it finishes instead of up
// to 30 seconds later. Three rules shape everything below:
//
//  1. **Push is never load-bearing.** The 30 s poll keeps running in push
//     mode. It is what notices a stream that died quietly, and it is the
//     entire behavior if push is off or the daemon is old. If a design
//     choice here would make poll mode worse, it is the wrong choice.
//  2. **One reconciler.** Events do not get their own state machine; they
//     call the same `reconcileDownload` the poll does. Two state machines
//     over one download eventually disagree, and then nobody can say which
//     is right.
//  3. **Progress is not a decision.** nzbd ticks at 1 Hz. Writing that
//     through to SQLite would be 86,400 writes a day per download to move
//     a progress bar, so progress is coalesced in memory and flushed on a
//     change that matters.

// progressFlush bounds how often a percentage-only change reaches the
// database. State changes are never delayed by it.
const progressFlush = 10 * time.Second

// ClientLink is the live state of one push subscription, for the UI's
// "are these two actually talking right now" question.
type ClientLink struct {
	ClientID  int64     `json:"clientId"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since,omitempty"`
	LastEvent time.Time `json:"lastEvent,omitempty"`
	LastSeq   uint64    `json:"lastSeq,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

type linkState struct {
	mu   sync.Mutex
	link ClientLink
}

// Subscriptions returns the current state of every push subscription.
func (s *Service) Subscriptions() []ClientLink {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	out := make([]ClientLink, 0, len(s.links))
	for _, l := range s.links {
		l.mu.Lock()
		out = append(out, l.link)
		l.mu.Unlock()
	}
	return out
}

func (s *Service) linkFor(cfg ports.ClientConfig) *linkState {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	if s.links == nil {
		s.links = map[int64]*linkState{}
	}
	l, ok := s.links[cfg.ID]
	if !ok {
		l = &linkState{link: ClientLink{ClientID: cfg.ID, Name: clientLabel(cfg), Mode: "push"}}
		s.links[cfg.ID] = l
	}
	return l
}

// RunSubscribers keeps one subscription per enabled push-mode client for
// as long as ctx lives. It is a supervisor, not a one-shot: clients are
// re-read periodically so switching a client to push in the settings UI
// takes effect without a restart.
func (s *Service) RunSubscribers(ctx context.Context) {
	running := map[int64]context.CancelFunc{}
	defer func() {
		for _, cancel := range running {
			cancel()
		}
	}()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		s.syncSubscribers(ctx, running)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// syncSubscribers starts subscriptions for clients that want one and stops
// those that no longer do.
func (s *Service) syncSubscribers(ctx context.Context, running map[int64]context.CancelFunc) {
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		s.log.Warn("subscribe: could not list clients", "err", err)
		return
	}
	want := map[int64]ports.ClientConfig{}
	for _, cfg := range clients {
		if cfg.Enabled && cfg.Mode == "push" {
			want[cfg.ID] = cfg
		}
	}
	for id, cancel := range running {
		if _, ok := want[id]; !ok {
			cancel()
			delete(running, id)
			s.linksMu.Lock()
			delete(s.links, id)
			s.linksMu.Unlock()
		}
	}
	for id, cfg := range want {
		if _, ok := running[id]; ok {
			continue
		}
		sub, ok := s.newClient(cfg).(ports.Subscriber)
		if !ok {
			// Configured for push against a client that cannot stream.
			// Say so once and leave it polling rather than pretending.
			s.log.Warn("subscribe: client cannot push; staying on the poll",
				"client", clientLabel(cfg), "type", cfg.Type)
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		running[id] = cancel
		go s.subscribeTo(cctx, cfg, sub)
	}
}

// subscribeTo consumes one client's event stream until ctx ends.
func (s *Service) subscribeTo(ctx context.Context, cfg ports.ClientConfig, sub ports.Subscriber) {
	link := s.linkFor(cfg)
	events, err := sub.Subscribe(ctx)
	if err != nil {
		s.noteLink(link, func(l *ClientLink) { l.Connected, l.LastError = false, err.Error() })
		s.log.Warn("subscribe: could not open the event stream",
			"client", clientLabel(cfg), "err", err)
		return
	}
	s.noteLink(link, func(l *ClientLink) {
		l.Connected, l.Since, l.LastError = true, time.Now(), ""
	})
	s.log.Info("subscribe: live on the event stream", "client", clientLabel(cfg))

	pending := map[int64]float64{}
	flushed := time.Now()
	for {
		select {
		case <-ctx.Done():
			s.noteLink(link, func(l *ClientLink) { l.Connected = false })
			return
		case ev, open := <-events:
			if !open {
				s.noteLink(link, func(l *ClientLink) { l.Connected = false })
				s.log.Info("subscribe: event stream ended", "client", clientLabel(cfg))
				return
			}
			s.noteLink(link, func(l *ClientLink) {
				l.LastEvent = time.Now()
				if ev.Seq > 0 {
					l.LastSeq = ev.Seq
				}
			})
			// An event IS contact, and for a push-mode client it is the
			// only kind there is between polls. Without this the contact
			// clock would age out on a client that is working perfectly.
			s.noteContact(cfg.ID, nil)
			s.handleEvent(ctx, cfg, ev, pending, &flushed)
		}
	}
}

// handleEvent turns one pushed event into the same reconcile the poll
// performs — or, for progress, into a coalesced in-memory update.
func (s *Service) handleEvent(ctx context.Context, cfg ports.ClientConfig, ev ports.ClientEvent, pending map[int64]float64, flushed *time.Time) {
	if ev.Kind == ports.EventReset {
		// We may have missed events. The poll is the reconcile, so ask it
		// to run now instead of waiting out its interval.
		s.log.Info("subscribe: stream gap; reconciling by poll", "client", clientLabel(cfg))
		if err := s.RefreshQueue(ctx); err != nil {
			s.log.Warn("subscribe: reconcile poll failed", "err", err)
		}
		return
	}
	if ev.Handle == "" {
		return
	}
	dl, ok := s.downloadByHandle(ctx, cfg.ID, ev.Handle)
	if !ok {
		return // something in the client that Monarr did not grab
	}

	if ev.Kind == ports.EventProgress {
		// Coalesce: hold the newest percentage and write it at most every
		// progressFlush, unless the state itself changed — a state change
		// is a decision and must not wait behind a progress bar.
		if dl.State == "downloading" && ev.Status.State == ports.StateDownloading {
			pending[dl.ID] = ev.Status.Progress
			if time.Since(*flushed) < progressFlush {
				return
			}
			*flushed = time.Now()
			for id, p := range pending {
				_ = s.db.UpdateDownloadState(ctx, id, "downloading", p, "")
				delete(pending, id)
			}
			return
		}
	}
	delete(pending, dl.ID) // a decision supersedes any held percentage

	source := "push"
	if ev.Seq > 0 {
		source = fmt.Sprintf("event %d", ev.Seq)
	}
	if ev.Kind == ports.EventStage {
		// A stage is progress with a name. It updates the trace's detail
		// but must not be mistaken for completion.
		s.reconcileDownload(ctx, dl, cfg, ev.Status, source)
		return
	}
	s.reconcileDownload(ctx, dl, cfg, ev.Status, source)
}

// downloadByHandle finds the active download a pushed event refers to.
func (s *Service) downloadByHandle(ctx context.Context, clientID int64, h ports.Handle) (sqlite.Download, bool) {
	active, err := s.db.ListActiveDownloads(ctx)
	if err != nil {
		return sqlite.Download{}, false
	}
	for _, dl := range active {
		if dl.ClientID == clientID && dl.Handle == string(h) {
			return dl, true
		}
	}
	return sqlite.Download{}, false
}

func (s *Service) noteLink(l *linkState, f func(*ClientLink)) {
	l.mu.Lock()
	f(&l.link)
	l.mu.Unlock()
}
