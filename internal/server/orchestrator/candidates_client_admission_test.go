package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func buildTestChannelWithPolicies(name string, policies objects.ChannelPolicies, passThroughUA bool, headerOps []objects.OverrideOperation) *biz.Channel {
	var settings *objects.ChannelSettings
	if passThroughUA || len(headerOps) > 0 {
		settings = &objects.ChannelSettings{
			PassThroughUserAgent:     lo.ToPtr(passThroughUA),
			HeaderOverrideOperations: headerOps,
		}
	}
	ch := &biz.Channel{
		Channel: &ent.Channel{
			Name:     name,
			Policies: policies,
			Settings: settings,
		},
	}
	return ch
}

func newRequestWithFormatAndUA(model string, apiFormat llm.APIFormat, userAgent string, extraHeaders map[string]string) *llm.Request {
	headers := make(http.Header)
	if userAgent != "" {
		headers.Set("User-Agent", userAgent)
	}
	for k, v := range extraHeaders {
		headers.Set(k, v)
	}
	return &llm.Request{
		Model:     model,
		APIFormat: apiFormat,
		RawRequest: &httpclient.Request{
			APIFormat: string(apiFormat),
			Headers:   headers,
		},
	}
}

func TestChannelAdmissionPolicySelector_Select(t *testing.T) {
	t.Parallel()

	cprPolicies := objects.ChannelPolicies{
		Stream:              objects.CapabilityPolicyUnlimited,
		AllowRequestFormats: []string{"openai/responses", "openai/responses_compact"},
		DenyRequestFormats:  []string{"openai/chat_completions", "anthropic/messages"},
		CodexOfficialClient: lo.ToPtr(true),
	}

	newCprCandidate := func() *ChannelModelsCandidate {
		return &ChannelModelsCandidate{
			Channel: buildTestChannelWithPolicies("cpr-56", cprPolicies, true, nil),
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-6.1-sol", ActualModel: "gpt-6.1-sol"}},
		}
	}

	newOpenCandidate := func() *ChannelModelsCandidate {
		return &ChannelModelsCandidate{
			Channel: buildTestChannelWithPolicies("other-channel", objects.ChannelPolicies{}, true, nil),
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-6.1-sol", ActualModel: "gpt-6.1-sol"}},
		}
	}

	t.Run("allows official Codex CLI on Responses API", func(t *testing.T) {
		t.Parallel()
		sel := WithChannelAdmissionPolicySelector(&mockSelector{
			candidates: []*ChannelModelsCandidate{newCprCandidate()},
		})
		req := newRequestWithFormatAndUA(
			"gpt-6.1-sol",
			llm.APIFormatOpenAIResponse,
			"codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64) xterm-256color",
			nil,
		)
		got, err := sel.Select(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Nil(t, sel.RejectionError(req))
	})

	t.Run("allows official Codex Desktop on Responses Compact API", func(t *testing.T) {
		t.Parallel()
		sel := WithChannelAdmissionPolicySelector(&mockSelector{
			candidates: []*ChannelModelsCandidate{newCprCandidate()},
		})
		req := newRequestWithFormatAndUA(
			"gpt-6-luna",
			llm.APIFormatOpenAIResponseCompact,
			"Codex Desktop/0.142.3 (Windows 10.0.22635; x86_64) unknown (Codex Desktop; 26.623.61825)",
			nil,
		)
		got, err := sel.Select(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Nil(t, sel.RejectionError(req))
	})

	t.Run("rejects inbound Chat Completions even with valid Codex UA", func(t *testing.T) {
		t.Parallel()
		sel := WithChannelAdmissionPolicySelector(&mockSelector{
			candidates: []*ChannelModelsCandidate{newCprCandidate()},
		})
		req := newRequestWithFormatAndUA(
			"gpt-6.1-sol",
			llm.APIFormatOpenAIChatCompletion,
			"codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64)",
			nil,
		)
		got, err := sel.Select(context.Background(), req)
		require.NoError(t, err)
		require.Empty(t, got)

		rejectErr := sel.RejectionError(req)
		var respErr *llm.ResponseError
		require.ErrorAs(t, rejectErr, &respErr)
		require.Equal(t, http.StatusBadRequest, respErr.StatusCode)
		require.Equal(t, "unsupported_request_format", respErr.Detail.Code)
	})

	t.Run("rejects inbound Anthropic Messages and Gemini Contents even with valid Codex UA", func(t *testing.T) {
		t.Parallel()
		for _, format := range []llm.APIFormat{llm.APIFormatAnthropicMessage, llm.APIFormatGeminiContents} {
			sel := WithChannelAdmissionPolicySelector(&mockSelector{
				candidates: []*ChannelModelsCandidate{newCprCandidate()},
			})
			req := newRequestWithFormatAndUA(
				"gpt-6.1-sol",
				format,
				"codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64)",
				nil,
			)
			got, err := sel.Select(context.Background(), req)
			require.NoError(t, err)
			require.Empty(t, got)

			rejectErr := sel.RejectionError(req)
			var respErr *llm.ResponseError
			require.ErrorAs(t, rejectErr, &respErr)
			require.Equal(t, http.StatusBadRequest, respErr.StatusCode)
			require.Equal(t, "unsupported_request_format", respErr.Detail.Code)
		}
	})

	t.Run("rejects missing User-Agent even when Originator header is present", func(t *testing.T) {
		t.Parallel()
		sel := WithChannelAdmissionPolicySelector(&mockSelector{
			candidates: []*ChannelModelsCandidate{newCprCandidate()},
		})
		req := newRequestWithFormatAndUA(
			"gpt-6.1-sol",
			llm.APIFormatOpenAIResponse,
			"",
			map[string]string{"Originator": "codex_cli_rs"},
		)
		got, err := sel.Select(context.Background(), req)
		require.NoError(t, err)
		require.Empty(t, got)

		rejectErr := sel.RejectionError(req)
		var respErr *llm.ResponseError
		require.ErrorAs(t, rejectErr, &respErr)
		require.Equal(t, http.StatusForbidden, respErr.StatusCode)
		require.Equal(t, "client_not_allowed", respErr.Detail.Code)
	})

	t.Run("rejects non-Codex User-Agent even when Originator header is codex_cli_rs", func(t *testing.T) {
		t.Parallel()
		for _, ua := range []string{
			"opencode/1.2.15 ai-sdk/provider-utils/3.0.20",
			"curl/8.7.1",
			"python-httpx/0.27.0",
			"OpenAI/JS 4.67.1",
		} {
			sel := WithChannelAdmissionPolicySelector(&mockSelector{
				candidates: []*ChannelModelsCandidate{newCprCandidate()},
			})
			req := newRequestWithFormatAndUA(
				"gpt-6.1-sol",
				llm.APIFormatOpenAIResponse,
				ua,
				map[string]string{"Originator": "codex_cli_rs"},
			)
			got, err := sel.Select(context.Background(), req)
			require.NoError(t, err)
			require.Empty(t, got, "expected UA %q to be rejected", ua)

			rejectErr := sel.RejectionError(req)
			var respErr *llm.ResponseError
			require.ErrorAs(t, rejectErr, &respErr)
			require.Equal(t, http.StatusForbidden, respErr.StatusCode)
			require.Equal(t, "client_not_allowed", respErr.Detail.Code)
		}
	})

	t.Run("mixed candidates keeps unrestricted channel when CPR channel rejects non-Codex client", func(t *testing.T) {
		t.Parallel()
		sel := WithChannelAdmissionPolicySelector(&mockSelector{
			candidates: []*ChannelModelsCandidate{newCprCandidate(), newOpenCandidate()},
		})
		req := newRequestWithFormatAndUA(
			"gpt-6.1-sol",
			llm.APIFormatOpenAIChatCompletion,
			"curl/8.7.1",
			nil,
		)
		got, err := sel.Select(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "other-channel", got[0].Channel.Name)
	})
}

func TestUserAgentPassThroughAndHeaderOverridePreservation(t *testing.T) {
	t.Parallel()

	inboundUA := "codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64)"
	overrideOps := []objects.OverrideOperation{
		{
			Op:    objects.OverrideOpSet,
			Path:  "User-Agent",
			Value: "Codex Desktop/0.142.3 (Windows 10.0.22635; x86_64) unknown (Codex Desktop; 26.623.61825)",
		},
		{
			Op:    objects.OverrideOpSet,
			Path:  "X-Custom-Header",
			Value: "preserved",
		},
	}

	ch := buildTestChannelWithPolicies(
		"cpr-56",
		objects.ChannelPolicies{
			AllowRequestFormats: []string{"openai/responses", "openai/responses_compact"},
			CodexOfficialClient: lo.ToPtr(true),
		},
		true,
		overrideOps,
	)

	llmReq := newRequestWithFormatAndUA("gpt-6.1-sol", llm.APIFormatOpenAIResponse, inboundUA, nil)
	state := &PersistenceState{
		OriginalModel: "gpt-6.1-sol",
		LlmRequest:    llmReq,
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: ch,
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-6.1-sol", ActualModel: "gpt-6.1-sol"}},
		},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	rawReq := &httpclient.Request{
		APIFormat: string(llm.APIFormatOpenAIResponse),
		Headers:   make(http.Header),
	}

	// 1. Inbound admission check passes
	require.NoError(t, checkChannelClientAdmission(ch, llmReq))

	// 2. applyUserAgentPassThrough sets inbound UA
	var err error
	rawReq, err = applyUserAgentPassThrough(outbound, nil).OnOutboundRawRequest(context.Background(), rawReq)
	require.NoError(t, err)
	require.Equal(t, inboundUA, rawReq.Headers.Get("User-Agent"))

	// 3. applyOverrideRequestHeaders applies X-Custom-Header but preserves byte-identical inbound UA when codexOfficialClient + passThroughUserAgent are active
	rawReq, err = applyOverrideRequestHeaders(outbound).OnOutboundRawRequest(context.Background(), rawReq)
	require.NoError(t, err)
	require.Equal(t, inboundUA, rawReq.Headers.Get("User-Agent"))
	require.Equal(t, "preserved", rawReq.Headers.Get("X-Custom-Header"))
}

func BenchmarkChannelClientAdmission(b *testing.B) {
	cprPolicies := objects.ChannelPolicies{
		Stream:              objects.CapabilityPolicyUnlimited,
		AllowRequestFormats: []string{"openai/responses", "openai/responses_compact"},
		DenyRequestFormats:  []string{"openai/chat_completions", "anthropic/messages"},
		CodexOfficialClient: lo.ToPtr(true),
	}
	cprCh := buildTestChannelWithPolicies("cpr-56", cprPolicies, true, nil)
	candidates := []*ChannelModelsCandidate{
		{
			Channel: cprCh,
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-6.1-sol", ActualModel: "gpt-6.1-sol"}},
		},
	}
	baseSelector := &mockSelector{candidates: candidates}
	req := newRequestWithFormatAndUA(
		"gpt-6.1-sol",
		llm.APIFormatOpenAIResponse,
		"codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64) xterm-256color",
		nil,
	)

	b.Run("CheckChannelClientAdmission_Allowed", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := checkChannelClientAdmission(cprCh, req); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("Selector_Allowed", func(b *testing.B) {
		b.ReportAllocs()
		ctx := context.Background()
		for i := 0; i < b.N; i++ {
			sel := WithChannelAdmissionPolicySelector(baseSelector)
			out, err := sel.Select(ctx, req)
			if err != nil || len(out) != 1 {
				b.Fatalf("unexpected: len=%d err=%v", len(out), err)
			}
		}
	})
}
