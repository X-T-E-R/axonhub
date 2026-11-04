package responses

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestWebSocketExecutorSurrogatesAndRawOrder(t *testing.T) {
	const raw = `{"z":9007199254740993,"s":"\ud83d...\udef0\ud83d\ud83d\udef0","literal":"\\ud83d","pair":"\uD83D\uDEF0","signature":"opaque","a":1}`
	const want = `{"z":9007199254740993,"s":"\ufffd...\ufffd\ufffd\ud83d\udef0","literal":"\\ud83d","pair":"\uD83D\uDEF0","signature":"opaque","a":1}`
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		for i := 0; i < 2; i++ {
			var body map[string]json.RawMessage
			require.NoError(t, conn.ReadJSON(&body))
			require.Equal(t, `[`+want+`]`, string(body["input"]))
			require.Equal(t, want, string(body["unknown"]))
			if i == 0 {
				require.NotContains(t, body, "previous_response_id")
			} else {
				require.Equal(t, `"r"`, string(body["previous_response_id"]))
			}
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`)))
		}
	}))
	defer server.Close()
	executor := NewWebSocketExecutor(nil)
	defer executor.Close()
	r := &httpclient.Request{Method: http.MethodPost, URL: server.URL + "/v1/responses", Headers: http.Header{"Session_id": {"surrogates"}}}
	for _, previous := range []string{"", `,"previous_response_id":"r"`} {
		r.Body = []byte(`{"model":"m","input":[` + raw + `],"unknown":` + raw + previous + `}`)
		stream, err := executor.DoStream(webSocketTestContext(), r)
		require.NoError(t, err)
		for stream.Next() {
		}
		require.NoError(t, stream.Err())
		require.NoError(t, stream.Close())
	}
	// Full-input prefix comparison uses the same normalized representation as
	// retained lineage, without losing order or integer precision.
	first, err := buildWebSocketCreatePayload([]byte(`{"input":[` + raw + `]}`))
	require.NoError(t, err)
	items, ok, err := payloadInputItems(first)
	require.NoError(t, err)
	require.True(t, ok)
	second, err := buildWebSocketCreatePayload([]byte(`{"input":[` + raw + `,{"role":"user","content":"next"}]}`))
	require.NoError(t, err)
	lease := &webSocketLease{pooled: &pooledWebSocketConn{lastInput: items, lastResponseID: "r"}, reused: true}
	require.NoError(t, prepareWebSocketPayloadForLease(second, lease))
	suffix, ok, err := payloadInputItems(second)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []json.RawMessage{json.RawMessage(`{"role":"user","content":"next"}`)}, suffix)
}

func TestWebSocketRawPrefixComparisonPreservesScalarAndIntegerSemantics(t *testing.T) {
	require.True(t, jsonRawEqual(json.RawMessage(`{"s":"\ufffd\ud83d\udef0","n":9007199254740993}`), json.RawMessage(`{"n":9007199254740993,"s":"�🛰"}`)))
	require.False(t, jsonRawEqual(json.RawMessage(`{"n":9007199254740992}`), json.RawMessage(`{"n":9007199254740993}`)))
}
