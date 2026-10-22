package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/looplj/axonhub/llm/httpclient"

	"github.com/stretchr/testify/require"
)

func TestPrepareWebSocketPayloadExplicitDeltaUsesMatchingLiveLease(t *testing.T) {
	pc := &pooledWebSocketConn{lastInput: []json.RawMessage{json.RawMessage(`{"role":"user","content":"old"}`)}, lastResponseID: "resp_parent"}
	payload, err := buildWebSocketCreatePayload([]byte(`{"model":"m","previous_response_id":"resp_parent","input":[{"type":"function_call_output","call_id":"call_1","output":"result"}]}`))
	require.NoError(t, err)
	lease := &webSocketLease{pooled: pc, reused: true}
	require.NoError(t, prepareWebSocketPayloadForLease(payload, lease))
	items, ok, err := payloadInputItems(payload)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, items, 1)
	require.JSONEq(t, `{"type":"function_call_output","call_id":"call_1","output":"result"}`, string(items[0]))
	require.Nil(t, lease.nextFullInput, "explicit delta is not a full canonical input baseline")
}

func TestPrepareWebSocketPayloadExplicitDeltaRejectsMissingLiveParent(t *testing.T) {
	for _, lease := range []*webSocketLease{nil, {}, {pooled: &pooledWebSocketConn{}, reused: false}, {pooled: &pooledWebSocketConn{lastResponseID: "other"}, reused: true}} {
		payload, err := buildWebSocketCreatePayload([]byte(`{"previous_response_id":"resp_parent","input":[{"role":"user","content":"delta"}]}`))
		require.NoError(t, err)
		require.ErrorContains(t, prepareWebSocketPayloadForLease(payload, lease), "previous_response_not_found")
	}
}

func TestWebSocketCreatePayloadPreservesLargeOpaqueIntegers(t *testing.T) {
	payload, err := buildWebSocketCreatePayload([]byte(`{"input":[{"type":"opaque","token":9007199254740993}]}`))
	require.NoError(t, err)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.Contains(t, string(raw), "9007199254740993")
}

func TestWebSocketExecutorNativeDeltaChainReusesAuthenticatedLiveLease(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		for i := 1; i <= 3; i++ {
			var body map[string]json.RawMessage
			require.NoError(t, conn.ReadJSON(&body))
			if i == 1 {
				require.NotContains(t, body, "previous_response_id")
			} else {
				require.JSONEq(t, fmt.Sprintf(`"resp_%d"`, i-1), string(body["previous_response_id"]))
				var input []json.RawMessage
				require.NoError(t, json.Unmarshal(body["input"], &input))
				require.Len(t, input, 1)
				require.Contains(t, string(input[0]), "function_call_output")
			}
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", i), "status": "completed", "output": []any{}}}))
		}
	}))
	defer server.Close()
	executor := NewWebSocketExecutor(nil)
	defer executor.Close()
	request := &httpclient.Request{Method: http.MethodPost, URL: server.URL + "/v1/responses", Headers: http.Header{"Session_id": []string{"chain"}}, Auth: &httpclient.AuthConfig{Type: httpclient.AuthTypeBearer, APIKey: "test-key"}}
	for i := 1; i <= 3; i++ {
		if i == 1 {
			request.Body = []byte(`{"model":"m","input":[{"role":"user","content":"full"}]}`)
		} else {
			request.Body = []byte(fmt.Sprintf(`{"model":"m","previous_response_id":"resp_%d","input":[{"type":"function_call_output","call_id":"call","output":"delta"}]}`, i-1))
		}
		stream, err := executor.DoStream(webSocketTestContext(), request)
		require.NoError(t, err)
		require.True(t, stream.Next())
		require.False(t, stream.Next())
		require.NoError(t, stream.Err())
		require.NoError(t, stream.Close())
	}
	require.Equal(t, int32(1), connections.Load())
}
