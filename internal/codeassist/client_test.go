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

func TestClient_RetryPolicy_Unary(t *testing.T) {
	attempts := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts <= 2 {
			return resp(429, "rate limit", "text/plain"), nil
		}
		return resp(200, `{"response": {"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`, "application/json"), nil
	})
	c := NewCaClient(mkClient(rt), 3, 1*time.Millisecond)
	g, err := c.GenerateContent(context.Background(), "gemini-2.5-flash", "proj", gemini.GeminiRequest{Contents: []gemini.GeminiContent{{Role: "user", Parts: []gemini.GeminiPart{{Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g.Candidates) != 1 || g.Candidates[0].Content.Parts[0].Text != "ok" {
		t.Fatalf("bad response: %+v", g)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestStream_401RefreshResume(t *testing.T) {
	attempts := 0
	sseBody := "data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"c1\"}]}}]}}\n\n" +
		"data: {\"response\": {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"c2\"}]}}]}}\n\n"
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return resp(401, "unauthorized", "text/plain"), nil
		}
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
	if attempts < 2 {
		t.Fatalf("expected retry after 401")
	}
	if len(parts) != 2 || parts[0] != "c1" || parts[1] != "c2" {
		t.Fatalf("bad parts: %+v", parts)
	}
}
