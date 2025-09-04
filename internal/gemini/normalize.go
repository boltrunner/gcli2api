package gemini

import (
	"strings"
)

// NormalizeGeminiRequest ensures roles are present.
func NormalizeGeminiRequest(req GeminiRequest) GeminiRequest {
	for i := range req.Contents {
		if strings.TrimSpace(req.Contents[i].Role) == "" {
			req.Contents[i].Role = "user"
		}
	}
	return req
}

// System prompt injection removed.
