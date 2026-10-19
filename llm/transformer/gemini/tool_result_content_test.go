package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
)

func TestConvertLLMToolResultContentBlocks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		texts []string
		want  map[string]any
	}{
		{name: "single text", texts: []string{"Observation"}, want: map[string]any{"result": "Observation"}},
		{name: "single JSON object", texts: []string{`{"temperature":21,"unit":"C"}`}, want: map[string]any{"temperature": float64(21), "unit": "C"}},
		{name: "multiple text blocks", texts: []string{"First observation", "Second observation"}, want: map[string]any{"result": "First observation\nSecond observation"}},
		{name: "multiple JSON blocks remain complete text", texts: []string{`{"one":1}`, `{"two":2}`}, want: map[string]any{"result": "{\"one\":1}\n{\"two\":2}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := make([]llm.MessageContentPart, 0, len(tc.texts))
			for _, text := range tc.texts {
				parts = append(parts, llm.MessageContentPart{Type: "text", Text: lo.ToPtr(text)})
			}
			got := convertLLMToolResultToGeminiContent(&llm.Message{Role: "tool", ToolCallID: lo.ToPtr("call-lookup"), ToolCallName: lo.ToPtr("lookup"), Content: llm.MessageContent{MultipleContent: parts}}, nil)
			require.Equal(t, "user", got.Role)
			require.Len(t, got.Parts, 1)
			require.Equal(t, "call-lookup", got.Parts[0].FunctionResponse.ID)
			require.Equal(t, "lookup", got.Parts[0].FunctionResponse.Name)
			require.Equal(t, tc.want, got.Parts[0].FunctionResponse.Response)
		})
	}
}

func TestConvertLLMToolResultMediaBlocks(t *testing.T) {
	got := convertLLMToolResultToGeminiContent(&llm.Message{Role: "tool", ToolCallID: lo.ToPtr("call-media"), ToolCallName: lo.ToPtr("inspect"), Content: llm.MessageContent{MultipleContent: []llm.MessageContentPart{
		{Type: "text", Text: lo.ToPtr("Tool observation")},
		{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,aW1hZ2U="}},
		{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.test/result.jpg"}},
		{Type: "video_url", VideoURL: &llm.VideoURL{URL: "https://example.test/result.mp4", MIMEType: "video/mp4"}},
		{Type: "document", Document: &llm.DocumentURL{URL: "data:application/pdf;base64,cGRm", MIMEType: "application/pdf"}},
		{Type: "input_audio", InputAudio: &llm.InputAudio{Data: "YXVkaW8=", Format: "wav"}},
	}}}, nil)
	require.Len(t, got.Parts, 6)
	require.Equal(t, map[string]any{"result": "Tool observation"}, got.Parts[0].FunctionResponse.Response)
	require.Equal(t, &Blob{MIMEType: "image/png", Data: "aW1hZ2U="}, got.Parts[1].InlineData)
	require.Equal(t, &FileData{MIMEType: "image/jpeg", FileURI: "https://example.test/result.jpg"}, got.Parts[2].FileData)
	require.Equal(t, &FileData{MIMEType: "video/mp4", FileURI: "https://example.test/result.mp4"}, got.Parts[3].FileData)
	require.Equal(t, &Blob{MIMEType: "application/pdf", Data: "cGRm"}, got.Parts[4].InlineData)
	require.Equal(t, &Blob{MIMEType: "audio/wav", Data: "YXVkaW8="}, got.Parts[5].InlineData)
}

func TestAnthropicToolResultBlocksToVertex(t *testing.T) {
	raw := []byte(`{"model":"gemini-4-argon","max_tokens":1024,"messages":[{"role":"user","content":"Inspect the observations"},{"role":"assistant","content":[{"type":"tool_use","id":"call-first","name":"first","input":{}},{"type":"tool_use","id":"call-second","name":"second","input":{}},{"type":"tool_use","id":"call-third","name":"third","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-first","content":[{"type":"text","text":"ONE"}]},{"type":"tool_result","tool_use_id":"call-second","content":[{"type":"text","text":"TWO_A"},{"type":"text","text":"TWO_B"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]},{"type":"tool_result","tool_use_id":"call-third","content":"{\"value\":3}"}]}]}`)
	normalized, err := anthropic.NewInboundTransformer().TransformRequest(context.Background(), &httpclient.Request{Body: raw, Headers: http.Header{"Content-Type": {"application/json"}}})
	require.NoError(t, err)
	outbound, err := NewOutboundTransformerWithConfig(Config{BaseURL: "https://aiplatform.googleapis.com/v1/projects/fixture-project/locations/global", PlatformType: PlatformVertex, APIKeyProvider: auth.NewStaticKeyProvider("fixture-only-never-sent")})
	require.NoError(t, err)
	wire, err := outbound.TransformRequest(context.Background(), normalized)
	require.NoError(t, err)
	var body GenerateContentRequest
	require.NoError(t, json.Unmarshal(wire.Body, &body))
	require.Len(t, body.Contents, 3)
	results := body.Contents[2]
	require.Equal(t, "user", results.Role)
	require.Len(t, results.Parts, 4)
	require.Equal(t, "first", results.Parts[0].FunctionResponse.Name)
	require.Equal(t, map[string]any{"result": "ONE"}, results.Parts[0].FunctionResponse.Response)
	require.Equal(t, "second", results.Parts[1].FunctionResponse.Name)
	require.Equal(t, map[string]any{"result": "TWO_A\nTWO_B"}, results.Parts[1].FunctionResponse.Response)
	require.Equal(t, &Blob{MIMEType: "image/png", Data: "aW1hZ2U="}, results.Parts[2].InlineData)
	require.Equal(t, "third", results.Parts[3].FunctionResponse.Name)
	require.Equal(t, map[string]any{"value": float64(3)}, results.Parts[3].FunctionResponse.Response)
	for _, part := range results.Parts {
		if part.FunctionResponse != nil {
			require.Empty(t, part.FunctionResponse.ID, "existing Vertex ID policy remains unchanged")
		}
	}
}
