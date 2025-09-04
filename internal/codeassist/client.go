package codeassist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"gcli2api/internal/config"
	"gcli2api/internal/gemini"
	"gcli2api/internal/httpx"
	// "gcli2api/internal/utils"
)

const (
	BaseURL   = "https://cloudcode-pa.googleapis.com"
	APIVer    = "v1internal"
	DefaultUA = "google-api-nodejs-client/9.15.1"
)

type CodeAssistRequest struct {
	Model   string               `json:"model"`
	Project string               `json:"project"`
	Request gemini.GeminiRequest `json:"request"`
}

type CodeAssistEnvelope struct {
	Response *gemini.GeminiAPIResponse `json:"response"`
}

type CaClient struct {
	httpClient *http.Client
	baseURL    string
	// transportRetries are used only for lightweight JSON helper calls
	// such as discovery/onboarding. Generation endpoints do not use
	// per-unit HTTP retries; MultiClient orchestrates retries across units.
	transportRetries int
	baseDelay        time.Duration
}

func NewCaClient(httpClient *http.Client, transportRetries int, baseDelay time.Duration) *CaClient {
	return &CaClient{httpClient: httpClient, baseURL: BaseURL, transportRetries: transportRetries, baseDelay: baseDelay}
}

func (c *CaClient) GenerateContent(ctx context.Context, model, project string, req gemini.GeminiRequest) (*gemini.GeminiAPIResponse, error) {
	url := fmt.Sprintf("%s/%s:generateContent", c.baseURL, APIVer)
	logrus.Debugf("new request %s", url)
	body := CodeAssistRequest{Model: model, Project: project, Request: req}
	pb, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(pb))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", config.UserAgent)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Envelope
		var env CodeAssistEnvelope
		dec := json.NewDecoder(resp.Body)
		if err := dec.Decode(&env); err != nil {
			return nil, err
		}
		if env.Response == nil {
			return nil, fmt.Errorf("empty response envelope")
		}
		return env.Response, nil
	}
	// Non-2xx
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return nil, fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(b))
}

// StreamClient returns a channel of responses and an error channel.
func (c *CaClient) GenerateContentStream(ctx context.Context, model, project string, req gemini.GeminiRequest) (<-chan gemini.GeminiAPIResponse, <-chan error) {
	out := make(chan gemini.GeminiAPIResponse, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)
		url := fmt.Sprintf("%s/%s:streamGenerateContent?alt=sse", c.baseURL, APIVer)
		body := CodeAssistRequest{Model: model, Project: project, Request: req}
		pb, err := json.Marshal(body)
		if err != nil {
			errs <- err
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(pb))
		if err != nil {
			errs <- err
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("User-Agent", config.UserAgent)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		// logrus.Infof("response received, status = %d", resp.StatusCode)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			err := fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(b))
			logrus.Warnf("error response: %v", err)
			errs <- err
			return
		}
		// Use manual SSE parsing similar to internal/sse if needed; upstream returns SSE with data: envelopes.
		// Here, mimic with a small scanner over lines.
		// Simpler: reuse sse.Parse by wrapping response
		type envelope = CodeAssistEnvelope
		readErr := parseSSEStream(ctx, resp.Body, func(env *envelope) error {
			if env != nil && env.Response != nil {
				select {
				case out <- *env.Response:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		if readErr != nil && readErr != io.EOF {
			errs <- readErr
			return
		}
	}()
	return out, errs
}

// parseSSEStream is a local minimal SSE parser to avoid extra imports.
func parseSSEStream(ctx context.Context, r io.Reader, cb func(*CodeAssistEnvelope) error) error {
	// Process each data line immediately like the TypeScript version
	br := bufio.NewScanner(r)
	// Increase buffer size for large events
	const maxCapacity = 1024 * 1024
	br.Buffer(make([]byte, 0, 64*1024), maxCapacity)

	for br.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := br.Text()

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Process data lines immediately
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimSpace(line[6:]) // Extract data after "data: "

			// Skip [DONE] messages like TypeScript version
			if data == "[DONE]" {
				continue
			}

			// Parse JSON data - handle both envelope and raw response formats
			var response gemini.GeminiAPIResponse
			var usageMetadata *gemini.UsageMetadata

			// First try to parse as a generic map to detect envelope format
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(data), &raw); err != nil {
				// Avoid logging raw SSE payload to prevent leaking sensitive data
				logrus.WithFields(logrus.Fields{
					"err":        err,
					"data_bytes": len(data),
				}).Error("failed to unmarshal SSE data as JSON")
				continue
			}

			// Check if this is an envelope format with "response" field
			if responseRaw, hasResponse := raw["response"]; hasResponse {
				// Extract the response from the envelope
				if err := json.Unmarshal(responseRaw, &response); err != nil {
					logrus.WithFields(logrus.Fields{
						"err":        err,
						"data_bytes": len(data),
					}).Error("failed to unmarshal envelope response")
					continue
				}

				// Extract usage metadata from envelope if present
				if usageRaw, hasUsage := raw["usageMetadata"]; hasUsage {
					var usage gemini.UsageMetadata
					if err := json.Unmarshal(usageRaw, &usage); err == nil {
						usageMetadata = &usage
					}
				}
			} else {
				// Try to parse as raw response directly
				if err := json.Unmarshal([]byte(data), &response); err != nil {
					logrus.WithFields(logrus.Fields{
						"err":        err,
						"data_bytes": len(data),
					}).Error("failed to unmarshal SSE data as raw response")
					continue
				}
			}

			// If we got usage metadata from envelope, merge it into response
			if usageMetadata != nil && response.UsageMetadata == nil {
				response.UsageMetadata = usageMetadata
			}

			// Wrap in envelope for callback compatibility
			env := &CodeAssistEnvelope{Response: &response}
			// logrus.Infof("received SSE envelope: %s", utils.TruncateLongStringInObject(env, 1000))
			if err := cb(env); err != nil {
				return err
			}
		}
	}

	if err := br.Err(); err != nil {
		return err
	}
	return nil
}

// DiscoverProjectID attempts to derive the Google Cloud project ID to use with
// Code Assist when none is provided. It mirrors the Node implementation:
// 1) POST :loadCodeAssist {metadata:{pluginType:"GEMINI"}}
//   - if response.cloudaicompanionProject is present, return it
//     2. else determine default tier from response.allowedTiers[*].isDefault
//     and POST :onboardUser with {tierId, metadata:{pluginType:"GEMINI"}, cloudaicompanionProject:"default"}
//   - poll :onboardUser with same body until {done:true}
//   - return response.cloudaicompanionProject.id
func (c *CaClient) DiscoverProjectID(ctx context.Context) (string, error) {
	type allowedTier struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"isDefault"`
	}
	type loadResp struct {
		// Could be a string project id or an object; accept raw to handle both.
		CloudAICompanionProject json.RawMessage `json:"cloudaicompanionProject"`
		AllowedTiers            []allowedTier   `json:"allowedTiers"`
	}
	// First: loadCodeAssist
	var lr loadResp
	if err := c.doJSON(ctx, "loadCodeAssist", map[string]any{
		"metadata": map[string]any{"pluginType": "GEMINI"},
	}, &lr, DefaultUA); err != nil {
		return "", err
	}
	if len(lr.CloudAICompanionProject) > 0 && string(lr.CloudAICompanionProject) != "null" {
		// Try string first
		var asStr string
		if err := json.Unmarshal(lr.CloudAICompanionProject, &asStr); err == nil && asStr != "" {
			return asStr, nil
		}
		// Fallback to object with id
		var obj struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(lr.CloudAICompanionProject, &obj); err == nil && obj.ID != "" {
			return obj.ID, nil
		}
	}
	// Determine default tier
	tierID := "free-tier"
	for _, t := range lr.AllowedTiers {
		if t.IsDefault && t.ID != "" {
			tierID = t.ID
			break
		}
	}
	// Kick off onboarding and poll
	type onboardResp struct {
		Done     bool `json:"done"`
		Response struct {
			CloudAICompanionProject struct {
				ID string `json:"id"`
			} `json:"cloudaicompanionProject"`
		} `json:"response"`
		// error omitted
	}
	req := map[string]any{
		"tierId": tierID,
		"metadata": map[string]any{
			"pluginType": "GEMINI",
		},
		"cloudaicompanionProject": "default",
	}
	// Loop with small delay similar to Node (2s)
	// Use retries/backoff wrapper for transport errors; logical polling remains explicit
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("discover project timeout")
		}
		var or onboardResp
		if err := c.doJSON(ctx, "onboardUser", req, &or, DefaultUA); err != nil {
			return "", err
		}
		if or.Done {
			if id := or.Response.CloudAICompanionProject.ID; id != "" {
				return id, nil
			}
			return "", fmt.Errorf("onboardUser done without project id")
		}
		// not done yet; sleep 2s
		t := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// doJSON posts JSON to ":<method>" and decodes the JSON response into out.
func (c *CaClient) doJSON(ctx context.Context, method string, body any, out any, ua string) error {
	url := fmt.Sprintf("%s/%s:%s", c.baseURL, APIVer, method)
	pb, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var lastErr error
	return httpx.WithRetries(ctx, c.transportRetries, c.baseDelay, func(attempt int) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(pb))
		if err != nil {
			lastErr = err
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			dec := json.NewDecoder(resp.Body)
			if out == nil {
				io.Copy(io.Discard, resp.Body)
				return nil
			}
			if err := dec.Decode(out); err != nil {
				lastErr = err
				return err
			}
			return nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		lastErr = fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode == 401 || resp.StatusCode == 429 || (resp.StatusCode >= 500 && resp.StatusCode <= 599) {
			return lastErr
		}
		return nil
	})
}
