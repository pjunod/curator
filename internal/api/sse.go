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
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(s.streamWriteTimeout)); err != nil {
		writeError(w, http.StatusInternalServerError, "streaming unsupported: write deadline unavailable")
		return
	}
	writeFailed := false
	defer func() {
		// Keep final response flushing bounded, but do not extend a failed
		// write's expired deadline. Successful frames clear it while idle;
		// HTTP/2 deadlines can terminate a stream even without another write.
		if !writeFailed {
			_ = controller.SetWriteDeadline(time.Now().Add(s.streamWriteTimeout))
		}
	}()
	writeFrame := func(frame string) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(s.streamWriteTimeout)); err != nil {
			writeFailed = true
			return false
		}
		if _, err := fmt.Fprint(w, frame); err != nil {
			writeFailed = true
			return false
		}
		if err := controller.Flush(); err != nil {
			writeFailed = true
			return false
		}
		if err := controller.SetWriteDeadline(time.Time{}); err != nil {
			writeFailed = true
			return false
		}
		return true
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
	if !writeFrame("retry: 3000\n\n") {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.streamsCtx.Done():
			return
		case <-heartbeat.C:
			if !s.ownerStreamAuthorized(r) {
				return
			}
			if !writeFrame(": ping\n\n") {
				return
			}
		case e, open := <-events:
			if !s.ownerStreamAuthorized(r) {
				return
			}
			if !open {
				return // bus closed; client will reconnect
			}
			payload, err := json.Marshal(sseEnvelope{Type: e.EventType(), TS: time.Now(), Payload: e})
			if err != nil {
				s.deps.Log.Warn("sse: could not marshal event", "event", e.EventType(), "err", err)
				continue
			}
			if !writeFrame(fmt.Sprintf("data: %s\n\n", payload)) {
				return
			}
		}
	}
}
