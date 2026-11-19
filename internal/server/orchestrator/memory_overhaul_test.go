package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestPersistenceState_ReleaseRequestPayloads_NonMutating(t *testing.T) {
	t.Parallel()

	origBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	origReq := &httpclient.Request{
		Method:    http.MethodPost,
		URL:       "http://upstream.test/v1/messages",
		APIFormat: string(llm.APIFormatAnthropicMessage),
		Body:      origBody,
	}
	origProviderReq := &httpclient.Request{
		Method:    http.MethodPost,
		URL:       "http://upstream.test/v1/messages",
		APIFormat: string(llm.APIFormatAnthropicMessage),
		Body:      origBody,
	}
	streamTrue := true
	state := &PersistenceState{
		RawRequest:         origReq,
		RawProviderRequest: origProviderReq,
		ProviderStreamResponse: &httpclient.Response{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": {"text/event-stream"}},
			Request:    origProviderReq,
		},
		LlmRequest: &llm.Request{
			APIFormat:   llm.APIFormatAnthropicMessage,
			RequestType: llm.RequestTypeChat,
			Model:       "claude-3-5-sonnet",
			Stream:      &streamTrue,
			Messages:    []llm.Message{{Role: "user"}},
			Tools:       []llm.Tool{{Type: "function"}},
			RawRequest:  origReq,
		},
	}

	state.ReleaseRequestPayloads()

	require.Nil(t, state.RawRequest)
	require.NotNil(t, state.RawProviderRequest)
	require.Nil(t, state.RawProviderRequest.Body)
	require.Equal(t, string(llm.APIFormatAnthropicMessage), state.RawProviderRequest.APIFormat)
	require.Nil(t, state.ProviderStreamResponse.Request)
	require.Equal(t, http.StatusOK, state.ProviderStreamResponse.StatusCode)
	require.NotNil(t, state.LlmRequest)
	require.Equal(t, llm.APIFormatAnthropicMessage, state.LlmRequest.APIFormat)
	require.Equal(t, "claude-3-5-sonnet", state.LlmRequest.Model)
	require.Nil(t, state.LlmRequest.Messages)
	require.Nil(t, state.LlmRequest.Tools)
	require.Nil(t, state.LlmRequest.RawRequest)

	// Caller's original structs must NOT be mutated in place (protects async observation writers).
	require.Equal(t, origBody, origReq.Body)
	require.Equal(t, origBody, origProviderReq.Body)
}

func TestShallowParseAndOnDemandMaterialization(t *testing.T) {
	t.Parallel()

	rawBody := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hello world"}]}`)
	httpReq := &httpclient.Request{
		Method:  http.MethodPost,
		Path:    "/v1/messages",
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    rawBody,
	}

	state := &PersistenceState{}
	inbound := &PersistentInboundTransformer{
		wrapped: anthropic.NewInboundTransformer(),
		state:   state,
	}

	llmReq, err := inbound.TransformRequest(context.Background(), httpReq)
	require.NoError(t, err)
	require.True(t, state.llmRequestShallow, "valid Anthropic request should use shallow parse")
	require.Equal(t, "claude-3-5-sonnet", llmReq.Model)
	require.NotNil(t, llmReq.Stream)
	require.True(t, *llmReq.Stream)
	require.Empty(t, llmReq.Messages, "shallow parse must skip Messages AST")

	// Simulate model mapping mutation before on-demand materialization (e.g. cross-protocol retry).
	llmReq.Model = "gpt-4o"

	materialized, err := state.EnsureFullLlmRequest(context.Background())
	require.NoError(t, err)
	require.False(t, state.llmRequestShallow)
	require.Same(t, llmReq, materialized, "materialization must update llmReq in place")
	require.Equal(t, "gpt-4o", materialized.Model, "scalar mutations before materialization must be preserved")
	require.Len(t, materialized.Messages, 1)

	// Invalid Anthropic request (missing max_tokens) must fall back to full transformer error.
	badReq := &httpclient.Request{
		Method:  http.MethodPost,
		Path:    "/v1/messages",
		Headers: http.Header{"Content-Type": {"application/json"}},
		Body:    []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`),
	}
	badState := &PersistenceState{}
	badInbound := &PersistentInboundTransformer{wrapped: anthropic.NewInboundTransformer(), state: badState}
	_, err = badInbound.TransformRequest(context.Background(), badReq)
	require.Error(t, err)
	require.False(t, badState.llmRequestShallow)
}

func TestDirectPassThroughStreamDecisionAndZeroGoroutineBypass(t *testing.T) {
	t.Parallel()

	ptTrue := true
	streamTrue := true
	outTr, err := anthropic.NewOutboundTransformer("https://api.anthropic.com", "sk-test")
	require.NoError(t, err)
	oaiOut, err := openai.NewOutboundTransformer("https://api.openai.com", "sk-test")
	require.NoError(t, err)

	state := &PersistenceState{
		inboundTransformer:    anthropic.NewInboundTransformer(),
		OriginalRequestStream: &streamTrue,
		LlmRequest: &llm.Request{
			APIFormat: llm.APIFormatAnthropicMessage,
			Model:     "claude-3-5-sonnet",
			Stream:    &streamTrue,
		},
		RawProviderRequest: &httpclient.Request{
			APIFormat: string(llm.APIFormatAnthropicMessage),
		},
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: &biz.Channel{
				Channel: &ent.Channel{
					ID:   1,
					Name: "anth-pt",
					Settings: &objects.ChannelSettings{
						PassThroughBody: &ptTrue,
					},
				},
			},
		},
	}

	outbound := &PersistentOutboundTransformer{
		wrapped: outTr,
		state:   state,
	}

	require.True(t, outbound.ShouldDirectPassThroughStream(context.Background(), state.RawProviderRequest))
	require.True(t, state.DirectPassThroughStream)

	// captureRawProviderStream must return source stream directly without spawning fan-out channels.
	src := streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":1}}}`)},
		{Type: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	})
	got, err := captureRawProviderStream(outbound, nil).OnOutboundRawStream(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, src, got, "direct pass-through must not wrap stream with channel fan-out")
	require.Nil(t, state.RawStreamCh, "direct pass-through must not allocate RawStreamCh")

	// Cross-protocol outbound must NOT activate direct pass-through.
	state.DirectPassThroughStream = false
	state.RawProviderRequest.APIFormat = string(llm.APIFormatOpenAIChatCompletion)
	outbound.wrapped = oaiOut
	require.False(t, outbound.ShouldDirectPassThroughStream(context.Background(), state.RawProviderRequest))
}

func TestStreamOptionsAttemptIsolationAcrossRetries(t *testing.T) {
	t.Parallel()

	streamFalse := false
	oaiOut, err := openai.NewOutboundTransformer("https://api.openai.com", "sk-test")
	require.NoError(t, err)

	req := &llm.Request{
		APIFormat: llm.APIFormatOpenAIChatCompletion,
		Model:     "gpt-4o",
		Stream:    &streamFalse,
		Messages:  []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hello")}}},
	}

	cand1 := &ChannelModelsCandidate{
		Channel: &biz.Channel{
			Channel: &ent.Channel{
				ID:   1,
				Name: "oai-force-stream",
				Policies: objects.ChannelPolicies{
					Stream: objects.CapabilityPolicyRequire,
				},
			},
			Outbound: oaiOut,
		},
		Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4o", ActualModel: "gpt-4o-attempt-1"}},
	}
	cand2 := &ChannelModelsCandidate{
		Channel: &biz.Channel{
			Channel: &ent.Channel{
				ID:   2,
				Name: "oai-normal",
			},
			Outbound: oaiOut,
		},
		Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4o", ActualModel: "gpt-4o-attempt-2"}},
	}

	// Attempt 1: channel forces streaming (Policies.Stream = Require) -> must inject stream=true & include_usage=true
	// without mutating the canonical req.Stream or req.StreamOptions.
	state := &PersistenceState{
		OriginalRequestStream:   &streamFalse,
		LlmRequest:              req,
		ChannelModelsCandidates: []*ChannelModelsCandidate{cand1, cand2},
		CurrentCandidateIndex:   0,
	}
	outbound := &PersistentOutboundTransformer{wrapped: oaiOut, state: state}

	raw1, err := outbound.TransformRequest(context.Background(), req)
	require.NoError(t, err)
	require.Contains(t, string(raw1.Body), `"stream":true`)
	require.Contains(t, string(raw1.Body), `"include_usage":true`)
	require.Nil(t, req.StreamOptions, "canonical request StreamOptions must not be mutated by attempt 1")

	// Attempt 2: channel does NOT force streaming -> must restore OriginalRequestStream (false)
	// and send stream=false without leaked stream_options.
	state.CurrentCandidateIndex = 1
	raw2, err := outbound.TransformRequest(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, req.Stream)
	require.False(t, *req.Stream, "attempt 2 must restore OriginalRequestStream")
	require.NotContains(t, string(raw2.Body), `"stream":true`)
	require.NotContains(t, string(raw2.Body), `"include_usage":true`)
}
