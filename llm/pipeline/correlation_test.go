package pipeline_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/anthropic/claudecode"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type correlationTokenTransport struct{}

func (correlationTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"offline-access","expires_in":3600,"token_type":"Bearer"}`)), Request: req}, nil
}

func TestCorrelationAllOutboundProtocols(t *testing.T) {
	token := oauth.NewStaticTokenProvider(&oauth.OAuthCredentials{AccessToken: "offline-test-token"})
	factories := map[string]func() (transformer.Outbound, error){
		"anthropic": func() (transformer.Outbound, error) {
			return anthropic.NewOutboundTransformer("https://offline.invalid", "offline-key")
		},
		"claudecode": func() (transformer.Outbound, error) {
			return claudecode.NewOutboundTransformer(claudecode.Params{BaseURL: "https://offline.invalid", TokenProvider: token})
		},
		"openai": func() (transformer.Outbound, error) {
			return openai.NewOutboundTransformer("https://offline.invalid", "offline-key")
		},
		"codex": func() (transformer.Outbound, error) {
			return codex.NewOutboundTransformer(codex.Params{BaseURL: "https://offline.invalid", TokenProvider: token})
		},
		"responses": func() (transformer.Outbound, error) {
			return responses.NewOutboundTransformer("https://offline.invalid", "offline-key")
		},
		"gemini": func() (transformer.Outbound, error) {
			return gemini.NewOutboundTransformer("https://offline.invalid", "offline-key")
		},
		"antigravity": func() (transformer.Outbound, error) {
			client := httpclient.NewHttpClientWithClient(&http.Client{Transport: correlationTokenTransport{}})
			return antigravity.NewTransformer(antigravity.Config{BaseURL: "https://offline.invalid", Project: "offline-project", APIKey: "offline-refresh"}, antigravity.WithHTTPClient(client))
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			outbound, err := factory()
			require.NoError(t, err)
			captured := errors.New("offline executor captured request")
			calls := 0
			check := func(req *httpclient.Request) {
				calls++
				require.Equal(t, "ew-ray:all-protocols", req.Headers.Get(httpclient.ClientRequestIDHeader))
			}
			executor := &mockExecutor{doFunc: func(_ context.Context, req *httpclient.Request) (*httpclient.Response, error) {
				check(req)
				return nil, captured
			}, doStreamFunc: func(_ context.Context, req *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
				check(req)
				return nil, captured
			}}
			p := pipeline.NewFactory(executor).Pipeline(openai.NewInboundTransformer(), outbound)
			_, err = p.Process(t.Context(), &httpclient.Request{Method: http.MethodPost, Headers: http.Header{"Content-Type": {"application/json"}, httpclient.ClientRequestIDHeader: {"ew-ray:all-protocols"}}, Body: []byte(`{"model":"offline-model","messages":[{"role":"user","content":"offline"}],"stream":false}`)})
			require.ErrorIs(t, err, captured)
			require.Equal(t, 1, calls)
		})
	}
}
