package codeassist

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"gcli2api/internal/gemini"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mkClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt, Timeout: 0}
}

func resp(status int, body string, ct string) *http.Response {
	if ct == "" {
		ct = "application/json"
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewBufferString(body)), Header: http.Header{"Content-Type": []string{ct}}}
}

func TestClient_Unary_NoInternalRetry(t *testing.T) {
	attempts := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return resp(429, "rate limit", "text/plain"), nil
	})
	c := NewCaClient(mkClient(rt), 2, 1*time.Millisecond)
	_, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "hi"}}}}})
	if err == nil {
		t.Fatalf("expected error due to single-attempt 429")
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts)
	}
}

func TestStream_SSEParse_Success(t *testing.T) {
	sseBody := "data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"c1\"}]}}]}}\n\n" +
		"data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"c2\"}]}}]}}\n\n"
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, sseBody, "text/event-stream"), nil
	})
	c := NewCaClient(mkClient(rt), 2, 1*time.Millisecond)
	out, errs := c.GenerateContentStream(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "x"}}}}})
	var parts []string
	for g := range out {
		if len(g.Candidates) > 0 && len(g.Candidates[0].Content.Parts) > 0 {
			parts = append(parts, g.Candidates[0].Content.Parts[0].Text)
		}
	}
	if err := <-errs; err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 2 || parts[0] != "c1" || parts[1] != "c2" {
		t.Fatalf("bad parts: %+v", parts)
	}
}
