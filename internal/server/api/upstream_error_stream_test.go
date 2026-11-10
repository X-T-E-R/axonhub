package api

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestUpstreamErrorStreamReadsPolicyOnlyForErrors(t *testing.T) {
	ctx, system := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModeCustom, CustomMessage: "safe message"})
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	system.Cache = xcache.NewRedisWithDomain[ent.System](client, xcache.DefaultNamespace, "system")
	_, err := system.RetryPolicy(ctx)
	require.NoError(t, err)
	slow := &slowRetryPolicyRedis{}
	client.AddHook(slow)

	for _, tc := range []struct {
		name  string
		event *httpclient.StreamEvent
		error bool
	}{
		{name: "nil"},
		{name: "empty", event: &httpclient.StreamEvent{Type: "ping"}},
		{name: "comment", event: &httpclient.StreamEvent{Data: []byte(": keep-alive")}},
		{name: "binary", event: &httpclient.StreamEvent{Type: "audio/pcm", Data: []byte{0, 1, 2}}},
		{name: "malformed error", event: &httpclient.StreamEvent{Type: "error", Data: []byte("{")}},
		{name: "done", event: &httpclient.StreamEvent{Data: []byte("[DONE]")}},
		{name: "text", event: &httpclient.StreamEvent{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"fixture"}`)}},
		{name: "completed", event: &httpclient.StreamEvent{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"status":"completed","error":null}}`)}},
		{name: "incomplete", event: &httpclient.StreamEvent{Type: "response.incomplete", Data: []byte(`{"type":"response.incomplete","response":{"status":"incomplete"}}`)}},
		{name: "null error", event: &httpclient.StreamEvent{Data: []byte(`{"error":null,"choices":[]}`)}},
		{name: "responses failed", event: &httpclient.StreamEvent{Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"error":{"message":"provider detail","details":"private"}}}`)}, error: true},
		{name: "responses flat error", event: &httpclient.StreamEvent{Data: []byte(`{"type":"error","message":"provider detail","code":"fixture_error","details":"private"}`)}, error: true},
		{name: "error envelope", event: &httpclient.StreamEvent{Data: []byte(`{"error":{"message":"provider detail","type":"server_error","code":"fixture_error"}}`)}, error: true},
		{name: "typed error", event: &httpclient.StreamEvent{Type: "error", Data: []byte(`{"message":"provider detail"}`)}, error: true},
		{name: "event marker", event: &httpclient.StreamEvent{Data: []byte(`{"event":"error","message":"provider detail"}`)}, error: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slow.gets.Load()
			stream := newUpstreamErrorStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{tc.event}), system)
			require.True(t, stream.Next())
			got := stream.Current()
			if tc.error {
				require.Contains(t, string(got.Data), "safe message")
				require.NotContains(t, string(got.Data), "provider detail")
				require.NotContains(t, string(got.Data), "private")
				require.Equal(t, int32(1), slow.gets.Load()-before)
			} else {
				require.Same(t, tc.event, got)
				require.Equal(t, int32(0), slow.gets.Load()-before)
			}
			require.False(t, stream.Next())
			require.NoError(t, stream.Err())
			require.NoError(t, stream.Close())
		})
	}
}

func TestUpstreamErrorStreamUsesCurrentPolicyAtFailure(t *testing.T) {
	ctx, system := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModePassthrough})
	delta := &httpclient.StreamEvent{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"fixture"}`)}
	failure := &httpclient.StreamEvent{Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"error":{"message":"provider detail","code":"fixture_code","details":"private"}}}`)}
	stream := newUpstreamErrorStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{delta, failure, failure}), system)
	require.True(t, stream.Next())
	require.Same(t, delta, stream.Current())
	require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{UpstreamErrorPolicy: biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModeCustom, CustomMessage: "updated after stream started"}}))
	require.True(t, stream.Next())
	rewritten := stream.Current()
	require.Contains(t, string(rewritten.Data), "updated after stream started")
	require.Contains(t, string(rewritten.Data), "fixture_code")
	require.NotContains(t, string(rewritten.Data), "provider detail")
	require.NotContains(t, string(rewritten.Data), "private")
	// An error policy is not frozen for the life of a stream.
	require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{UpstreamErrorPolicy: biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModePassthrough}}))
	require.True(t, stream.Next())
	require.Same(t, failure, stream.Current())
	require.False(t, stream.Next())
	require.NoError(t, stream.Err())
	require.NoError(t, stream.Close())
}
