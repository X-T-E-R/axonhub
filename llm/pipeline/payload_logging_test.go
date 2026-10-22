package pipeline_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/logpolicy"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/stretchr/testify/require"
)

func TestSensitivePipelineAndResponsesPayloadLogging(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"payload-sentinel\"}]}]}}\n\n")
	}))
	defer server.Close()
	outbound, err := responses.NewOutboundTransformer(server.URL, "test-key")
	require.NoError(t, err)
	for _, suppressed := range []bool{true, false} {
		logs.Reset()
		ctx := context.Background()
		if suppressed {
			ctx = logpolicy.WithoutPayloads(ctx)
		}
		pipe := pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(responses.NewInboundTransformer(), outbound)
		result, err := pipe.Process(ctx, &httpclient.Request{Method: http.MethodPost, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"model":"m","stream":true,"input":"payload-sentinel"}`)})
		require.NoError(t, err)
		require.True(t, result.Stream)
		for result.EventStream.Next() {
			_ = result.EventStream.Current()
		}
		require.NoError(t, result.EventStream.Err())
		require.NoError(t, result.EventStream.Close())
		if suppressed {
			require.NotContains(t, logs.String(), "payload-sentinel")
		} else {
			require.Contains(t, logs.String(), "payload-sentinel")
		}
	}
}
