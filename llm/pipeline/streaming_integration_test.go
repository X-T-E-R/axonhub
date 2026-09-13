package pipeline_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/internal/pkg/xtest"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	responsestransformer "github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type streamUpgradeOutboundWrapper struct {
	transformer.Outbound
}

type emptyAggregateInboundWrapper struct {
	transformer.Inbound
}

type emptyJSONObjectAggregateInboundWrapper struct {
	transformer.Inbound
}

func (w *streamUpgradeOutboundWrapper) TransformRequest(ctx context.Context, request *llm.Request) (*httpclient.Request, error) {
	if request != nil && (request.Stream == nil || !*request.Stream) {
		request.Stream = lo.ToPtr(true)
	}

	return w.Outbound.TransformRequest(ctx, request)
}

func (w *emptyAggregateInboundWrapper) AggregateStreamChunks(ctx context.Context, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return nil, llm.ResponseMeta{}, nil
}

func (w *emptyJSONObjectAggregateInboundWrapper) AggregateStreamChunks(ctx context.Context, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return []byte(`{}`), llm.ResponseMeta{ID: "agg-empty"}, nil
}

// TestPipeline_Streaming_OpenAI_to_OpenAI tests streaming pipeline with OpenAI inbound and outbound transformers.
func TestPipeline_Streaming_OpenAI_to_OpenAI(t *testing.T) {
	ctx := context.Background()

	// Create transformers
	inbound := openai.NewInboundTransformer()
	outbound, err := openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	// Load test data using xtest
	streamEvents, err := xtest.LoadStreamChunks(t, "openai-tool.stream.jsonl")
	require.NoError(t, err)

	// Create mock executor for streaming
	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			// Verify request format
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/chat/completions")

			// Verify auth headers are finalized
			require.Nil(t, request.Auth)
			require.Equal(t, "Bearer test-api-key", request.Headers.Get("Authorization"))

			// Return mock stream
			return streams.SliceStream(streamEvents), nil
		},
	}

	// Create pipeline
	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	// Create test request (OpenAI format with streaming)
	requestBody := map[string]any{
		"model":  "gpt-4",
		"stream": true,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "What is the weather in New York City?",
			},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Get weather at the given location",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{
								"type": "string",
							},
						},
						"required": []string{"location"},
					},
				},
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/chat/completions",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	// Process the request
	result, err := pipeline.Process(ctx, httpRequest)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.NotNil(t, result.EventStream)

	// Collect all events from the stream
	var collectedEvents []*httpclient.StreamEvent

	for result.EventStream.Next() {
		event := result.EventStream.Current()
		collectedEvents = append(collectedEvents, event)
	}

	// Verify we got events
	require.NotEmpty(t, collectedEvents)

	// Verify the last event is [DONE]
	lastEvent := collectedEvents[len(collectedEvents)-1]
	require.Equal(t, "[DONE]", string(lastEvent.Data))
}

// TestPipeline_Streaming_OpenAI_to_Anthropic tests streaming pipeline with OpenAI inbound and Anthropic outbound transformers.
func TestPipeline_Streaming_OpenAI_to_Anthropic(t *testing.T) {
	ctx := context.Background()

	// Create transformers
	inbound := openai.NewInboundTransformer()
	outbound, err := anthropic.NewOutboundTransformer("https://api.anthropic.com", "test-api-key")
	require.NoError(t, err)

	// Load test data using xtest
	streamEvents, err := xtest.LoadStreamChunks(t, "anthropic-tool.stream.jsonl")
	require.NoError(t, err)

	// Create mock executor for streaming
	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			// Verify request format (should be Anthropic format)
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/v1/messages")
			require.Equal(t, "2023-06-01", request.Headers.Get("Anthropic-Version"))

			// Verify auth headers are finalized
			require.Nil(t, request.Auth)
			require.Equal(t, "test-api-key", request.Headers.Get("X-Api-Key"))

			// Return mock stream
			return streams.SliceStream(streamEvents), nil
		},
	}

	// Create pipeline
	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	// Create test request (OpenAI format with streaming)
	requestBody := map[string]any{
		"model":      "gpt-4",
		"stream":     true,
		"max_tokens": 1024,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "What is the weather in San Francisco, CA?",
			},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Get weather at the given location",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{
								"type": "string",
							},
						},
						"required": []string{"location"},
					},
				},
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/chat/completions",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	// Process the request
	result, err := pipeline.Process(ctx, httpRequest)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.NotNil(t, result.EventStream)

	// Collect all events from the stream
	var collectedEvents []*httpclient.StreamEvent

	for result.EventStream.Next() {
		event := result.EventStream.Current()
		collectedEvents = append(collectedEvents, event)
	}

	// Verify we got events
	require.NotEmpty(t, collectedEvents)

	// Verify the last event is [DONE]
	lastEvent := collectedEvents[len(collectedEvents)-1]
	require.Equal(t, "[DONE]", string(lastEvent.Data))
}

// TestPipeline_Streaming_Anthropic_to_OpenAI tests streaming pipeline with Anthropic inbound and OpenAI outbound transformers.
func TestPipeline_Streaming_Anthropic_to_OpenAI(t *testing.T) {
	ctx := context.Background()

	// Create transformers
	inbound := anthropic.NewInboundTransformer()
	outbound, err := openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	// Load test data using xtest
	streamEvents, err := xtest.LoadStreamChunks(t, "openai-tool.stream.jsonl")
	require.NoError(t, err)

	// Create mock executor for streaming
	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			// Verify request format (should be OpenAI format)
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/chat/completions")

			// Verify auth headers are finalized
			require.Nil(t, request.Auth)
			require.Equal(t, "Bearer test-api-key", request.Headers.Get("Authorization"))

			// Return mock stream
			return streams.SliceStream(streamEvents), nil
		},
	}

	// Create pipeline
	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	// Create test request (Anthropic format with streaming)
	requestBody := map[string]any{
		"model":      "claude-3-sonnet-20240229",
		"max_tokens": 1024,
		"stream":     true,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "What is the weather in New York City?",
			},
		},
		"tools": []map[string]any{
			{
				"name":        "get_weather",
				"description": "Get weather at the given location",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"location"},
				},
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/messages",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	// Process the request
	result, err := pipeline.Process(ctx, httpRequest)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.NotNil(t, result.EventStream)

	// Collect all events from the stream
	var collectedEvents []*httpclient.StreamEvent

	for result.EventStream.Next() {
		event := result.EventStream.Current()
		collectedEvents = append(collectedEvents, event)
	}

	// Verify we got events
	require.NotEmpty(t, collectedEvents)

	// The last event should be a message_stop event in Anthropic format
	lastEvent := collectedEvents[len(collectedEvents)-1]
	require.NotEmpty(t, lastEvent.Data)
}

// TestPipeline_Streaming_Anthropic_to_Anthropic tests streaming pipeline with Anthropic inbound and outbound transformers.
func TestPipeline_Streaming_Anthropic_to_Anthropic(t *testing.T) {
	ctx := context.Background()

	// Create transformers
	inbound := anthropic.NewInboundTransformer()
	outbound, err := anthropic.NewOutboundTransformer("https://api.anthropic.com", "test-api-key")
	require.NoError(t, err)

	// Load test data using xtest
	streamEvents, err := xtest.LoadStreamChunks(t, "anthropic-tool.stream.jsonl")
	require.NoError(t, err)

	// Create mock executor for streaming
	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			// Verify request format (should be Anthropic format)
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/v1/messages")
			require.Equal(t, "2023-06-01", request.Headers.Get("Anthropic-Version"))

			// Verify auth headers are finalized
			require.Nil(t, request.Auth)
			require.Equal(t, "test-api-key", request.Headers.Get("X-Api-Key"))

			// Return mock stream
			return streams.SliceStream(streamEvents), nil
		},
	}

	// Create pipeline
	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)
	// Create test request (Anthropic format with streaming)
	requestBody := map[string]any{
		"model":      "claude-3-sonnet-20240229",
		"max_tokens": 1024,
		"stream":     true,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "What is the weather in San Francisco, CA?",
			},
		},
		"tools": []map[string]any{
			{
				"name":        "get_weather",
				"description": "Get weather at the given location",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"location"},
				},
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/messages",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	// Process the request
	result, err := pipeline.Process(ctx, httpRequest)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.NotNil(t, result.EventStream)

	// Collect all events from the stream
	var collectedEvents []*httpclient.StreamEvent

	for result.EventStream.Next() {
		event := result.EventStream.Current()
		collectedEvents = append(collectedEvents, event)
	}

	// Verify we got events
	require.NotEmpty(t, collectedEvents)

	// The last event should be a message_stop event in Anthropic format
	lastEvent := collectedEvents[len(collectedEvents)-1]
	require.NotEmpty(t, lastEvent.Data)
}

func TestPipeline_NonStreaming_AutoAggregateUpgradedStream(t *testing.T) {
	ctx := context.Background()

	inbound := openai.NewInboundTransformer()
	baseOutbound, err := openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	outbound := &streamUpgradeOutboundWrapper{Outbound: baseOutbound}

	streamEvents, err := xtest.LoadStreamChunks(t, "openai-tool.stream.jsonl")
	require.NoError(t, err)

	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/chat/completions")
			require.Nil(t, request.Auth)
			require.Equal(t, "Bearer test-api-key", request.Headers.Get("Authorization"))

			var reqBody map[string]any
			err := json.Unmarshal(request.Body, &reqBody)
			require.NoError(t, err)
			require.Equal(t, true, reqBody["stream"])

			return streams.SliceStream(streamEvents), nil
		},
	}

	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	requestBody := map[string]any{
		"model": "gpt-4",
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "What is the weather in New York City?",
			},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Get weather at the given location",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{
								"type": "string",
							},
						},
						"required": []string{"location"},
					},
				},
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/chat/completions",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	result, err := pipeline.Process(ctx, httpRequest)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream)
	require.NotNil(t, result.Response)
	require.Equal(t, http.StatusOK, result.Response.StatusCode)
	require.Equal(t, "application/json", result.Response.Headers.Get("Content-Type"))

	var finalResponse map[string]any
	err = json.Unmarshal(result.Response.Body, &finalResponse)
	require.NoError(t, err)
	require.Equal(t, "chat.completion", finalResponse["object"])
	require.NotEmpty(t, finalResponse["choices"])
}

func TestPipeline_NonStreaming_AutoAggregateUpgradedStream_OpenAIEmptyStreamChunks(t *testing.T) {
	ctx := context.Background()

	inbound := openai.NewInboundTransformer()
	baseOutbound, err := openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	outbound := &streamUpgradeOutboundWrapper{Outbound: baseOutbound}

	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/chat/completions")
			return streams.SliceStream([]*httpclient.StreamEvent{}), nil
		},
	}

	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	requestBody := map[string]any{
		"model": "gpt-4",
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "Test message",
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/chat/completions",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	result, err := pipeline.Process(ctx, httpRequest)
	require.Nil(t, result)
	require.ErrorContains(t, err, "failed to auto-aggregate streaming response")
	require.ErrorContains(t, err, "empty stream chunks")
}

func TestPipeline_NonStreaming_AutoAggregateUpgradedStream_AnthropicEmptyStreamChunks(t *testing.T) {
	ctx := context.Background()

	inbound := anthropic.NewInboundTransformer()
	baseOutbound, err := anthropic.NewOutboundTransformer("https://api.anthropic.com", "test-api-key")
	require.NoError(t, err)

	outbound := &streamUpgradeOutboundWrapper{Outbound: baseOutbound}

	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/v1/messages")
			return streams.SliceStream([]*httpclient.StreamEvent{}), nil
		},
	}

	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	requestBody := map[string]any{
		"model":      "claude-3-sonnet-20240229",
		"max_tokens": 1024,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "Test message",
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/messages",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	result, err := pipeline.Process(ctx, httpRequest)
	require.Nil(t, result)
	require.ErrorContains(t, err, "failed to auto-aggregate streaming response")
	require.ErrorContains(t, err, "empty stream chunks")
}

func TestPipeline_NonStreaming_AutoAggregateUpgradedStream_EmptyAggregatedBody(t *testing.T) {
	ctx := context.Background()

	baseInbound := openai.NewInboundTransformer()
	inbound := &emptyAggregateInboundWrapper{Inbound: baseInbound}
	baseOutbound, err := openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	outbound := &streamUpgradeOutboundWrapper{Outbound: baseOutbound}

	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/chat/completions")
			return streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte("[DONE]")}}), nil
		},
	}

	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	requestBody := map[string]any{
		"model": "gpt-4",
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "Test message",
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/chat/completions",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	result, err := pipeline.Process(ctx, httpRequest)
	require.Nil(t, result)
	require.ErrorContains(t, err, "failed to auto-aggregate streaming response")
	require.ErrorContains(t, err, "empty aggregated body")
}

func TestPipeline_NonStreaming_AutoAggregateUpgradedStream_EmptyJSONObjectAggregatedBodyAllowed(t *testing.T) {
	ctx := context.Background()

	inbound := &emptyJSONObjectAggregateInboundWrapper{Inbound: openai.NewInboundTransformer()}
	baseOutbound, err := openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	outbound := &streamUpgradeOutboundWrapper{Outbound: baseOutbound}

	executor := &mockExecutor{
		doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
			require.Equal(t, http.MethodPost, request.Method)
			require.Contains(t, request.URL, "/chat/completions")
			return streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte(`{"stub":true}`)}}), nil
		},
	}

	factory := pipeline.NewFactory(executor)
	pipeline := factory.Pipeline(inbound, outbound)

	requestBody := map[string]any{
		"model": "gpt-4",
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": "Test message",
			},
		},
	}

	requestBodyBytes, err := json.Marshal(requestBody)
	require.NoError(t, err)

	httpRequest := &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/chat/completions",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBodyBytes,
	}

	result, err := pipeline.Process(ctx, httpRequest)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream)
	require.NotNil(t, result.Response)
	require.Equal(t, "{}", string(result.Response.Body))
}
func TestPipeline_Streaming_WithTestData(t *testing.T) {
	tests := []struct {
		name                string
		inboundType         string
		outboundType        string
		inputStreamFile     string
		expectedOutputCheck func(t *testing.T, events []*httpclient.StreamEvent)
	}{
		{
			name:            "OpenAI to OpenAI with tool calls",
			inboundType:     "openai",
			outboundType:    "openai",
			inputStreamFile: "openai-tool.stream.jsonl",
			expectedOutputCheck: func(t *testing.T, events []*httpclient.StreamEvent) {
				t.Helper()
				require.NotEmpty(t, events)
				lastEvent := events[len(events)-1]
				require.Equal(t, "[DONE]", string(lastEvent.Data))
			},
		},
		{
			name:            "Anthropic to Anthropic with tool calls",
			inboundType:     "anthropic",
			outboundType:    "anthropic",
			inputStreamFile: "anthropic-tool.stream.jsonl",
			expectedOutputCheck: func(t *testing.T, events []*httpclient.StreamEvent) {
				t.Helper()
				require.NotEmpty(t, events)
				// Anthropic streams end with message_stop event
				lastEvent := events[len(events)-1]
				require.NotEmpty(t, lastEvent.Data)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Create transformers based on test type
			var (
				inbound  transformer.Inbound
				outbound transformer.Outbound
			)

			switch tt.inboundType {
			case "openai":
				inbound = openai.NewInboundTransformer()
			case "anthropic":
				inbound = anthropic.NewInboundTransformer()
			}

			switch tt.outboundType {
			case "openai":
				var err error

				outbound, err = openai.NewOutboundTransformer("https://api.openai.com", "test-api-key")
				require.NoError(t, err)
			case "anthropic":
				var err error

				outbound, err = anthropic.NewOutboundTransformer("https://api.anthropic.com", "test-api-key")
				require.NoError(t, err)
			}

			// Load test data using xtest
			streamEvents, err := xtest.LoadStreamChunks(t, tt.inputStreamFile)
			require.NoError(t, err)

			// Create mock executor
			executor := &mockExecutor{
				doStreamFunc: func(ctx context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
					return streams.SliceStream(streamEvents), nil
				},
			}

			// Create pipeline
			factory := pipeline.NewFactory(executor)
			pipeline := factory.Pipeline(inbound, outbound)

			// Create appropriate test request
			var (
				requestBody map[string]any
				requestURL  string
			)

			if tt.inboundType == "openai" {
				requestBody = map[string]any{
					"model":  "gpt-4",
					"stream": true,
					"messages": []map[string]any{
						{
							"role":    "user",
							"content": "Test message",
						},
					},
				}
				requestURL = "/v1/chat/completions"
			} else {
				requestBody = map[string]any{
					"model":      "claude-3-sonnet-20240229",
					"max_tokens": 1024,
					"stream":     true,
					"messages": []map[string]any{
						{
							"role":    "user",
							"content": "Test message",
						},
					},
				}
				requestURL = "/v1/messages"
			}

			requestBodyBytes, err := json.Marshal(requestBody)
			require.NoError(t, err)

			httpRequest := &httpclient.Request{
				Method: http.MethodPost,
				URL:    requestURL,
				Headers: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: requestBodyBytes,
			}

			// Process the request
			result, err := pipeline.Process(ctx, httpRequest)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.Stream)
			require.NotNil(t, result.EventStream)

			// Collect all events from the stream
			var collectedEvents []*httpclient.StreamEvent

			for result.EventStream.Next() {
				event := result.EventStream.Current()
				collectedEvents = append(collectedEvents, event)
			}

			// Run the expected output check
			tt.expectedOutputCheck(t, collectedEvents)
		})
	}
}

func runChatUpstreamResponsesStream(
	t *testing.T,
	contentType string,
	body string,
) ([]*httpclient.StreamEvent, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))

		var upstreamRequest map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&upstreamRequest))
		require.Equal(t, true, upstreamRequest["stream"])

		w.Header().Set("Content-Type", contentType)
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
	}))
	defer server.Close()

	outbound, err := openai.NewOutboundTransformer(server.URL, "test-api-key")
	require.NoError(t, err)
	p := pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(
		responsestransformer.NewInboundTransformer(),
		outbound,
	)

	result, err := p.Process(t.Context(), &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/responses",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: []byte(`{"model":"command-r","input":"test prompt","stream":true}`),
	})
	if err != nil {
		return nil, err
	}
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.NotNil(t, result.EventStream)
	defer result.EventStream.Close()

	var events []*httpclient.StreamEvent
	for result.EventStream.Next() {
		events = append(events, result.EventStream.Current())
	}

	return events, result.EventStream.Err()
}

func responsesEventByType(t *testing.T, events []*httpclient.StreamEvent, eventType string) responsestransformer.StreamEvent {
	t.Helper()

	for _, event := range events {
		if event == nil || event.Type != eventType {
			continue
		}

		var decoded responsestransformer.StreamEvent
		require.NoError(t, json.Unmarshal(event.Data, &decoded))
		return decoded
	}

	require.FailNow(t, "missing Responses stream event", "type=%s events=%v", eventType, events)
	return responsestransformer.StreamEvent{}
}

func responsesEventsByType(t *testing.T, events []*httpclient.StreamEvent, eventType string) []responsestransformer.StreamEvent {
	t.Helper()

	var decodedEvents []responsestransformer.StreamEvent
	for _, event := range events {
		if event == nil || event.Type != eventType {
			continue
		}
		var decoded responsestransformer.StreamEvent
		require.NoError(t, json.Unmarshal(event.Data, &decoded))
		decodedEvents = append(decodedEvents, decoded)
	}
	return decodedEvents
}

func TestPipeline_Streaming_OpenAIChatJSONToResponses(t *testing.T) {
	t.Run("text and usage", func(t *testing.T) {
		events, err := runChatUpstreamResponsesStream(t, "application/json; charset=utf-8", `{
			"id":"chatcmpl-json","object":"chat.completion","created":42,"model":"command-r",
			"choices":[{"index":0,"message":{"role":"assistant","content":"complete reply"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10},"error":null
		}`)
		require.NoError(t, err)

		delta := responsesEventByType(t, events, "response.output_text.delta")
		require.Equal(t, "complete reply", delta.Delta)

		completed := responsesEventByType(t, events, "response.completed")
		require.NotNil(t, completed.Response)
		require.Equal(t, "chatcmpl-json", completed.Response.ID)
		require.NotNil(t, completed.Response.Status)
		require.Equal(t, "completed", *completed.Response.Status)
		require.NotNil(t, completed.Response.Usage)
		require.Equal(t, int64(10), completed.Response.Usage.TotalTokens)
		require.Len(t, completed.Response.Output, 1)
		require.Equal(t, "message", completed.Response.Output[0].Type)
	})

	t.Run("tool calls and usage", func(t *testing.T) {
		events, err := runChatUpstreamResponsesStream(t, "application/json", `{
			"id":"chatcmpl-tool","object":"chat.completion","created":43,"model":"command-r",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}},
				{"id":"call_2","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"docs\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}
		}`)
		require.NoError(t, err)

		arguments := responsesEventsByType(t, events, "response.function_call_arguments.done")
		require.Len(t, arguments, 2)
		require.Equal(t, `{"path":"README.md"}`, arguments[0].Arguments)
		require.Equal(t, `{"path":"docs"}`, arguments[1].Arguments)

		completed := responsesEventByType(t, events, "response.completed")
		require.NotNil(t, completed.Response)
		require.NotNil(t, completed.Response.Usage)
		require.Equal(t, int64(13), completed.Response.Usage.TotalTokens)
		require.Len(t, completed.Response.Output, 2)
		require.Equal(t, "function_call", completed.Response.Output[0].Type)
		require.Equal(t, "call_1", completed.Response.Output[0].CallID)
		require.Equal(t, "read_file", completed.Response.Output[0].Name)
		require.Equal(t, `{"path":"README.md"}`, completed.Response.Output[0].Arguments)
		require.Equal(t, "function_call", completed.Response.Output[1].Type)
		require.Equal(t, "call_2", completed.Response.Output[1].CallID)
		require.Equal(t, "list_files", completed.Response.Output[1].Name)
		require.Equal(t, `{"path":"docs"}`, completed.Response.Output[1].Arguments)
	})
}

func TestPipeline_Streaming_OpenAIChatJSONToResponsesFailures(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "OpenAI JSON error",
			contentType: "application/json",
			body:        `{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"quota"}}`,
			wantCode:    "quota",
			wantMessage: "quota exhausted",
		},
		{
			name:        "top-level typed JSON error",
			contentType: "application/json",
			body:        `{"type":"error","message":"bad gateway response","code":"bad_response"}`,
			wantCode:    "bad_response",
			wantMessage: "bad gateway response",
		},
		{
			name:        "SSE error remains supported",
			contentType: "text/event-stream",
			body:        "data: {\"error\":{\"message\":\"stream quota exhausted\",\"type\":\"insufficient_quota\",\"code\":\"stream_quota\"}}\n\n",
			wantCode:    "stream_quota",
			wantMessage: "stream quota exhausted",
		},
		{
			name:        "empty JSON body",
			contentType: "application/json",
			body:        "",
			wantCode:    "stream_error",
			wantMessage: "unexpected EOF",
		},
		{
			name:        "malformed JSON body",
			contentType: "application/json",
			body:        `{"choices":[}`,
			wantCode:    "stream_error",
			wantMessage: "invalid JSON stream response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := runChatUpstreamResponsesStream(t, tt.contentType, tt.body)
			require.NoError(t, err)

			errorEvent := responsesEventByType(t, events, "error")
			require.Equal(t, tt.wantCode, errorEvent.Code)
			require.Contains(t, errorEvent.Message, tt.wantMessage)
		})
	}
}

func TestPipeline_Streaming_OpenAIChatNativeSSEToResponsesUnchanged(t *testing.T) {
	events, err := runChatUpstreamResponsesStream(t, "text/event-stream", ""+
		"data: {\"id\":\"chatcmpl-sse\",\"object\":\"chat.completion.chunk\",\"created\":44,\"model\":\"command-r\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"native stream\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2,\"total_tokens\":8}}\n\n"+
		"data: [DONE]\n\n")
	require.NoError(t, err)

	delta := responsesEventByType(t, events, "response.output_text.delta")
	require.Equal(t, "native stream", delta.Delta)
	completed := responsesEventByType(t, events, "response.completed")
	require.NotNil(t, completed.Response)
	require.Equal(t, "chatcmpl-sse", completed.Response.ID)
}
