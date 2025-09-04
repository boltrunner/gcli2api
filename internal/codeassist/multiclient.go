package codeassist

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"

	"gcli2api/internal/auth"
	"gcli2api/internal/gemini"
	"gcli2api/internal/httpx"
	"gcli2api/internal/state"
)

// CredSource represents a credential source for building a pool entry.
type CredSource struct {
	Path    string
	Raw     auth.RawToken
	Persist bool
}

// MultiClient fans out requests across a pool of per-credential clients.
type MultiClient struct {
	entries []*entry
	rr      uint64 // round-robin counter
	store   *state.Store

	// immutable configuration
	provider string
	clientID string

	// factory for unit tests
	mkCaClient func(httpCli *http.Client, retries int, baseDelay time.Duration) *CaClient
	// retries is the MultiClient cross-unit retry budget. Total attempts
	// per request = 1 + retries.
	retries   int
	baseDelay time.Duration
	proxyURL  *url.URL
}

type entry struct {
	idx       int
	path      string
	tokenKey  string
	ca        *CaClient
	projectID atomic.Value // string
}

// NewMultiClient constructs a MultiClient. It does not perform network calls.
// projectMap maps expanded credential paths to ordered project IDs to use.
func NewMultiClient(oauthCfg oauth2.Config, sources []CredSource, retries int, baseDelay time.Duration, st *state.Store, proxyURL *url.URL, projectMap map[string][]string) (*MultiClient, error) {
	mc := &MultiClient{
		store:    st,
		provider: "gemini-cli-oauth",
		clientID: oauthCfg.ClientID,
		mkCaClient: func(httpCli *http.Client, retries int, baseDelay time.Duration) *CaClient {
			// Keep a small transport retry budget for discovery-only JSON calls.
			// Generation paths do not use per-unit retries.
			transportRetries := 2
			return NewCaClient(httpCli, transportRetries, baseDelay)
		},
		retries:   retries,
		baseDelay: baseDelay,
		proxyURL:  proxyURL,
	}
	idx := 0
	for _, src := range sources {
		// Build a TokenSource without forcing network calls.
		baseTS := oauthCfg.TokenSource(context.Background(), src.Raw.ToOAuth2Token())
		ts := auth.NewPersistingTokenSource(baseTS, src.Raw, src.Path, src.Persist)
		httpCli := httpx.NewOAuthHTTPClient(ts, proxyURL)
		ca := mc.mkCaClient(httpCli, retries, baseDelay)
		identity := src.Raw.RefreshToken
		tokenKey := state.ComputeTokenKey(mc.provider, mc.clientID, identity)
		if units, ok := projectMap[src.Path]; ok {
			if len(units) == 0 {
				logrus.Warnf("[MultiClient] empty projectIds list for credential %s; falling back to discovery", src.Path)
				e := &entry{idx: idx, path: src.Path, tokenKey: tokenKey, ca: ca}
				mc.entries = append(mc.entries, e)
				idx++
			} else {
				// Configured projects: create one unit per explicit project id
				// and include a discovery-based unit only if "_auto" is present.
				includeAuto := false
				for _, pid := range units {
					if pid == "_auto" {
						includeAuto = true
						continue
					}
					e := &entry{idx: idx, path: src.Path, tokenKey: tokenKey, ca: ca}
					e.projectID.Store(pid)
					mc.entries = append(mc.entries, e)
					idx++
				}
				if includeAuto {
					// Add one discovery-based unit for this credential
					e := &entry{idx: idx, path: src.Path, tokenKey: tokenKey, ca: ca}
					mc.entries = append(mc.entries, e)
					idx++
				}
			}
		} else {
			e := &entry{idx: idx, path: src.Path, tokenKey: tokenKey, ca: ca}
			mc.entries = append(mc.entries, e)
			idx++
		}
	}
	if len(mc.entries) == 0 {
		return nil, fmt.Errorf("no valid credentials provided")
	}
	// Load persisted round-robin counter, if available, so we continue from
	// the next account on restart instead of defaulting to index 0.
	if mc.store != nil {
		if v, ok, err := mc.store.GetRRCounter(context.Background(), mc.provider, mc.clientID); err == nil && ok {
			atomic.StoreUint64(&mc.rr, v)
		}
	}
	logrus.Infof("[MultiClient] initialized with %d credential(s) and %d unit(s)", len(sources), len(mc.entries))
	return mc, nil
}

func (mc *MultiClient) pickStart() int {
	n := len(mc.entries)
	if n == 0 {
		return 0
	}
	v := atomic.AddUint64(&mc.rr, 1) - 1
	// Best-effort persistence of the incremented counter (v+1). This allows
	// the next process start to pick the next account in sequence.
	if mc.store != nil {
		_ = mc.store.SetRRCounter(context.Background(), mc.provider, mc.clientID, v+1)
	}
	return int(v % uint64(n))
}

func (mc *MultiClient) GenerateContent(ctx context.Context, model, project string, req gemini.GeminiRequest) (*gemini.GeminiAPIResponse, error) {
	n := len(mc.entries)
	if n == 0 {
		return nil, fmt.Errorf("no credentials configured")
	}
	start := mc.pickStart()
	var lastErr error
	total := mc.retries + 1
	for k := 0; k < total; k++ {
		j := (start + k) % n
		e := mc.entries[j]
		prj := project
		if prj == "" {
			pid, err := mc.getOrDiscoverProjectID(ctx, e)
			if err != nil {
				lastErr = err
				logrus.Warnf("[MultiClient] discovery failed; rotating attempt=%d idx=%d err=%v", k+1, e.idx, err)
				// rotate on discovery failure
				continue
			}
			prj = pid
		}
		credName := e.displayName()
		logrus.Infof("[MultiClient] attempt=%d idx=%d cred=%s model=%s project=%s", k+1, e.idx, credName, model, prj)
		resp, err := e.ca.GenerateContent(ctx, model, prj, req)
		if err == nil {
			logrus.Infof("[MultiClient] status=ok idx=%d cred=%s project=%s", e.idx, credName, prj)
			return resp, nil
		}
		lastErr = err
		if k == total-1 || !isRetryable(err) {
			logrus.Warnf("[MultiClient] non-retryable or budget exhausted idx=%d cred=%s project=%s err=%v", e.idx, credName, prj, err)
			return nil, err
		}
		logrus.Warnf("[MultiClient] rotating on error idx=%d cred=%s project=%s err=%v", e.idx, credName, prj, err)
		continue
	}
	return nil, lastErr
}

func (mc *MultiClient) GenerateContentStream(ctx context.Context, model, project string, req gemini.GeminiRequest) (<-chan gemini.GeminiAPIResponse, <-chan error) {
	out := make(chan gemini.GeminiAPIResponse, 16)
	// Unbuffered error channel ensures consumers observe error before out closes
	errs := make(chan error)
	go func() {
		n := len(mc.entries)
		if n == 0 {
			// Close out first so receivers break their loops, then send error
			close(out)
			errs <- fmt.Errorf("no credentials configured")
			close(errs)
			return
		}
		start := mc.pickStart()
		total := mc.retries + 1
		var lastErr error
		for k := 0; k < total; k++ {
			j := (start + k) % n
			e := mc.entries[j]
			prj := project
			if prj == "" {
				pid, err := mc.getOrDiscoverProjectID(ctx, e)
				if err != nil {
					lastErr = err
					logrus.Warnf("[MultiClient] discovery failed (stream); rotating attempt=%d idx=%d err=%v", k+1, e.idx, err)
					// rotate on discovery failure
					continue
				}
				prj = pid
			}
			credName := e.displayName()
			logrus.Infof("[MultiClient] streaming attempt=%d idx=%d cred=%s model=%s project=%s", k+1, e.idx, credName, model, prj)
			upOut, upErrs := e.ca.GenerateContentStream(ctx, model, prj, req)
			sentAny := false
			// Inner loop for this upstream stream
			for {
				select {
				case g, ok := <-upOut:
					if !ok {
						// Upstream output closed. If an error is pending, forward it;
						// otherwise, finish gracefully.
						if upErrs != nil {
							select {
							case e2, ok2 := <-upErrs:
								if ok2 && e2 != nil {
									// Deliver error first so consumer sees it before out closes
									errs <- e2
									close(out)
									close(errs)
									return
								}
							case <-ctx.Done():
								errs <- ctx.Err()
								close(out)
								close(errs)
								return
							}
						}
						// No error pending; close cleanly
						close(out)
						close(errs)
						return
					}
					sentAny = true
					out <- g
				case err, ok := <-upErrs:
					if !ok || err == nil {
						// Treat closed errs channel as normal end; continue
						// draining output until it closes.
						upErrs = nil
						continue
					}
					if err != nil {
						if !sentAny && k < total-1 && isRetryable(err) {
							logrus.Warnf("[MultiClient] rotating stream on early error idx=%d cred=%s err=%v", e.idx, credName, err)
							// break inner loop to next attempt
							lastErr = err
							goto nextAttempt
						}
						// either after first event or not retryable/budget exhausted
						// Deliver error first so consumer sees it before out closes
						errs <- err
						close(out)
						close(errs)
						return
					}
				case <-ctx.Done():
					errs <- ctx.Err()
					close(out)
					close(errs)
					return
				}
			}
		nextAttempt:
			continue
		}
		// All attempts exhausted or only discovery failures
		if lastErr != nil {
			// Close out first so receivers break their loops, then deliver the error
			close(out)
			errs <- lastErr
			close(errs)
			return
		}
		// Otherwise clean completion without error
		close(out)
		close(errs)
	}()
	return out, errs
}

func (e *entry) displayName() string {
	if e.path == "" {
		return fmt.Sprintf("idx-%d", e.idx)
	}

	// Replace home directory with ~
	if homeDir, err := os.UserHomeDir(); err == nil {
		if strings.HasPrefix(e.path, homeDir) {
			return "~" + e.path[len(homeDir):]
		}
	}

	return e.path
}

func (mc *MultiClient) getOrDiscoverProjectID(ctx context.Context, e *entry) (string, error) {
	if v := e.projectID.Load(); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return s, nil
		}
	}
	// Lookup in store
	if mc.store != nil {
		if pid, ok, err := mc.store.GetProjectID(ctx, e.tokenKey); err == nil && ok {
			e.projectID.Store(pid)
			return pid, nil
		}
	}
	// Discover via client
	logrus.Infof("[MultiClient] project id not found in cache for %s, attempting discovery", e.displayName())
	pid, err := e.ca.DiscoverProjectID(ctx)
	if err != nil {
		return "", err
	}
	if pid == "" {
		return "", fmt.Errorf("fail to discovered project")
	}
	e.projectID.Store(pid)
	if mc.store != nil {
		// Best-effort persistence
		_ = mc.store.UpsertProjectID(ctx, e.tokenKey, mc.provider, mc.clientID, pid)
	}
	return pid, nil
}

// isRetryable determines if an error should trigger rotation/retry.
// It treats HTTP 401, 403, 429, and all 5xx as retryable, as well as
// common transport timeouts. Context cancellations are not retried.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	s := err.Error()
	if strings.Contains(s, "status 401") || strings.Contains(s, "status 403") || strings.Contains(s, "status 429") {
		return true
	}
	if strings.Contains(s, "status 5") { // covers 5xx
		return true
	}
	// Transport/classic timeouts
	ls := strings.ToLower(s)
	if strings.Contains(ls, "timeout") || strings.Contains(ls, "connection reset") || strings.Contains(ls, "temporary failure") || strings.Contains(ls, "unexpected eof") {
		return true
	}
	return false
}
