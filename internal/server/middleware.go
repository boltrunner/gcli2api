package server

import (
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher by forwarding to the underlying ResponseWriter
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{
			ResponseWriter: w,
			statusCode:     200, // default status code
		}
		next.ServeHTTP(wrapped, r)
		dur := time.Since(start)
		logrus.Infof("%s %s %d %s", r.Method, r.URL.Path, wrapped.statusCode, dur)
	})
}

// withRecover adds a panic recovery layer to prevent leaking stack traces
// and to ensure a clean 500 response is sent to the client.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// Minimal error details; avoid stack traces or sensitive info
				logrus.WithField("path", r.URL.Path).Errorf("panic recovered: %v", rec)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withConcurrencyLimit adds simple server-wide concurrency limiting.
func (s *Server) withConcurrencyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		}
	})
}
