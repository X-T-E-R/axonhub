package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// mockTransformer is a simple mock transformer for testing.
type mockTransformer struct {
	aggregatedResponse []byte
	aggregatedMeta     llm.ResponseMeta
	aggregatedErr      error
	apiFormat          llm.APIFormat
	aggregateCalls     int
}

func (m *mockTransformer) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	body, err := json.Marshal(map[string]any{
		"model":       req.Model,
		"messages":    req.Messages,
		"temperature": 0.5,
		"max_tokens":  1000,
	})
	if err != nil {
		return nil, err
	}

	return &httpclient.Request{
		Method: "POST",
		URL:    "https://api.example.com/v1/chat/completions",
		Body:   body,
	}, nil
}

func (m *mockTransformer) TransformResponse(ctx context.Context, resp *httpclient.Response) (*llm.Response, error) {
	return &llm.Response{}, nil
}

func (m *mockTransformer) TransformStream(ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return nil, nil
}

func (m *mockTransformer) TransformError(ctx context.Context, err *httpclient.Error) *llm.ResponseError {
	return nil
}

func (m *mockTransformer) AggregateStreamChunks(ctx context.Context, _ *httpclient.Request, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	m.aggregateCalls++
	return m.aggregatedResponse, m.aggregatedMeta, m.aggregatedErr
}

func (m *mockTransformer) APIFormat() llm.APIFormat {
	if m.apiFormat != "" {
		return m.apiFormat
	}

	return llm.APIFormatOpenAIChatCompletion
}

func TestPersistentOutboundTransformer_TransformRequest_OriginalModelRestoration(t *testing.T) {
	tests := []struct {
		name               string
		originalModel      string
		inputModel         string
		actualModel        string
		expectedFinalModel string
	}{
		{
			name:               "no original model - should use candidate ActualModel",
			originalModel:      "",
			inputModel:         "gpt-4",
			actualModel:        "gpt-4",
			expectedFinalModel: "gpt-4",
		},
		{
			name:               "has original model - should use candidate ActualModel (not OriginalModel)",
			originalModel:      "gpt-3.5-turbo",
			inputModel:         "mapped-gpt-4",
			actualModel:        "gpt-4",
			expectedFinalModel: "gpt-4",
		},
		{
			name:               "candidate ActualModel different from input - should use ActualModel",
			originalModel:      "gpt-4",
			inputModel:         "mapped-gpt-4",
			actualModel:        "claude-3-opus",
			expectedFinalModel: "claude-3-opus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			ctx := context.Background()

			channel := &biz.Channel{
				Channel: &ent.Channel{
					ID:              1,
					Name:            "test-channel",
					SupportedModels: []string{"gpt-4", "gpt-3.5-turbo"},
					Settings:        nil,
				},
				Outbound: &mockTransformer{},
			}

			processor := &PersistentOutboundTransformer{
				wrapped: &mockTransformer{},
				state: &PersistenceState{
					OriginalModel:    tt.originalModel,
					CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
					ChannelModelsCandidates: []*ChannelModelsCandidate{
						{Channel: channel, Priority: 0, Models: []biz.ChannelModelEntry{{RequestModel: tt.inputModel, ActualModel: tt.actualModel}}},
					},
					CurrentCandidateIndex: 0,
					RequestExec:           &ent.RequestExecution{ID: 1}, // Dummy to skip creation
				},
			}

			text := "Hello"
			llmRequest := &llm.Request{
				Model: tt.inputModel,
				Messages: []llm.Message{
					{
						Role: "user",
						Content: llm.MessageContent{
							Content: &text,
						},
					},
				},
			}

			// Execute
			channelRequest, err := processor.TransformRequest(ctx, llmRequest)

			// Assert
			require.NoError(t, err)
			require.NotNil(t, channelRequest)

			// Verify model restoration in the request body
			bodyStr := string(channelRequest.Body)
			model := gjson.Get(bodyStr, "model")
			require.Equal(t, tt.expectedFinalModel, model.String())

			// Also verify the llmRequest was modified
			require.Equal(t, tt.expectedFinalModel, llmRequest.Model)
		})
	}
}

func TestPersistentOutboundTransformer_PrepareForRetry(t *testing.T) {
	// Setup
	ctx := context.Background()

	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("single model, retry should trigger 'reuse same model' logic", func(t *testing.T) {
		// Case: single model, retry should trigger "reuse same model" logic
		processor := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
					},
				},
				CurrentModelIndex: 0,
				RequestExec:       &ent.RequestExecution{ID: 1},
			},
		}

		// Execute PrepareForRetry
		// It should reset RequestExec and do not increase the CurrentModelIndex
		err := processor.PrepareForRetry(ctx)

		// Assert
		require.NoError(t, err)
		require.Zero(t, processor.state.CurrentModelIndex)
		require.Nil(t, processor.state.RequestExec)
	})

	t.Run("multiple models, retry should trigger 'reuse same model' logic", func(t *testing.T) {
		// Case: multiple models, retry should trigger "reuse same model" logic
		processor := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
						{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"},
					},
				},
				CurrentModelIndex: 0,
				RequestExec:       &ent.RequestExecution{ID: 1},
			},
		}

		// Execute PrepareForRetry
		// It should reset RequestExec and do increased the CurrentModelIndex
		err := processor.PrepareForRetry(ctx)

		// Assert
		require.NoError(t, err)
		require.Equal(t, 1, processor.state.CurrentModelIndex)
		require.Nil(t, processor.state.RequestExec)
	})
}

func TestPersistentOutboundTransformer_PrepareForRetry_UsesCandidateAPIFormatOutbound(t *testing.T) {
	ctx := context.Background()

	primaryOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	embeddingOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIEmbedding}
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: primaryOutbound,
		Outbounds: map[string]transformer.Outbound{
			llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound,
		},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: primaryOutbound,
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel:   channel,
				APIFormat: llm.APIFormatOpenAIEmbedding.String(),
				Models: []biz.ChannelModelEntry{
					{RequestModel: "text-embedding-3-small", ActualModel: "text-embedding-3-small"},
					{RequestModel: "text-embedding-3-large", ActualModel: "text-embedding-3-large"},
				},
			},
			CurrentModelIndex: 0,
			RequestExec:       &ent.RequestExecution{ID: 1},
		},
	}

	err := processor.PrepareForRetry(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processor.state.CurrentModelIndex)
	require.Same(t, embeddingOutbound, processor.wrapped)
}

func TestPersistentOutboundTransformer_NextChannel_UsesCandidateAPIFormatOutbound(t *testing.T) {
	ctx := context.Background()

	primaryOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	embeddingOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIEmbedding}
	chatChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "chat-channel",
		},
		Outbound: primaryOutbound,
	}
	embeddingChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   2,
			Name: "embedding-channel",
		},
		Outbound: primaryOutbound,
		Outbounds: map[string]transformer.Outbound{
			llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound,
		},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: primaryOutbound,
		state: &PersistenceState{
			CurrentCandidateIndex: 0,
			ChannelModelsCandidates: []*ChannelModelsCandidate{
				{
					Channel: chatChannel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4o-mini", ActualModel: "gpt-4o-mini"}},
				},
				{
					Channel:   embeddingChannel,
					APIFormat: llm.APIFormatOpenAIEmbedding.String(),
					Models:    []biz.ChannelModelEntry{{RequestModel: "text-embedding-3-small", ActualModel: "text-embedding-3-small"}},
				},
			},
		},
	}

	err := processor.NextChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processor.state.CurrentCandidateIndex)
	require.Same(t, embeddingChannel, processor.state.CurrentCandidate.Channel)
	require.Same(t, embeddingOutbound, processor.wrapped)
}

func TestSelectOutboundForCandidate(t *testing.T) {
	primaryOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	embeddingOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIEmbedding}

	t.Run("nil candidate returns nil", func(t *testing.T) {
		require.Nil(t, selectOutboundForCandidate(nil))
	})

	t.Run("candidate with nil channel returns nil", func(t *testing.T) {
		candidate := &ChannelModelsCandidate{APIFormat: llm.APIFormatOpenAIEmbedding.String()}
		require.Nil(t, selectOutboundForCandidate(candidate))
	})

	t.Run("api format set and found in outbounds returns matching outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:   &ent.Channel{ID: 1, Name: "test"},
			Outbound:  primaryOutbound,
			Outbounds: map[string]transformer.Outbound{llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound},
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: llm.APIFormatOpenAIEmbedding.String(),
		}
		require.Same(t, embeddingOutbound, selectOutboundForCandidate(candidate))
	})

	t.Run("api format set but not in outbounds falls back to channel outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:   &ent.Channel{ID: 1, Name: "test"},
			Outbound:  primaryOutbound,
			Outbounds: map[string]transformer.Outbound{},
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: llm.APIFormatOpenAIEmbedding.String(),
		}
		require.Same(t, primaryOutbound, selectOutboundForCandidate(candidate))
	})

	t.Run("nil outbounds falls back to channel outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:  &ent.Channel{ID: 1, Name: "test"},
			Outbound: primaryOutbound,
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: llm.APIFormatOpenAIEmbedding.String(),
		}
		require.Same(t, primaryOutbound, selectOutboundForCandidate(candidate))
	})

	t.Run("empty api format falls back to channel outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:   &ent.Channel{ID: 1, Name: "test"},
			Outbound:  primaryOutbound,
			Outbounds: map[string]transformer.Outbound{llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound},
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: "",
		}
		require.Same(t, primaryOutbound, selectOutboundForCandidate(candidate))
	})
}

func TestPersistentOutboundTransformer_TransformRequest_ResetsStreamCompletedForNewAttempt(t *testing.T) {
	ctx := context.Background()

	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:              1,
			Name:            "test-channel",
			SupportedModels: []string{"gpt-4"},
		},
		Outbound: &mockTransformer{},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			StreamCompleted: true,
			ChannelModelsCandidates: []*ChannelModelsCandidate{
				{Channel: channel, Priority: 0, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}}},
			},
			CurrentCandidateIndex: 0,
			RequestExec:           &ent.RequestExecution{ID: 1},
		},
	}

	text := "Hello"
	llmRequest := &llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{{
			Role: "user",
			Content: llm.MessageContent{
				Content: &text,
			},
		}},
	}

	_, err := processor.TransformRequest(ctx, llmRequest)
	require.NoError(t, err)
	require.False(t, processor.state.StreamCompleted)
}

func TestPersistentOutboundTransformer_CanRetry(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	retryableErr := &httpclient.Error{StatusCode: http.StatusTooManyRequests}
	nonRetryableErr := &httpclient.Error{StatusCode: http.StatusBadRequest}

	t.Run("no current candidate", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: nil,
			},
		}

		require.False(t, outbound.CanRetry(retryableErr))
	})

	t.Run("nil error", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
			},
		}

		require.False(t, outbound.CanRetry(nil))
	})

	t.Run("non-retryable error", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
			},
		}

		require.False(t, outbound.CanRetry(nonRetryableErr))
	})

	t.Run("skip-by-circuit-breaker should not trigger same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
						{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"},
					},
				},
				CurrentModelIndex: 0,
			},
		}

		require.False(t, outbound.CanRetry(errSkipCandidateByCircuitBreaker))
	})

	t.Run("auto-aggregate empty errors are retryable", func(t *testing.T) {
		for _, retryErr := range []error{
			fmt.Errorf("failed to auto-aggregate streaming response: %w", pipeline.ErrEmptyResponse),
			fmt.Errorf("failed to auto-aggregate streaming response: %w", pipeline.ErrEmptyStreamChunks),
			fmt.Errorf("failed to auto-aggregate streaming response: %w", pipeline.ErrEmptyAggregatedBody),
		} {
			outbound := &PersistentOutboundTransformer{
				wrapped: &mockTransformer{},
				state: &PersistenceState{
					CurrentCandidate: &ChannelModelsCandidate{
						Channel: channel,
						Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
					},
					CurrentModelIndex: 0,
				},
			}

			require.True(t, outbound.CanRetry(retryErr))
		}
	})
}

func TestShouldForceStreamingForCandidate(t *testing.T) {
	newCandidate := func(policy objects.CapabilityPolicy, apiFormat llm.APIFormat) *ChannelModelsCandidate {
		return &ChannelModelsCandidate{
			APIFormat: apiFormat.String(),
			Channel: &biz.Channel{
				Channel: &ent.Channel{
					Policies: objects.ChannelPolicies{Stream: policy},
				},
			},
		}
	}

	t.Run("supported require-stream fallback request forces streaming", func(t *testing.T) {
		require.True(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIChatCompletion),
			&llm.Request{RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIChatCompletion},
		))
	})

	t.Run("native non-stream candidate does not force streaming", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyUnlimited, llm.APIFormatOpenAIChatCompletion),
			&llm.Request{RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIChatCompletion},
		))
	})

	t.Run("unsupported embedding request does not force streaming", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIEmbedding),
			&llm.Request{RequestType: llm.RequestTypeEmbedding, APIFormat: llm.APIFormatOpenAIEmbedding},
		))
	})

	t.Run("unsupported compact request does not force streaming", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIResponseCompact),
			&llm.Request{RequestType: llm.RequestTypeCompact, APIFormat: llm.APIFormatOpenAIResponseCompact},
		))
	})

	t.Run("client requested stream keeps existing streaming path", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIChatCompletion),
			&llm.Request{Stream: lo.ToPtr(true), RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIChatCompletion},
		))
	})
}

func TestPersistentOutboundTransformer_FailurePolicyRoutingChangeRetriesUnauthorizedWithFreshKey(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:failure-policy-routing-retry?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	badKey := "sk-bad-delete-secret"
	spareKey := "sk-spare-secret"
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Failure Policy Retry Channel").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{badKey, spareKey}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			FailurePolicy: &objects.ChannelFailurePolicy{
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:      "delete-401",
					Name:    "Delete unauthorized key",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusUnauthorized},
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDeleteKey}},
				}},
			},
		}).
		SaveX(ctx)

	systemService := biz.NewSystemService(biz.SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	channelService := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		Ent:           client,
		SystemService: systemService,
		HttpClient:    httpclient.NewHttpClient(),
	})
	defer channelService.Stop()
	require.NoError(t, channelService.ReloadEnabledChannelsCache(ctx))
	currentChannel := channelService.GetEnabledChannel(ch.ID)
	require.NotNil(t, currentChannel)

	candidate := &ChannelModelsCandidate{
		Channel: currentChannel,
		Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
	}
	state := &PersistenceState{
		ChannelService:          channelService,
		CurrentCandidateIndex:   0,
		CurrentCandidate:        candidate,
		CurrentModelIndex:       0,
		ChannelModelsCandidates: []*ChannelModelsCandidate{candidate},
	}
	outbound := &PersistentOutboundTransformer{
		wrapped: currentChannel.Outbound,
		state:   state,
	}
	middleware := &performanceRecording{outbound: outbound}

	ctx = contexts.WithChannelAPIKey(ctx, badKey)
	_, err := middleware.OnOutboundRawRequest(ctx, &httpclient.Request{Method: http.MethodPost, URL: "https://api.openai.example.com/v1/chat/completions"})
	require.NoError(t, err)
	middleware.OnOutboundRawError(ctx, &httpclient.Error{StatusCode: http.StatusUnauthorized})

	require.True(t, state.FailurePolicyRoutingChanged)
	updated := client.Channel.GetX(ctx, ch.ID)
	require.NotContains(t, updated.Credentials.APIKeys, badKey)
	require.Contains(t, updated.Credentials.APIKeys, spareKey)

	require.True(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusUnauthorized}))
	require.NoError(t, outbound.PrepareForRetry(ctx))
	require.False(t, state.FailurePolicyRoutingChanged)
	require.NotContains(t, state.CurrentCandidate.Channel.Credentials.APIKeys, badKey)
	require.Equal(t, []string{spareKey}, state.CurrentCandidate.Channel.Credentials.APIKeys)
}

func TestPersistentOutboundTransformer_HasMoreChannels_DisableRetries(t *testing.T) {
	currentChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:       1,
			Name:     "retry-disabled-channel",
			Settings: &objects.ChannelSettings{DisableRetries: true},
		},
		Outbound: &mockTransformer{},
	}
	nextChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   2,
			Name: "fallback-channel",
		},
		Outbound: &mockTransformer{},
	}

	outbound := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			CurrentCandidate:        &ChannelModelsCandidate{Channel: currentChannel},
			ChannelModelsCandidates: []*ChannelModelsCandidate{{Channel: currentChannel}, {Channel: nextChannel}},
			CurrentCandidateIndex:   0,
		},
	}

	require.False(t, outbound.HasMoreChannels())
	require.False(t, outbound.CanRetry(io.EOF), "transport failures must not bypass DisableRetries")
}

func TestIsCompletedAggregatedOutboundResponse(t *testing.T) {
	t.Run("usage with completion tokens means completed", func(t *testing.T) {
		require.True(t, isCompletedAggregated(llm.ResponseMeta{Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}))
	})

	t.Run("usage with zero completion tokens is not completed", func(t *testing.T) {
		require.False(t, isCompletedAggregated(llm.ResponseMeta{Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10}}))
	})

	t.Run("response id without usage is not completed", func(t *testing.T) {
		require.False(t, isCompletedAggregated(llm.ResponseMeta{ID: "resp_123"}))
	})

	t.Run("explicit completed flag is completed", func(t *testing.T) {
		require.True(t, isCompletedAggregated(llm.ResponseMeta{ID: llm.SpeechStreamResponseID, Completed: true}))
	})

	t.Run("speech stream aggregate id alone is not completed", func(t *testing.T) {
		require.False(t, isCompletedAggregated(llm.ResponseMeta{ID: llm.SpeechStreamResponseID}))
	})

	t.Run("missing usage and id is not completed", func(t *testing.T) {
		require.False(t, isCompletedAggregated(llm.ResponseMeta{}))
	})
}

type sliceEventStream struct {
	events []*httpclient.StreamEvent
	index  int
	err    error
	closed bool
}

func createOutboundTestPrimaryDataStorage(t *testing.T, ctx context.Context, client *ent.Client) {
	t.Helper()

	_, err := client.DataStorage.Create().
		SetName("outbound-test-primary").
		SetDescription("outbound test primary storage").
		SetPrimary(true).
		SetType(datastorage.TypeDatabase).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)
}

func (s *sliceEventStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}

	s.index++
	return true
}

func (s *sliceEventStream) Current() *httpclient.StreamEvent {
	if s.index == 0 || s.index > len(s.events) {
		return nil
	}

	return s.events[s.index-1]
}

func (s *sliceEventStream) Err() error {
	return s.err
}

func (s *sliceEventStream) Close() error {
	s.closed = true
	return nil
}

func TestOutboundPersistentStream_Close_AggregatedResponsesCompletionHandling(t *testing.T) {
	ctx := context.Background()
	ctx = authz.WithTestBypass(ctx)

	t.Run("response in_progress without terminal event is not completed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		createOutboundTestPrimaryDataStorage(t, ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, systemService, usageLogService := setupTestServices(t, client)
		require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreChunks: true}))

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.in_progress", Data: []byte(`{"type":"response.in_progress"}`)}},
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: []byte(`{"id":"resp_123","status":"in_progress"}`),
		}
		state := &PersistenceState{}
		perfStart := time.Now().Add(-75 * time.Millisecond)
		perf := &biz.PerformanceRecord{StartTime: perfStart, EndTime: perfStart.Add(50 * time.Millisecond)}

		persistentStream := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, perf, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.NotEqual(t, requestexecution.StatusCompleted, dbExec.Status)
		require.Equal(t, requestexecution.StatusFailed, dbExec.Status)
		require.NotNil(t, dbExec.MetricsLatencyMs)
		require.EqualValues(t, 50, *dbExec.MetricsLatencyMs)
		require.Contains(t, dbExec.ErrorMessage, "stream ended without terminal event or completed response")
		require.Empty(t, dbExec.ResponseBody)
		require.Empty(t, dbExec.ExternalID)
		chunks, err := requestService.LoadRequestExecutionResponseChunks(ctx, dbExec)
		require.NoError(t, err)
		require.Len(t, chunks, 1)
		require.JSONEq(t, `{"event":"response.in_progress","data":{"type":"response.in_progress"}}`, string(chunks[0]))
		usageCount, err := client.UsageLog.Query().Count(ctx)
		require.NoError(t, err)
		require.Zero(t, usageCount)
		require.Equal(t, 1, transformer.aggregateCalls, "failure persistence must not aggregate chunks again")
		require.False(t, state.StreamCompleted)
	})

	t.Run("explicit stream error preserves chunks without completing", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		createOutboundTestPrimaryDataStorage(t, ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, systemService, usageLogService := setupTestServices(t, client)
		require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreChunks: true}))

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusProcessing).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		streamErr := errors.New("upstream stream failed")
		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"partial"}`)}},
			err:    streamErr,
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: []byte(`{"id":"must-not-be-persisted","status":"completed"}`),
			aggregatedMeta: llm.ResponseMeta{
				ID:        "must-not-be-persisted",
				Completed: true,
				Usage: &llm.Usage{
					CompletionTokens: 1,
				},
			},
		}
		state := &PersistenceState{}
		perfStart := time.Now().Add(-75 * time.Millisecond)
		perf := &biz.PerformanceRecord{StartTime: perfStart, EndTime: perfStart.Add(50 * time.Millisecond)}

		persistentStream := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, perf, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, dbExec.Status)
		require.NotNil(t, dbExec.MetricsLatencyMs)
		require.EqualValues(t, 50, *dbExec.MetricsLatencyMs)
		require.Equal(t, streamErr.Error(), dbExec.ErrorMessage)
		require.Empty(t, dbExec.ResponseBody)
		require.Empty(t, dbExec.ExternalID)
		chunks, err := requestService.LoadRequestExecutionResponseChunks(ctx, dbExec)
		require.NoError(t, err)
		require.Len(t, chunks, 1)
		require.JSONEq(t, `{"event":"response.output_text.delta","data":{"type":"response.output_text.delta","delta":"partial"}}`, string(chunks[0]))
		usageCount, err := client.UsageLog.Query().Count(ctx)
		require.NoError(t, err)
		require.Zero(t, usageCount)
		require.Zero(t, transformer.aggregateCalls)
		require.False(t, state.StreamCompleted)

		blockedExec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusProcessing).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		chunkWriteErr := errors.New("injected chunk persistence failure")
		var statusObservedByChunkWrite requestexecution.Status
		client.RequestExecution.Use(func(next ent.Mutator) ent.Mutator {
			return hook.RequestExecutionFunc(func(hookCtx context.Context, mutation *ent.RequestExecutionMutation) (ent.Value, error) {
				if _, writesChunks := mutation.ResponseChunks(); !writesChunks {
					return next.Mutate(hookCtx, mutation)
				}

				executionID, ok := mutation.ID()
				require.True(t, ok)
				current, queryErr := client.RequestExecution.Get(hookCtx, executionID)
				require.NoError(t, queryErr)
				statusObservedByChunkWrite = current.Status

				return nil, chunkWriteErr
			})
		})

		blockedStream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"partial"}`)}},
			err:    streamErr,
		}
		blockedPersistentStream := NewOutboundPersistentStream(ctx, blockedStream, req, blockedExec, requestService, usageLogService, transformer, nil, &PersistenceState{})
		for blockedPersistentStream.Next() {
			_ = blockedPersistentStream.Current()
		}
		require.NoError(t, blockedPersistentStream.Close())

		blockedDBExec, err := client.RequestExecution.Get(ctx, blockedExec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, statusObservedByChunkWrite, "terminal status must be stored before chunk persistence starts")
		require.Equal(t, requestexecution.StatusFailed, blockedDBExec.Status)
		require.Equal(t, streamErr.Error(), blockedDBExec.ErrorMessage)
		require.Empty(t, blockedDBExec.ResponseChunks)
	})

	t.Run("aggregated completed response without terminal event is completed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		aggregated := []byte(`{"id":"resp_456","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"hi"}`)}},
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: aggregated,
			aggregatedMeta: llm.ResponseMeta{
				ID: "resp_456",
				Usage: &llm.Usage{
					PromptTokens:     10,
					CompletionTokens: 2,
					TotalTokens:      12,
				},
			},
		}
		state := &PersistenceState{}

		persistentStream := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCompleted, dbExec.Status)
		require.JSONEq(t, string(aggregated), string(dbExec.ResponseBody))
		require.Equal(t, "resp_456", dbExec.ExternalID)
		require.True(t, state.StreamCompleted)
	})

	t.Run("canceled client with aggregated completed response is still completed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		baseCtx := ent.NewContext(ctx, client)
		project := createTestProject(t, baseCtx, client)
		ch := createTestChannel(t, baseCtx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(baseCtx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(baseCtx)
		require.NoError(t, err)

		aggregated := []byte(`{"id":"resp_codex_like","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"done"}`)}},
			err:    context.Canceled,
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: aggregated,
			aggregatedMeta: llm.ResponseMeta{
				ID: "resp_codex_like",
				Usage: &llm.Usage{
					PromptTokens:     20,
					CompletionTokens: 1,
					TotalTokens:      21,
				},
			},
		}
		state := &PersistenceState{}

		requestCtx, cancel := context.WithCancel(baseCtx)
		cancel()

		persistentStream := NewOutboundPersistentStream(requestCtx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(baseCtx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCompleted, dbExec.Status)
		require.JSONEq(t, string(aggregated), string(dbExec.ResponseBody))
		require.Equal(t, "resp_codex_like", dbExec.ExternalID)
		require.True(t, state.StreamCompleted)
	})
}

func TestOutboundPersistentStream_ProviderSuccessAggregationFailureCompletesWithEvidenceGap(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)
	createOutboundTestPrimaryDataStorage(t, ctx, client)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	_, requestService, systemService, usageLogService := setupTestServices(t, client)
	require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreChunks: true, StoreResponseBody: true}))
	req, err := client.Request.Create().SetProjectID(project.ID).SetChannelID(channel.ID).
		SetModelID("gpt-5").SetStatus(request.StatusProcessing).SetRequestBody([]byte(`{"stream":true}`)).Save(ctx)
	require.NoError(t, err)
	exec, err := client.RequestExecution.Create().SetRequestID(req.ID).SetProjectID(project.ID).
		SetChannelID(channel.ID).SetModelID("gpt-5").SetFormat("openai/responses").
		SetStatus(requestexecution.StatusProcessing).SetStream(true).SetRequestBody([]byte(`{"stream":true}`)).Save(ctx)
	require.NoError(t, err)

	aggregateErr := errors.New("malformed diagnostic tool payload")
	raw := &sliceEventStream{events: []*httpclient.StreamEvent{{
		Type: "response.completed",
		Data: []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`),
	}}}
	state := &PersistenceState{}
	stream := NewOutboundPersistentStream(ctx, raw, req, exec, requestService, usageLogService,
		&mockTransformer{apiFormat: llm.APIFormatOpenAIResponse, aggregatedErr: aggregateErr}, nil, state)
	for stream.Next() {
		_ = stream.Current()
	}
	require.NoError(t, stream.Close())

	updated, err := client.RequestExecution.Get(ctx, exec.ID)
	require.NoError(t, err)
	require.Equal(t, requestexecution.StatusCompleted, updated.Status)
	require.Empty(t, updated.ErrorMessage)
	require.Empty(t, updated.ResponseBody)
	require.NotNil(t, updated.EvidenceDisposition)
	require.Equal(t, "unavailable", updated.EvidenceDisposition.ResponseBody.Outcome)
	require.Equal(t, "stream_aggregation_incomplete", lo.FromPtr(updated.EvidenceDisposition.ResponseBody.FailureClass))
	require.NotEmpty(t, updated.ResponseChunks)
	usageCount, err := client.UsageLog.Query().Count(ctx)
	require.NoError(t, err)
	require.Zero(t, usageCount)
}

func TestOutboundPersistentStream_ResponsesAbnormalTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		eventType  string
		status     string
		wantStatus requestexecution.Status
	}{
		{name: "failed", eventType: "response.failed", status: "failed", wantStatus: requestexecution.StatusFailed},
		{name: "incomplete", eventType: "response.completed", status: "incomplete", wantStatus: requestexecution.StatusFailed},
		{name: "cancelled", eventType: "response.cancelled", status: "cancelled", wantStatus: requestexecution.StatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := authz.WithTestBypass(context.Background())
			client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
			defer client.Close()
			ctx = ent.NewContext(ctx, client)
			project := createTestProject(t, ctx, client)
			channel := createTestChannel(t, ctx, client)
			_, requestService, _, usageLogService := setupTestServices(t, client)
			req, err := client.Request.Create().SetProjectID(project.ID).SetChannelID(channel.ID).
				SetModelID("gpt-5").SetStatus(request.StatusProcessing).SetRequestBody([]byte(`{}`)).Save(ctx)
			require.NoError(t, err)
			exec, err := client.RequestExecution.Create().SetRequestID(req.ID).SetProjectID(project.ID).
				SetChannelID(channel.ID).SetModelID("gpt-5").SetFormat("openai/responses").
				SetStatus(requestexecution.StatusProcessing).SetStream(true).SetRequestBody([]byte(`{}`)).Save(ctx)
			require.NoError(t, err)
			data := []byte(fmt.Sprintf(`{"type":%q,"response":{"id":"resp_terminal","status":%q,"output":[]}}`, tc.eventType, tc.status))
			raw := &sliceEventStream{events: []*httpclient.StreamEvent{{Type: tc.eventType, Data: data}}}
			perfStart := time.Now().Add(-75 * time.Millisecond)
			perf := &biz.PerformanceRecord{StartTime: perfStart, EndTime: perfStart.Add(50 * time.Millisecond)}
			stream := NewOutboundPersistentStream(ctx, raw, req, exec, requestService, usageLogService,
				&mockTransformer{apiFormat: llm.APIFormatOpenAIResponse}, perf, &PersistenceState{})
			for stream.Next() {
				_ = stream.Current()
			}
			require.NoError(t, stream.Close())
			updated, err := client.RequestExecution.Get(ctx, exec.ID)
			require.NoError(t, err)
			require.Equal(t, tc.wantStatus, updated.Status)
			if tc.wantStatus == requestexecution.StatusFailed {
				require.NotNil(t, updated.MetricsLatencyMs)
				require.EqualValues(t, 50, *updated.MetricsLatencyMs)
			}
			require.Empty(t, updated.ResponseBody)
		})
	}
}

func TestPersistentStreams_ResponsesAbnormalTerminalKeepsParentAndExecutionConsistent(t *testing.T) {
	for _, tc := range []struct {
		name                string
		eventType           string
		status              string
		wantRequestStatus   request.Status
		wantExecutionStatus requestexecution.Status
	}{
		{
			name:                "failed",
			eventType:           "response.failed",
			status:              "failed",
			wantRequestStatus:   request.StatusFailed,
			wantExecutionStatus: requestexecution.StatusFailed,
		},
		{
			name:                "incomplete",
			eventType:           "response.completed",
			status:              "incomplete",
			wantRequestStatus:   request.StatusFailed,
			wantExecutionStatus: requestexecution.StatusFailed,
		},
		{
			name:                "cancelled",
			eventType:           "response.cancelled",
			status:              "cancelled",
			wantRequestStatus:   request.StatusCanceled,
			wantExecutionStatus: requestexecution.StatusCanceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
			t.Cleanup(func() { _ = client.Close() })
			ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
			createOutboundTestPrimaryDataStorage(t, ctx, client)
			project := createTestProject(t, ctx, client)
			channel := createTestChannel(t, ctx, client)
			_, requestService, _, usageLogService := setupTestServices(t, client)
			req := client.Request.Create().SetProjectID(project.ID).SetChannelID(channel.ID).SetModelID("gpt-5").
				SetStatus(request.StatusProcessing).SetRequestBody([]byte(`{}`)).SaveX(ctx)
			exec := client.RequestExecution.Create().SetRequestID(req.ID).SetProjectID(project.ID).SetChannelID(channel.ID).
				SetModelID("gpt-5").SetFormat("openai/responses").SetStatus(requestexecution.StatusProcessing).
				SetStream(true).SetRequestBody([]byte(`{}`)).SaveX(ctx)

			data := []byte(fmt.Sprintf(`{"type":%q,"response":{"id":"resp_terminal","status":%q,"output":[]}}`, tc.eventType, tc.status))
			event := &httpclient.StreamEvent{Type: tc.eventType, Data: data}
			state := &PersistenceState{}
			outbound := NewOutboundPersistentStream(ctx, &sliceEventStream{events: []*httpclient.StreamEvent{event}}, req, exec,
				requestService, usageLogService, &mockTransformer{apiFormat: llm.APIFormatOpenAIResponse}, nil, state)
			inbound := NewInboundPersistentStream(ctx, outbound, req, exec, requestService, &mockInboundTransformer{}, nil, state)
			require.True(t, inbound.Next())
			_ = inbound.Current()
			require.NoError(t, inbound.Close())
			require.Equal(t, tc.wantRequestStatus, client.Request.GetX(ctx, req.ID).Status)
			require.Equal(t, tc.wantExecutionStatus, client.RequestExecution.GetX(ctx, exec.ID).Status)
		})
	}
}

func TestOutboundPersistentStream_RetryAttemptTerminalStateIsIsolated(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	createOutboundTestPrimaryDataStorage(t, ctx, client)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	_, requestService, _, usageLogService := setupTestServices(t, client)
	req := client.Request.Create().SetProjectID(project.ID).SetChannelID(channel.ID).SetModelID("gpt-5").
		SetStatus(request.StatusProcessing).SetRequestBody([]byte(`{}`)).SaveX(ctx)
	makeExecution := func() *ent.RequestExecution {
		return client.RequestExecution.Create().SetRequestID(req.ID).SetProjectID(project.ID).SetChannelID(channel.ID).
			SetModelID("gpt-5").SetFormat("openai/responses").SetStatus(requestexecution.StatusProcessing).
			SetStream(true).SetRequestBody([]byte(`{}`)).SaveX(ctx)
	}
	firstExec, secondExec := makeExecution(), makeExecution()
	state := &PersistenceState{}
	transform := &mockTransformer{
		apiFormat:          llm.APIFormatOpenAIResponse,
		aggregatedResponse: []byte(`{"id":"resp_first","status":"completed","output":[]}`),
		aggregatedMeta:     llm.ResponseMeta{ID: "resp_first", Completed: true},
	}
	first := NewOutboundPersistentStream(ctx, &sliceEventStream{events: []*httpclient.StreamEvent{{
		Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"status":"completed"}}`),
	}}}, req, firstExec, requestService, usageLogService, transform, nil, state)
	require.True(t, first.Next())
	_ = first.Current()

	state.resetStreamTerminalState()
	second := NewOutboundPersistentStream(ctx, &sliceEventStream{events: []*httpclient.StreamEvent{{
		Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"status":"failed"}}`),
	}}}, req, secondExec, requestService, usageLogService, transform, nil, state)
	require.True(t, second.Next())
	_ = second.Current()

	require.NoError(t, first.Close())
	require.NoError(t, second.Close())
	require.Equal(t, requestexecution.StatusCompleted, client.RequestExecution.GetX(ctx, firstExec.ID).Status)
	require.Equal(t, requestexecution.StatusFailed, client.RequestExecution.GetX(ctx, secondExec.ID).Status)
}

func TestPersistentStreams_FinalizeExecutionBeforeParentRequest(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	createOutboundTestPrimaryDataStorage(t, ctx, client)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	_, requestService, _, usageLogService := setupTestServices(t, client)
	req := client.Request.Create().SetProjectID(project.ID).SetChannelID(channel.ID).SetModelID("gpt-5").
		SetStatus(request.StatusProcessing).SetRequestBody([]byte(`{}`)).SaveX(ctx)
	exec := client.RequestExecution.Create().SetRequestID(req.ID).SetProjectID(project.ID).SetChannelID(channel.ID).
		SetModelID("gpt-5").SetFormat("openai/responses").SetStatus(requestexecution.StatusProcessing).
		SetStream(true).SetRequestBody([]byte(`{}`)).SaveX(ctx)

	executionWasTerminal := false
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if status, ok := mutation.Status(); ok && status == request.StatusCompleted {
				current := client.RequestExecution.GetX(hookCtx, exec.ID)
				executionWasTerminal = current.Status == requestexecution.StatusCompleted
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	event := &httpclient.StreamEvent{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_order","status":"completed","output":[]}}`)}
	state := &PersistenceState{}
	outbound := NewOutboundPersistentStream(ctx, &sliceEventStream{events: []*httpclient.StreamEvent{event}}, req, exec,
		requestService, usageLogService, &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: []byte(`{"id":"resp_order","status":"completed","output":[]}`),
			aggregatedMeta:     llm.ResponseMeta{ID: "resp_order", Completed: true},
		}, nil, state)
	inbound := NewInboundPersistentStream(ctx, outbound, req, exec, requestService, &mockInboundTransformer{
		aggregateResponseBody: []byte(`{"id":"resp_order","status":"completed","output":[]}`),
		aggregateMeta:         llm.ResponseMeta{ID: "resp_order", Completed: true},
	}, nil, state)
	require.True(t, inbound.Next())
	_ = inbound.Current()
	require.NoError(t, inbound.Close())
	require.True(t, executionWasTerminal)
	require.Equal(t, request.StatusCompleted, client.Request.GetX(ctx, req.ID).Status)
	require.Equal(t, requestexecution.StatusCompleted, client.RequestExecution.GetX(ctx, exec.ID).Status)
}

func TestPersistentOutboundTransformer_TransformRequest_WithPrepopulatedState(t *testing.T) {
	// Setup
	ctx := context.Background()

	// Pre-populate channels (now done by inbound transformer)
	testChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:              1,
			Name:            "test-channel",
			SupportedModels: []string{"gpt-4", "gpt-3.5-turbo"}, // Add gpt-3.5-turbo
			Settings:        nil,
		},
		Outbound: &mockTransformer{},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			OriginalModel: "gpt-3.5-turbo",
			ChannelModelsCandidates: []*ChannelModelsCandidate{
				{Channel: testChannel, Priority: 0, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"}}},
			}, // Pre-populated by inbound
			CurrentCandidateIndex: 0,
			RequestExec:           &ent.RequestExecution{ID: 1}, // Dummy to skip creation
		},
	}

	text := "Hello"
	llmRequest := &llm.Request{
		Model: "mapped-gpt-4", // This was mapped by inbound transformer
		Messages: []llm.Message{
			{
				Role: "user",
				Content: llm.MessageContent{
					Content: &text,
				},
			},
		},
	}

	// Execute
	channelRequest, err := processor.TransformRequest(ctx, llmRequest)

	// Assert
	require.NoError(t, err)
	require.NotNil(t, channelRequest)

	// Verify original model was restored
	require.Equal(t, "gpt-3.5-turbo", llmRequest.Model)

	// Verify channel was used
	require.Equal(t, testChannel, processor.state.CurrentCandidate.Channel)
}

func TestFilterResponseCustomToolMessagesForNonResponsesOutbound(t *testing.T) {
	baseRequest := &llm.Request{
		APIFormat: llm.APIFormatOpenAIResponse,
		Messages: []llm.Message{
			{
				Role: "assistant",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_custom_1",
						Type: llm.ToolTypeResponsesCustomTool,
						ResponseCustomToolCall: &llm.ResponseCustomToolCall{
							CallID: "call_custom_1",
							Name:   "apply_patch",
							Input:  "*** Begin Patch\n*** End Patch\n",
						},
					},
					{
						ID:   "call_function_1",
						Type: llm.ToolTypeFunction,
						Function: llm.FunctionCall{
							Name:      "get_weather",
							Arguments: "{}",
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: func() *string { v := "call_custom_1"; return &v }(),
				Content: llm.MessageContent{
					Content: func() *string { v := "custom"; return &v }(),
				},
			},
			{
				Role:       "tool",
				ToolCallID: func() *string { v := "call_function_1"; return &v }(),
				Content: llm.MessageContent{
					Content: func() *string { v := "function"; return &v }(),
				},
			},
		},
	}

	t.Run("filters when inbound is responses and outbound is not", func(t *testing.T) {
		got := filterResponseCustomToolMessagesForNonResponsesOutbound(baseRequest, llm.APIFormatOpenAIChatCompletion)
		require.NotSame(t, baseRequest, got)
		require.Len(t, got.Messages, 2)
		require.Len(t, got.Messages[0].ToolCalls, 1)
		require.Equal(t, llm.ToolTypeFunction, got.Messages[0].ToolCalls[0].Type)
		require.NotNil(t, got.Messages[1].ToolCallID)
		require.Equal(t, "call_function_1", *got.Messages[1].ToolCallID)
	})

	t.Run("does not filter when outbound is responses", func(t *testing.T) {
		got := filterResponseCustomToolMessagesForNonResponsesOutbound(baseRequest, llm.APIFormatOpenAIResponse)
		require.Same(t, baseRequest, got)
	})

	t.Run("does not filter when inbound is not responses", func(t *testing.T) {
		nonResponsesReq := *baseRequest
		nonResponsesReq.APIFormat = llm.APIFormatOpenAIChatCompletion
		got := filterResponseCustomToolMessagesForNonResponsesOutbound(&nonResponsesReq, llm.APIFormatOpenAIChatCompletion)
		require.Same(t, &nonResponsesReq, got)
	})
}

// ========== 429 Retry-After Tests ==========

func TestPersistentOutboundTransformer_CanRetry_429_WithRetryAfter(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("429 with Retry-After should not retry same channel", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with Retry-After header
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"30"}},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("429 with multiple headers including Retry-After should not retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with multiple headers
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers: http.Header{
				"Retry-After":  []string{"60"},
				"Content-Type": []string{"application/json"},
			},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})
}

func TestPersistentOutboundTransformer_CanRetry_429_WithoutRetryAfter(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("429 without Retry-After (nil headers) should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error without headers
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    nil,
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("429 without Retry-After (empty headers) should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with empty headers
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("429 without Retry-After (headers but no Retry-After key) should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with headers but no Retry-After
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers: http.Header{
				"Content-Type": []string{"application/json"},
			},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})
}

func TestPersistentOutboundTransformer_CanRetry_ChannelRetryableStatusCodes(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
			Settings: &objects.ChannelSettings{
				RetryableStatusCodes: []int{http.StatusBadRequest, http.StatusForbidden},
			},
		},
		Outbound: &mockTransformer{},
	}

	outbound := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: channel,
				Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
			},
			CurrentModelIndex: 0,
		},
	}

	require.True(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusBadRequest}))
	require.True(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusForbidden}))
	require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusUnauthorized}))
}

func TestPersistentOutboundTransformer_CanRetry_429_WithMultipleModels(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("429 with Retry-After should not retry even with multiple models", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
						{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"},
					},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with Retry-After header
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"30"}},
		}

		// Should skip retry even though there are more models
		require.False(t, outbound.CanRetry(httpErr))
	})
}
