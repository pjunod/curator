package api

import (
	"context"
	"net/http"
)

// StopStreams interrupts idle event streams. A stream blocked writing to a
// stalled peer also has its own network write deadline.
func (s *Server) StopStreams() { s.cancelStreams() }

// BeginShutdown closes request/background admission before any Wait can run.
// Ordinary requests already admitted retain their HTTP shutdown grace.
func (s *Server) BeginShutdown() {
	s.lifecycleMu.Lock()
	s.closing = true
	s.lifecycleMu.Unlock()
	s.StopStreams()
	s.cancelBackground()
}

// CancelRequests is the second shutdown stage, after HTTP grace expires.
// Cancellation is a request to stop, not evidence that a handler has stopped.
func (s *Server) CancelRequests() { s.cancelRequests() }

// Wait joins actual handler/background completion before their dependencies
// close. BeginShutdown must precede it so registration cannot race the wait.
func (s *Server) Wait() {
	s.handlers.Wait()
	s.background.Wait()
}

func (s *Server) trackRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lifecycleMu.Lock()
		if s.closing {
			s.lifecycleMu.Unlock()
			writeError(w, http.StatusServiceUnavailable, "server is shutting down")
			return
		}
		s.handlers.Add(1)
		s.lifecycleMu.Unlock()
		defer s.handlers.Done()

		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(s.requestsCtx, cancel)
		defer stop()
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// startBackground serializes admission and registration with shutdown.
func (s *Server) startBackground(work func(context.Context)) bool {
	s.lifecycleMu.Lock()
	if s.closing || s.backgroundCtx.Err() != nil {
		s.lifecycleMu.Unlock()
		return false
	}
	s.background.Add(1)
	s.lifecycleMu.Unlock()
	go func() { defer s.background.Done(); work(s.backgroundCtx) }()
	return true
}
