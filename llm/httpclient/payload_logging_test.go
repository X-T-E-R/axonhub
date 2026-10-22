package httpclient

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looplj/axonhub/llm/logpolicy"
	"github.com/stretchr/testify/require"
)

func TestSensitivePayloadLoggingIsRequestScoped(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: payload-sentinel\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"text":"payload-sentinel"}`))
	}))
	defer server.Close()
	client := NewHttpClient()
	for _, suppressed := range []bool{true, false} {
		logs.Reset()
		ctx := context.Background()
		if suppressed {
			ctx = logpolicy.WithoutPayloads(ctx)
		}
		_, err := client.Do(ctx, &Request{Method: http.MethodPost, URL: server.URL, Body: []byte(`{"input":"payload-sentinel"}`)})
		require.NoError(t, err)
		stream, err := client.DoStream(ctx, &Request{Method: http.MethodPost, URL: server.URL + "/stream", Body: []byte(`{"input":"payload-sentinel"}`)})
		require.NoError(t, err)
		for stream.Next() {
			_ = stream.Current()
		}
		require.NoError(t, stream.Err())
		require.NoError(t, stream.Close())
		if suppressed {
			require.NotContains(t, logs.String(), "payload-sentinel")
		} else {
			require.Contains(t, logs.String(), "payload-sentinel")
		}
	}
}
