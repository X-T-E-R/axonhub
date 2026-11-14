package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestPersistentOutboundTransformer_MultiKeyAuthFailureRetriesAgainstFixture(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests, 432} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			testMultiKeyCredentialRetryAgainstFixture(t, status)
		})
	}
}

func testMultiKeyCredentialRetryAgainstFixture(t *testing.T, failureStatus int) {
	var mu sync.Mutex
	var authorization []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorization = append(authorization, r.Header.Get("Authorization"))
		mu.Unlock()

		if r.Header.Get("Authorization") == "Bearer bad-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failureStatus)
			_, _ = w.Write([]byte(`{"error":{"message":"bad credential","type":"authentication_error"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture-ok","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), enttest.NewEntClient(t, "sqlite3", "file:credential-retry-"+strconv.Itoa(failureStatus)+"?mode=memory&_fk=0")))
	client := ent.FromContext(ctx)
	t.Cleanup(func() { _ = client.Close() })

	row := client.Channel.Create().
		SetName("credential retry fixture").
		SetType(channel.TypeOpenai).
		SetBaseURL(upstream.URL + "/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"bad-key", "good-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SaveX(ctx)

	channelService := biz.NewChannelServiceForTest(client)
	defer channelService.Stop()
	fixtureChannel, err := channelService.GetChannel(ctx, row.ID)
	require.NoError(t, err)
	require.NotNil(t, fixtureChannel)
	require.Equal(t, 2, fixtureChannel.RoutableCredentialCount())

	state := &PersistenceState{
		ChannelService:          channelService,
		CurrentCandidateIndex:   0,
		CurrentCandidate:        &ChannelModelsCandidate{Channel: fixtureChannel},
		ChannelModelsCandidates: []*ChannelModelsCandidate{{Channel: fixtureChannel, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}}}},
	}
	outbound := &PersistentOutboundTransformer{wrapped: fixtureChannel.Outbound, state: state}
	state.CurrentCandidate.Models = []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}}
	inbound := openai.NewInboundTransformer()
	perf := &performanceRecording{outbound: outbound}

	ctx = contexts.EnsureContainer(ctx)
	pipe := pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(
		inbound,
		outbound,
		pipeline.WithRetry(0, 1, 0),
		pipeline.WithMiddlewares(perf),
	)
	body, err := json.Marshal(map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	require.NoError(t, err)

	result, err := pipe.Process(ctx, &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "/v1/chat/completions",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    body,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Response)
	require.Equal(t, http.StatusOK, result.Response.StatusCode)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"Bearer bad-key", "Bearer good-key"}, authorization)
}

type credentialSelectionOutbound struct {
	mockTransformer
	selectCredential func(context.Context)
}

func (m *credentialSelectionOutbound) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	if m.selectCredential != nil {
		m.selectCredential(ctx)
	}
	return m.mockTransformer.TransformRequest(ctx, req)
}

type credentialRetryCustomizer struct {
	credentialSelectionOutbound
	calls int
	base  pipeline.Executor
}

func (m *credentialRetryCustomizer) CustomizeExecutor(executor pipeline.Executor) pipeline.Executor {
	m.calls++
	m.base = executor
	return executor
}

func (m *credentialRetryCustomizer) TransformStream(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return nil, nil
}

func TestPersistentOutboundTransformer_SelectedCredentialEgressWinsOverCustomizer(t *testing.T) {
	selected := httpclient.NewHttpClient()
	customizer := &credentialRetryCustomizer{}
	customizer.selectCredential = func(ctx context.Context) {
		contexts.WithChannelCredentialEgress(ctx, "selected", selected, nil)
	}
	ch := &biz.Channel{
		Channel:  &ent.Channel{ID: 72, Name: "egress-owner", Settings: &objects.ChannelSettings{}},
		Outbound: customizer,
	}
	state := &PersistenceState{
		CurrentCandidate:        &ChannelModelsCandidate{Channel: ch, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}},
		ChannelModelsCandidates: []*ChannelModelsCandidate{{Channel: ch, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}}},
	}
	outbound := &PersistentOutboundTransformer{state: state}
	ctx := contexts.EnsureContainer(context.Background())
	_, err := outbound.TransformRequest(ctx, &llm.Request{Model: "gpt-4"})
	require.NoError(t, err)
	got := outbound.CustomizeExecutor(&sequenceExecutor{})
	require.Same(t, selected, got)
	require.Same(t, selected, customizer.base)
	require.Equal(t, 1, customizer.calls)
}

func TestPersistentOutboundTransformer_TransformRequestRejectsExplicitEmptySelectionButPreservesOAuth(t *testing.T) {
	callbackCalls := 0
	emptyOutbound := &credentialSelectionOutbound{selectCredential: func(ctx context.Context) {
		contexts.WithChannelCredentialEgress(ctx, "", nil, func(int, bool, bool) { callbackCalls++ })
	}}
	channel := &biz.Channel{
		Channel:  &ent.Channel{ID: 73, Name: "empty-selection", Credentials: objects.ChannelCredentials{APIKeys: []string{"key"}}, Settings: &objects.ChannelSettings{}},
		Outbound: emptyOutbound,
	}
	state := &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: channel, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}}, ChannelModelsCandidates: []*ChannelModelsCandidate{{Channel: channel, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}}}}
	outbound := &PersistentOutboundTransformer{state: state}
	ctx := contexts.EnsureContainer(context.Background())
	_, err := outbound.TransformRequest(ctx, &llm.Request{Model: "gpt-4"})
	require.ErrorContains(t, err, "no eligible API key")
	require.Equal(t, 1, callbackCalls)

	oauthChannel := &biz.Channel{
		Channel:  &ent.Channel{ID: 74, Name: "oauth-selection", Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{AccessToken: "oauth-token"}}, Settings: &objects.ChannelSettings{}},
		Outbound: &mockTransformer{},
	}
	oauthState := &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: oauthChannel, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}}, ChannelModelsCandidates: []*ChannelModelsCandidate{{Channel: oauthChannel, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}}}}
	oauthOutbound := &PersistentOutboundTransformer{state: oauthState}
	oauthCtx := contexts.EnsureContainer(context.Background())
	_, err = oauthOutbound.TransformRequest(oauthCtx, &llm.Request{Model: "gpt-4"})
	require.NoError(t, err)
}

func TestPersistentOutboundTransformer_CredentialRetryPreservesSingleKey429AndDisableRetries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		keys          []string
		disable       bool
		wantSameRetry bool
	}{
		{name: "single key 429", keys: []string{"only-key"}},
		{name: "disabled retries", keys: []string{"bad-key", "good-key"}, disable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contexts.EnsureContainer(context.Background())
			ch := &biz.Channel{
				Channel: &ent.Channel{
					ID:          71,
					Name:        "retry guard",
					Credentials: objects.ChannelCredentials{APIKeys: tc.keys},
					Settings:    &objects.ChannelSettings{DisableRetries: tc.disable},
				},
			}
			state := &PersistenceState{
				CurrentCandidate:        &ChannelModelsCandidate{Channel: ch, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}}},
				ChannelModelsCandidates: []*ChannelModelsCandidate{{Channel: ch, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}}}},
			}
			outbound := &PersistentOutboundTransformer{state: state}
			ctx = contexts.WithChannelAPIKey(ctx, tc.keys[0])
			require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusTooManyRequests}))
		})
	}
}
