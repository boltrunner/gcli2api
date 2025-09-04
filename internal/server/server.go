package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gcli2api/internal/codeassist"
	"gcli2api/internal/config"
	"gcli2api/internal/gemini"

	// "gcli2api/internal/utils"

	"github.com/sirupsen/logrus"
	"github.com/tiktoken-go/tokenizer"
)

var (
	modelPathUnary  = regexp.MustCompile(`^/v1beta/models/([^/]+):generateContent$`)
	modelPathStream = regexp.MustCompile(`^/v1beta/models/([^/]+):streamGenerateContent$`)
)

// CodeAssist abstracts the client for easier testing.
type CodeAssist interface {
	GenerateContent(ctx context.Context, model, project string, req gemini.GeminiRequest) (*gemini.GeminiAPIResponse, error)
	GenerateContentStream(ctx context.Context, model, project string, req gemini.GeminiRequest) (<-chan gemini.GeminiAPIResponse, <-chan error)
}

type Server struct {
	cfg      config.Config
	httpCli  *http.Client
	caClient CodeAssist
	// sem is a simple semaphore for concurrency limiting
	sem chan struct{}
}

func New(cfg config.Config, httpCli *http.Client) *Server {
	// Apply safe defaults when fields are zero to match config.LoadConfig behavior
	if cfg.RequestMaxRetries == 0 {
		cfg.RequestMaxRetries = 3
	}
	if cfg.RequestBaseDelayMillis == 0 {
		cfg.RequestBaseDelayMillis = 1000
	}
	if cfg.RequestMaxBodyBytes == 0 {
		cfg.RequestMaxBodyBytes = 16 * 1024 * 1024
	}
	if cfg.MaxConcurrentRequests == 0 {
		cfg.MaxConcurrentRequests = 64
	}
	return &Server{
		cfg:      cfg,
		httpCli:  httpCli,
		caClient: codeassist.NewCaClient(httpCli, cfg.RequestMaxRetries, time.Duration(cfg.RequestBaseDelayMillis)*time.Millisecond),
		sem:      make(chan struct{}, cfg.MaxConcurrentRequests),
	}
}

// NewWithCAClient allows injecting a custom CodeAssist client (for tests).
func NewWithCAClient(cfg config.Config, ca CodeAssist) *Server {
	// Apply same defaults as New to ensure handlers work in tests with zero config
	if cfg.RequestMaxRetries == 0 {
		cfg.RequestMaxRetries = 3
	}
	if cfg.RequestBaseDelayMillis == 0 {
		cfg.RequestBaseDelayMillis = 1000
	}
	if cfg.RequestMaxBodyBytes == 0 {
		cfg.RequestMaxBodyBytes = 16 * 1024 * 1024
	}
	if cfg.MaxConcurrentRequests == 0 {
		cfg.MaxConcurrentRequests = 64
	}
	return &Server{cfg: cfg, caClient: ca, sem: make(chan struct{}, cfg.MaxConcurrentRequests)}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1beta/models", s.handleListModels)
	mux.HandleFunc("/v1beta/models/", s.handleModel)
	// Order: recover (outermost) -> logging -> concurrency limiter -> handlers
	return s.withRecover(s.withLogging(s.withConcurrencyLimit(mux)))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) authorize(r *http.Request) bool {
	key := s.cfg.AuthKey
	if key == "" {
		return true
	}
	if ah := r.Header.Get("Authorization"); ah != "" {
		const p = "Bearer "
		if strings.HasPrefix(ah, p) {
			// Constant-time comparison to mitigate timing attacks
			if 1 == subtle.ConstantTimeCompare([]byte(strings.TrimSpace(ah[len(p):])), []byte(key)) {
				return true
			}
		}
	}
	if h := r.Header.Get("x-goog-api-key"); h != "" {
		if 1 == subtle.ConstantTimeCompare([]byte(h), []byte(key)) {
			return true
		}
	}
	return false
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listModels())
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Path
	if m := modelPathUnary.FindStringSubmatch(path); m != nil {
		model := m[1]
		s.handleGenerateContent(model, w, r)
		return
	}
	if m := modelPathStream.FindStringSubmatch(path); m != nil {
		model := m[1]
		s.handleStreamGenerateContent(model, w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) validateModel(model string) bool {
	return gemini.IsSupportedModel(model)
}

func (s *Server) decodeGeminiRequest(r *http.Request) (gemini.GeminiRequest, error) {
	var req gemini.GeminiRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	req = gemini.NormalizeGeminiRequest(req)
	return req, nil
}

func (s *Server) handleGenerateContent(model string, w http.ResponseWriter, r *http.Request) {
	if !s.validateModel(model) {
		http.Error(w, "unknown model", http.StatusBadRequest)
		return
	}
	// Limit request body size
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.RequestMaxBodyBytes)
	req, err := s.decodeGeminiRequest(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	// Enriched logging: model, thinking config, and total tokens
	var thinking any
	if req.GenerationConfig != nil {
		thinking = req.GenerationConfig.ThinkingConfig
	}
	totalTokens := countRequestTokens(req)
	logrus.WithFields(logrus.Fields{
		"model":          model,
		"thinkingConfig": thinking,
		"totalTokens":    totalTokens,
	}).Info("sending to upstream")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	resp, err := s.caClient.GenerateContent(ctx, model, "", req)
	if err != nil {
		http.Error(w, err.Error(), httpStatusFromError(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStreamGenerateContent(model string, w http.ResponseWriter, r *http.Request) {
	if !s.validateModel(model) {
		http.Error(w, "unknown model", http.StatusBadRequest)
		return
	}
	// Limit request body size
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.RequestMaxBodyBytes)
	req, err := s.decodeGeminiRequest(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	// logrus.Infof("decoded request %s", utils.TruncateLongStringInObject(req, 100))
	flusher, ok := w.(http.Flusher)
	if !ok {
		logrus.Warn("streaming unsupported")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out, errs := s.caClient.GenerateContentStream(ctx, model, "", req)

	// Prepare enriched logging: model, thinking config, and total tokens
	var thinking any
	if req.GenerationConfig != nil {
		thinking = req.GenerationConfig.ThinkingConfig
	}
	totalTokens := countRequestTokens(req)
	logrus.WithFields(logrus.Fields{
		"model":          model,
		"thinkingConfig": thinking,
		"totalTokens":    totalTokens,
	}).Info("sending to upstream")
	enc := json.NewEncoder(w)
	for {
		select {
		case g, ok := <-out:
			if !ok {
				return
			}
			// SSE event - send raw response like TypeScript version
			if _, err := fmt.Fprint(w, "data: "); err != nil {
				logrus.Errorf("error writing data prefix: %v", err)
				return
			}
			if err := enc.Encode(g); err != nil {
				return
			}
			// enc.Encode writes a trailing newline
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				logrus.Errorf("error writing newline: %v", err)
				return
			}
			flusher.Flush()
		case e, ok := <-errs:
			// If the error channel is closed or yields a nil error,
			// treat it as a normal end-of-stream signal but continue
			// draining the output channel until it closes.
			if !ok || e == nil {
				// Disable further selects on errs to avoid busy looping on a closed channel
				errs = nil
				continue
			}
			// Non-nil error: emit error event then end
			if _, err := fmt.Fprint(w, "event: error\n"); err != nil {
				logrus.Errorf("error writing error event: %v", err)
				return
			}
			if _, err := fmt.Fprintf(w, "data: {\"error\":{\"message\":%q}}\n\n", e.Error()); err != nil {
				logrus.Errorf("error writing error data: %v", err)
				return
			}
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// countRequestTokens approximates the total token count for the request
// by summing tokens of all text parts in systemInstruction and contents
// using tiktoken-go/tokenizer. We default to O200kBase encoding.
func countRequestTokens(req gemini.GeminiRequest) int {
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return 0
	}
	total := 0
	// system instruction ignored for token counting (feature removed)
	// contents
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.Text != "" {
				if n, err := enc.Count(p.Text); err == nil {
					total += n
				}
			}
		}
	}
	return total
}

func httpStatusFromError(err error) int {
	// Simple mapping; upstream errors already include status text sometimes.
	s := err.Error()
	if strings.Contains(s, "status 401") {
		return http.StatusUnauthorized
	}
	if strings.Contains(s, "status 403") {
		return http.StatusForbidden
	}
	if strings.Contains(s, "status 429") {
		return http.StatusTooManyRequests
	}
	if strings.Contains(s, "status 5") {
		return http.StatusBadGateway
	}
	return http.StatusBadRequest
}

func listModels() interface{} {
	type model struct {
		Name                       string   `json:"name"`
		Version                    string   `json:"version"`
		DisplayName                string   `json:"displayName"`
		Description                string   `json:"description"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	}
	out := struct {
		Models []model `json:"models"`
	}{Models: make([]model, 0, len(gemini.SupportedModels))}
	for _, m := range gemini.SupportedModels {
		out.Models = append(out.Models, model{
			Name:                       "models/" + m.Name,
			Version:                    "001",
			DisplayName:                m.DisplayName,
			Description:                m.Description,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
		})
	}
	return out
}
