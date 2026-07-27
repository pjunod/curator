package api

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Caller is another application that has called Monarr, and when it last did.
//
// The Connections panel answers "are the three apps talking right now", and
// until this existed it only answered it in one direction: what Monarr
// reaches out to. The plurx side of the pipeline is entirely INBOUND — plurx
// pushes watch state and reads the calendar — so a plurx configured
// perfectly, calling every few minutes, appeared nowhere at all. "I set it
// up and I see no status" is the correct reaction to that, and the answer is
// not a better explanation, it is a row.
type Caller struct {
	// Name is the product token from the User-Agent ("plurx"), which is what
	// somebody recognizes. Same idea as nzbd's `picked_up_by`.
	Name     string    `json:"name"`
	Agent    string    `json:"agent"`
	LastSeen time.Time `json:"lastSeen"`
	LastPath string    `json:"lastPath"`
	Calls    int64     `json:"calls"`
}

// CallerRegistry remembers who has called, in memory.
//
// In memory on purpose. This answers "is it talking to me *now*", and a
// restart genuinely resets that: a caller that has not been heard from since
// Monarr came up is exactly what the panel should show as quiet. Persisting
// it would let a plurx that has been off for a week keep looking present.
type CallerRegistry struct {
	mu sync.Mutex
	by map[string]*Caller
}

func NewCallerRegistry() *CallerRegistry {
	return &CallerRegistry{by: map[string]*Caller{}}
}

// Note records one inbound call from a machine client.
func (r *CallerRegistry) Note(agent, path string) {
	name := productToken(agent)
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.by[name]
	if !ok {
		c = &Caller{Name: name}
		r.by[name] = c
	}
	c.Agent = agent
	c.LastSeen = time.Now()
	c.LastPath = path
	c.Calls++
}

// List returns every known caller, most recently seen first.
func (r *CallerRegistry) List() []Caller {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Caller, 0, len(r.by))
	for _, c := range r.by {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// productToken pulls "plurx" out of "plurx/0.4.1 (something)".
//
// Browsers are deliberately excluded: this list is about other applications,
// and one row per Chrome version would bury the one row that matters.
func productToken(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return ""
	}
	first := agent
	if i := strings.IndexAny(first, " \t"); i > 0 {
		first = first[:i]
	}
	name := first
	if i := strings.Index(name, "/"); i > 0 {
		name = name[:i]
	}
	switch strings.ToLower(name) {
	case "mozilla", "chrome", "safari", "firefox", "edge", "opera", "webkit":
		return ""
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}

// noteCallers records machine callers — anything presenting an API key.
//
// Keyed off the key rather than the path: a session cookie is a person with
// a browser open, and a person is not a connection. Everything that
// authenticates with X-Api-Key is another program, which is precisely the
// set worth showing.
func (s *Server) noteCallers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Callers != nil && apiKeyOf(r) != "" {
			s.deps.Callers.Note(r.UserAgent(), r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func apiKeyOf(r *http.Request) string {
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	return r.URL.Query().Get("apikey")
}
