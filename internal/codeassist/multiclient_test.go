package codeassist

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gcli2api/internal/auth"
	"gcli2api/internal/gemini"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Focused test: rotation behavior on 401 vs 500.
func TestMultiClient_RotationPolicy_Unary(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
		{Path: "b.json", Raw: auth.RawToken{AccessToken: "xb", RefreshToken: "rb"}, Persist: false},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 1, 1*time.Millisecond, nil, nil, nil)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}

	// Subtest: rotates on 401 to next credential and succeeds
	t.Run("rotate on 401", func(t *testing.T) {
		// entry[0] returns 401; entry[1] returns 200
		attempts := []int{0, 0}
		mc.entries[0].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts[0]++
			return resp(401, "unauthorized", "text/plain"), nil
		})), 0, 1*time.Millisecond)
		mc.entries[1].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts[1]++
			return resp(200, `{"response": {"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`, "application/json"), nil
		})), 0, 1*time.Millisecond)

		g, err := mc.GenerateContent(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "hi"}}}}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(g.Candidates) == 0 || len(g.Candidates[0].Content.Parts) == 0 || g.Candidates[0].Content.Parts[0].Text != "ok" {
			t.Fatalf("bad response: %+v", g)
		}
		if attempts[0] != 1 || attempts[1] != 1 {
			t.Fatalf("expected attempts [1,1], got %v", attempts)
		}
	})

	// Subtest: rotates on 500 to next credential and succeeds
	t.Run("rotate on 500", func(t *testing.T) {
		// Reset round-robin so we start from idx=0
		atomic.StoreUint64(&mc.rr, 0)
		attempts := []int{0, 0}
		mc.entries[0].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts[0]++
			return resp(500, "boom", "text/plain"), nil
		})), 0, 1*time.Millisecond)
		mc.entries[1].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts[1]++
			return resp(200, `{"response": {"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`, "application/json"), nil
		})), 0, 1*time.Millisecond)

		g, err := mc.GenerateContent(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "hi"}}}}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(g.Candidates) == 0 || len(g.Candidates[0].Content.Parts) == 0 || g.Candidates[0].Content.Parts[0].Text != "ok" {
			t.Fatalf("bad response: %+v", g)
		}
		if attempts[0] != 1 || attempts[1] != 1 {
			t.Fatalf("expected attempts [1,1], got %v", attempts)
		}
	})
}

// New behavior: per-credential project units and rotation across them.
func TestMultiClient_ProjectUnits_RoundRobin(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
	}
	projectMap := map[string][]string{
		"a.json": {"p1", "p2"},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 1, 1*time.Millisecond, nil, nil, projectMap)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}
	if len(mc.entries) != 2 {
		t.Fatalf("expected 2 entries (2 configured, no discovery), got %d", len(mc.entries))
	}

	// entry[0] should send project p1 and return 401; entry[1] should send p2 and return 200
	attempts := []int{0, 0}
	mc.entries[0].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		attempts[0]++
		var body CodeAssistRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Project != "p1" {
			t.Errorf("entry 0 expected project p1, got %q", body.Project)
		}
		return resp(401, "unauthorized", "text/plain"), nil
	})), 0, 1*time.Millisecond)
	mc.entries[1].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		attempts[1]++
		var body CodeAssistRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Project != "p2" {
			t.Errorf("entry 1 expected project p2, got %q", body.Project)
		}
		return resp(200, `{"response": {"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`, "application/json"), nil
	})), 0, 1*time.Millisecond)

	g, err := mc.GenerateContent(context.Background(), "gemini-2.5-flash", "", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g.Candidates) == 0 || len(g.Candidates[0].Content.Parts) == 0 || g.Candidates[0].Content.Parts[0].Text != "ok" {
		t.Fatalf("bad response: %+v", g)
	}
	if attempts[0] != 1 || attempts[1] != 1 {
		t.Fatalf("expected attempts [1,1], got %v", attempts)
	}
}

// When projectIds includes the special token "_auto", include a discovery-based unit
// in addition to any explicit project ids.
func TestMultiClient_ProjectUnits_WithAuto_DiscoveryIncluded(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
	}
	projectMap := map[string][]string{
		"a.json": {"_auto", "p1"},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 0, 1*time.Millisecond, nil, nil, projectMap)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}
	if len(mc.entries) != 2 {
		t.Fatalf("expected 2 entries (1 configured + 1 discovery), got %d", len(mc.entries))
	}
	// Validate that exactly one entry has a pre-set projectID (p1) and the other is discovery-based
	configured := 0
	discovery := 0
	for _, e := range mc.entries {
		if v := e.projectID.Load(); v != nil {
			if s, ok := v.(string); ok && s != "" {
				configured++
				continue
			}
		}
		discovery++
	}
	if configured != 1 || discovery != 1 {
		t.Fatalf("expected 1 configured and 1 discovery entry, got configured=%d discovery=%d", configured, discovery)
	}
}

// When projectIds is ["_auto"] only, we include just one discovery-based unit.
func TestMultiClient_ProjectUnits_AutoOnly(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
	}
	projectMap := map[string][]string{
		"a.json": {"_auto"},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 0, 1*time.Millisecond, nil, nil, projectMap)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}
	if len(mc.entries) != 1 {
		t.Fatalf("expected 1 entry (discovery only), got %d", len(mc.entries))
	}
	if v := mc.entries[0].projectID.Load(); v != nil {
		if s, ok := v.(string); ok && s != "" {
			t.Fatalf("expected discovery entry with empty projectID, got %q", s)
		}
	}
}

func TestMultiClient_EmptyProjectList_FallsBackToDiscovery(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
	}
	projectMap := map[string][]string{
		"a.json": {},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 0, 1*time.Millisecond, nil, nil, projectMap)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}
	if len(mc.entries) != 1 {
		t.Fatalf("expected 1 entry (discovery fallback), got %d", len(mc.entries))
	}
}

// Helper ReadCloser that fails immediately with a desired error
type failingBody struct{ err error }

func (f *failingBody) Read(p []byte) (int, error) { return 0, f.err }
func (f *failingBody) Close() error               { return nil }

// Helper ReadCloser that yields one event then fails
type oneEventThenFail struct {
	yielded bool
	err     error
}

func (o *oneEventThenFail) Read(p []byte) (int, error) {
	if !o.yielded {
		o.yielded = true
		s := "data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"first\"}]}}]}}\n\n"
		copy(p, []byte(s))
		return len(s), nil
	}
	return 0, o.err
}
func (o *oneEventThenFail) Close() error { return nil }

func TestMultiClient_StreamRotation_BeforeFirstEvent(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
		{Path: "b.json", Raw: auth.RawToken{AccessToken: "xb", RefreshToken: "rb"}, Persist: false},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 1, 1*time.Millisecond, nil, nil, nil)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}
	// entry[0]: immediate timeout error before any event
	mc.entries[0].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: &failingBody{err: io.ErrUnexpectedEOF}, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}, nil
	})), 0, 1*time.Millisecond)
	// entry[1]: successful SSE
	sseBody := "data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok1\"}]}}]}}\n\n" +
		"data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok2\"}]}}]}}\n\n"
	mc.entries[1].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, sseBody, "text/event-stream"), nil
	})), 0, 1*time.Millisecond)

	out, errs := mc.GenerateContentStream(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "x"}}}}})
	var parts []string
	for g := range out {
		if len(g.Candidates) > 0 && len(g.Candidates[0].Content.Parts) > 0 {
			parts = append(parts, g.Candidates[0].Content.Parts[0].Text)
		}
	}
	if err := <-errs; err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 2 || parts[0] != "ok1" || parts[1] != "ok2" {
		t.Fatalf("bad parts: %+v", parts)
	}
}

func TestMultiClient_Stream_NoRotation_AfterFirstEvent(t *testing.T) {
	oauthCfg := oauth2.Config{ClientID: "test", ClientSecret: "s", Scopes: []string{"s"}, Endpoint: google.Endpoint}
	sources := []CredSource{
		{Path: "a.json", Raw: auth.RawToken{AccessToken: "xa", RefreshToken: "ra"}, Persist: false},
		{Path: "b.json", Raw: auth.RawToken{AccessToken: "xb", RefreshToken: "rb"}, Persist: false},
	}
	mc, err := NewMultiClient(oauthCfg, sources, 1, 1*time.Millisecond, nil, nil, nil)
	if err != nil {
		t.Fatalf("init multiclient: %v", err)
	}
	// entry[0]: send one event then fail
	mc.entries[0].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: &oneEventThenFail{err: io.ErrUnexpectedEOF}, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}, nil
	})), 0, 1*time.Millisecond)
	// entry[1]: would succeed if rotated, but should not be used
	sseBody := "data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"later\"}]}}]}}\n\n"
	mc.entries[1].ca = NewCaClient(mkClient(rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, sseBody, "text/event-stream"), nil
	})), 0, 1*time.Millisecond)

	out, errs := mc.GenerateContentStream(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "x"}}}}})
	// Expect to receive exactly one event then error; no rotation
	var got []string
	var streamErr error
	done := false
	for !done {
		select {
		case g, ok := <-out:
			if !ok {
				done = true
				break
			}
			if len(g.Candidates) > 0 && len(g.Candidates[0].Content.Parts) > 0 {
				got = append(got, g.Candidates[0].Content.Parts[0].Text)
			}
		case e := <-errs:
			streamErr = e
			done = true
		}
	}
	if len(got) != 1 || got[0] != "first" {
		t.Fatalf("expected one first event, got %+v", got)
	}
	if streamErr == nil {
		t.Fatalf("expected error after first event")
	}
}
