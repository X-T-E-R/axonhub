package gemini

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
)

// Verify API-key request construction for projectless and explicit resource
// paths. This fixture does not establish that a real key can authenticate to
// either endpoint; provider acceptance is checked by the separate live smoke.
func TestVertexAPIKeyStreamingResourcePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		wantURL string
	}{
		{
			name:    "express retains projectless endpoint",
			baseURL: "https://aiplatform.googleapis.com/v1",
			wantURL: "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-4-argon:streamGenerateContent?alt=sse",
		},
		{
			name:    "standard preserves explicit global location",
			baseURL: "https://aiplatform.googleapis.com/v1/projects/test-credit-project/locations/global",
			wantURL: "https://aiplatform.googleapis.com/v1/projects/test-credit-project/locations/global/publishers/google/models/gemini-4-argon:streamGenerateContent?alt=sse",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outbound, err := NewOutboundTransformerWithConfig(Config{
				BaseURL:        tc.baseURL,
				PlatformType:   PlatformVertex,
				APIKeyProvider: auth.NewStaticKeyProvider("test-vertex-key"),
			})
			require.NoError(t, err)

			req, err := outbound.TransformRequest(context.Background(), &llm.Request{
				Model:  "gemini-4-argon",
				Stream: lo.ToPtr(true),
				Messages: []llm.Message{{
					Role:    "user",
					Content: llm.MessageContent{Content: lo.ToPtr("Reply OK.")},
				}},
				MaxTokens: lo.ToPtr(int64(64)),
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantURL, req.URL)
			require.Equal(t, "api_key", req.Auth.Type)
			require.Equal(t, "x-goog-api-key", req.Auth.HeaderKey)
			require.Equal(t, "test-vertex-key", req.Auth.APIKey)

			var body map[string]any
			require.NoError(t, json.Unmarshal(req.Body, &body))
			generation := body["generationConfig"].(map[string]any)
			require.Equal(t, float64(64), generation["maxOutputTokens"])
			require.NotContains(t, generation, "thinkingConfig")
		})
	}
}
