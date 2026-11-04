package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

const wsSurrogateRawItem = `{"z":9007199254740993,"s":"\ud83d...\udef0\ud83d\ud83d\udef0","literal":"\\ud83d","pair":"\uD83D\uDEF0","signature":"opaque","a":1}`
const wsSurrogateItem = `{"z":9007199254740993,"s":"\ufffd...\ufffd\ufffd\ud83d\udef0","literal":"\\ud83d","pair":"\uD83D\uDEF0","signature":"opaque","a":1}`

func TestResponsesWebSocketSurrogatesFullAndContinuation(t *testing.T) {
	bodies := make(chan []byte, 2)
	n := 0
	h := wsTestHandler(func(_ context.Context, req *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		bodies <- req.Body
		n++
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion(fmt.Sprintf("r%d", n), `[`+wsSurrogateRawItem+`]`)})}, nil
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	for i, previous := range []string{"", `,"previous_response_id":"r1"`} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"m","input":[`+wsSurrogateRawItem+`]`+previous+`}`)))
		require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(<-bodies, &body))
		var input []json.RawMessage
		require.NoError(t, json.Unmarshal(body["input"], &input))
		require.Len(t, input, 1+i*2)
		for _, item := range input {
			require.Equal(t, wsSurrogateItem, string(item))
		}
	}
}

func TestResponsesWebSocketSurrogateStateBudgetsAndStringInput(t *testing.T) {
	state := newResponsesWSState(time.Minute)
	f, err := parseResponsesWSCreate([]byte(`{"type":"response.create","model":"m","input":[` + wsSurrogateRawItem + `]}`))
	require.Nil(t, err)
	_, input, err := state.prepare(f, time.Now())
	require.Nil(t, err)
	require.True(t, state.commit(f, input, json.RawMessage(`{"id":"r","status":"completed","output":[`+wsSurrogateRawItem+`]}`), time.Now()))
	require.Equal(t, rawItemsBytes([]json.RawMessage{json.RawMessage(wsSurrogateItem), json.RawMessage(wsSurrogateItem)})+2, state.bytes)
	f, err = parseResponsesWSCreate([]byte(`{"type":"response.create","model":"m","input":"\ud800..."}`))
	require.Nil(t, err)
	var decoded []struct{ Content string }
	body, _, err := state.prepare(f, time.Now())
	require.Nil(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &fields))
	require.NoError(t, json.Unmarshal(fields["input"], &decoded))
	require.Equal(t, "�...", decoded[0].Content)
}
