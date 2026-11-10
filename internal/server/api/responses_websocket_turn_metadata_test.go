package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

const wsMetadataBaseline = `{"session_id":"session","thread_id":"thread","installation_id":"installation","window_id":"window","turn_id":"handshake-turn","old_option":true}`

func wsMetadataFrame(t *testing.T, metadata any) responsesWSCreate {
	t.Helper()
	body, err := json.Marshal(map[string]any{"type": "response.create", "model": "m", "input": "current", "client_metadata": metadata})
	require.NoError(t, err)
	frame, rejection := parseResponsesWSCreate(body)
	require.Nil(t, rejection)
	return frame
}

func TestResponsesWebSocketTurnMetadataContract(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata any
		headers  []string
		want     string
		invalid  bool
	}{
		{name: "absent key preserves invalid legacy handshake", headers: []string{"legacy-not-json"}},
		{name: "unrelated keys never map to headers", metadata: map[string]any{"Authorization": "Bearer replacement", "session_id": "other", "X-Codex-Turn-Metadata": `{}`}},
		{name: "nonobject container has no opt-in key", metadata: "legacy"},
		{name: "no handshake no fixed identity invented", metadata: map[string]any{responsesWSTurnMetadataKey: `{"turn_id":"turn-2","request_kind":"turn"}`}, want: `{"turn_id":"turn-2","request_kind":"turn"}`},
		{name: "present empty object is opt-in", metadata: map[string]any{responsesWSTurnMetadataKey: `{}`}, want: `{}`},
		{name: "preserve frame text and opaque numbers", metadata: map[string]any{responsesWSTurnMetadataKey: ` {"turn_id":"turn-2","future":9007199254740993} `}, headers: []string{`{}`}, want: ` {"turn_id":"turn-2","future":9007199254740993} `},
		{name: "inherit only baseline fixed identity", metadata: map[string]any{responsesWSTurnMetadataKey: `{"turn_id":"turn-2","future":9007199254740993}`}, headers: []string{wsMetadataBaseline}, want: `{"session_id":"session","thread_id":"thread","installation_id":"installation","window_id":"window","turn_id":"turn-2","future":9007199254740993}`},
		{name: "matching escaped fixed identity", metadata: map[string]any{responsesWSTurnMetadataKey: `{"session_id":"\u0073ession","turn_id":"turn-2"}`}, headers: []string{wsMetadataBaseline}, want: `{"session_id":"session","thread_id":"thread","installation_id":"installation","window_id":"window","turn_id":"turn-2"}`},
		{name: "invalid handshake disallows opt-in", metadata: map[string]any{responsesWSTurnMetadataKey: `{}`}, headers: []string{"legacy-not-json"}, invalid: true},
		{name: "empty present handshake disallows opt-in", metadata: map[string]any{responsesWSTurnMetadataKey: `{}`}, headers: []string{""}, invalid: true},
		{name: "multiple handshake values disallow opt-in", metadata: map[string]any{responsesWSTurnMetadataKey: `{}`}, headers: []string{`{}`, `{}`}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{}
			if test.headers != nil {
				headers[codex.TurnMetadataHeader] = test.headers
			}
			frame := wsMetadataFrame(t, test.metadata)
			original, err := json.Marshal(frame.body)
			require.NoError(t, err)
			got, rejection := responsesWSTurnMetadata(frame, headers)
			if test.invalid {
				require.NotNil(t, rejection)
				require.Equal(t, "invalid_turn_metadata", rejection.code)
			} else {
				require.Nil(t, rejection)
				if test.want == "" {
					require.Empty(t, got)
				} else {
					require.JSONEq(t, test.want, got)
				}
			}
			after, err := json.Marshal(frame.body)
			require.NoError(t, err)
			require.Equal(t, original, after, "projection must not change the request body")
		})
	}
	invalidValues := []any{nil, true, 12, map[string]any{}, []any{}, "", "not-json", `null`, `[]`, `"text"`, `{"turn_id":"x"} {}`, "\n{}", "{}\r\nInjected: yes", "{\x00}", "{}\x7f", `{"session_id":"other"}`, `{"SESSION_ID":"session"}`, `{"turn_id":"a","turn_id":"b"}`, `{"session_id":null}`}
	for _, value := range invalidValues {
		frame := wsMetadataFrame(t, map[string]any{responsesWSTurnMetadataKey: value})
		_, rejection := responsesWSTurnMetadata(frame, http.Header{codex.TurnMetadataHeader: {wsMetadataBaseline}})
		require.NotNil(t, rejection, "%#v", value)
		require.Equal(t, http.StatusBadRequest, rejection.status)
		require.Equal(t, "invalid_turn_metadata", rejection.code)
		require.True(t, rejection.notExecuted)
	}
	for _, key := range responsesWSFixedTurnMetadataKeys {
		for _, baseline := range []string{"", `{}`, wsMetadataBaseline} {
			metadata, err := json.Marshal(map[string]string{key: "new-identity"})
			require.NoError(t, err)
			headers := http.Header{}
			if baseline != "" {
				headers.Set(codex.TurnMetadataHeader, baseline)
			}
			_, rejection := responsesWSTurnMetadata(wsMetadataFrame(t, map[string]any{responsesWSTurnMetadataKey: string(metadata)}), headers)
			require.NotNil(t, rejection, "%s baseline %s", key, baseline)
			require.Equal(t, "invalid_turn_metadata", rejection.code)
		}
	}
}

func TestResponsesWebSocketTurnMetadataRejectsBeforeInferenceAndNeverSticks(t *testing.T) {
	var calls atomic.Int32
	headers := http.Header{"Authorization": {"Bearer original"}, codex.TurnMetadataHeader: {wsMetadataBaseline}, "Session-Id": {"independent-session"}, "Thread-Id": {"independent-thread"}, "X-Codex-Installation-Id": {"independent-installation"}, "X-Codex-Window-Id": {"independent-window"}}
	h := wsTestHandler(func(ctx context.Context, req *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		n := calls.Add(1)
		for _, key := range []string{"Authorization", "Session-Id", "Thread-Id", "X-Codex-Installation-Id", "X-Codex-Window-Id"} {
			require.Equal(t, headers.Get(key), req.Headers.Get(key))
		}
		scope, ok := shared.GetSessionScope(ctx)
		require.True(t, ok)
		require.Equal(t, "test-scope", scope)
		if n == 1 {
			require.Contains(t, req.Headers.Get(codex.TurnMetadataHeader), `"turn_id":"frame-turn"`)
			require.NotContains(t, req.Headers.Get(codex.TurnMetadataHeader), "handshake-turn")
		} else {
			require.Equal(t, wsMetadataBaseline, req.Headers.Get(codex.TurnMetadataHeader))
			_, projected := shared.ResponsesWebSocketTurnMetadata(ctx)
			require.False(t, projected)
		}
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion("done", `[]`)})}, nil
	})
	authenticate := h.authenticate
	h.authenticate = func(c *gin.Context) (context.Context, int, error) {
		for name := range headers {
			require.Equal(t, headers.Get(name), c.Request.Header.Get(name), "authentication must see the immutable handshake")
		}
		return authenticate(c)
	}
	server := wsTestServer(t, h)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var cap map[string]any
	require.NoError(t, json.Unmarshal([]byte(response.Header.Get(responsesWSCapabilityHeader)), &cap))
	require.Equal(t, true, cap["codex_turn_metadata_per_frame"])
	send := func(metadata any) {
		frame := wsMetadataFrame(t, metadata)
		require.NoError(t, conn.WriteJSON(frame.body))
	}
	send(map[string]any{responsesWSTurnMetadataKey: `{"turn_id":"frame-turn"}`, "Authorization": "Bearer never-map"})
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	for _, value := range []any{false, `{"session_id":"changed"}`, `[]`} {
		send(map[string]any{responsesWSTurnMetadataKey: value})
		event := wsRead(t, conn)
		require.Equal(t, "400", string(event["status"]))
		require.Equal(t, "invalid_turn_metadata", wsErrorCode(event))
		require.Equal(t, "not_executed", wsEventString(event, "execution"))
		require.Equal(t, int32(1), calls.Load())
	}
	send(map[string]any{"turn_id": "ignored"})
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	require.Equal(t, int32(2), calls.Load())
}
