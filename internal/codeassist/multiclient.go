package codeassist

import (
	"context"
	"fmt"
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
	retries    int
	baseDelay  time.Duration
	proxyURL   *url.URL
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
			return NewCaClient(httpCli, retries, baseDelay)
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
	for i := 0; i < n; i++ {
		j := (start + i) % n
		e := mc.entries[j]
		prj := project
		if prj == "" {
			pid, err := mc.getOrDiscoverProjectID(ctx, e)
			if err != nil {
				lastErr = err
				// Treat discovery failure like a hard error; don't rotate unless 401/429 below, so return.
				return nil, err
			}
			prj = pid
		}
		credName := e.displayName()
		logrus.Infof("[MultiClient] attempt=%d idx=%d cred=%s model=%s project=%s", i+1, e.idx, credName, model, prj)
		resp, err := e.ca.GenerateContent(ctx, model, prj, req)
		if err == nil {
			logrus.Infof("[MultiClient] status=ok idx=%d cred=%s project=%s", e.idx, credName, prj)
			return resp, nil
		}
		lastErr = err
		// Rotate only on 401/429
		es := err.Error()
		if strings.Contains(es, "status 401") || strings.Contains(es, "status 429") {
			logrus.Warnf("[MultiClient] rotating on error idx=%d cred=%s project=%s err=%v", e.idx, credName, prj, err)
			continue
		}
		// Return immediately for other errors
		logrus.Warnf("[MultiClient] non-rotating error idx=%d cred=%s project=%s err=%v", e.idx, credName, prj, err)
		return nil, err
	}
	return nil, lastErr
}

func (mc *MultiClient) GenerateContentStream(ctx context.Context, model, project string, req gemini.GeminiRequest) (<-chan gemini.GeminiAPIResponse, <-chan error) {
	// Single-cred-per-stream policy.
	out := make(chan gemini.GeminiAPIResponse, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		n := len(mc.entries)
		if n == 0 {
			errs <- fmt.Errorf("no credentials configured")
			return
		}
		e := mc.entries[mc.pickStart()]
		prj := project
		if prj == "" {
			pid, err := mc.getOrDiscoverProjectID(ctx, e)
			if err != nil {
				errs <- err
				return
			}
			prj = pid
		}
		credName := e.displayName()
		logrus.Infof("[MultiClient] streaming idx=%d cred=%s model=%s project=%s", e.idx, credName, model, prj)
		upOut, upErrs := e.ca.GenerateContentStream(ctx, model, prj, req)
		for {
			select {
			case g, ok := <-upOut:
				if !ok {
					return
				}
				out <- g
			case err := <-upErrs:
				if err != nil {
					errs <- err
				}
				return
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
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
