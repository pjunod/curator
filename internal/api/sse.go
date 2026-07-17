package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sseEnvelope is the JSON wire format for every SSE message.
type sseEnvelope struct {
	Type    string    `json:"type"`
	TS      time.Time `json:"ts"`
	Payload any       `json:"payload"`
}

// StreamEvents implements GET /events: every bus event as an SSE data line,
// with periodic comment heartbeats so proxies keep the connection alive.
func (s *Server) StreamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	events, cancel := s.deps.Bus.SubscribeAll(64)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable buffering in nginx-style proxies
	w.WriteHeader(http.StatusOK)

	// Ask EventSource clients to wait 3s before reconnecting.
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, open := <-events:
			if !open {
				return // bus closed; client will reconnect
			}
			payload, err := json.Marshal(sseEnvelope{Type: e.EventType(), TS: time.Now(), Payload: e})
			if err != nil {
				s.deps.Log.Warn("sse: could not marshal event", "event", e.EventType(), "err", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}
