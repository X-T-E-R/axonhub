package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

type capturedUpstreamReq struct {
	body    []byte
	headers http.Header
}

func setupAnthropicThreadTestRouter(
	t *testing.T,
	chType channel.Type,
	upstreamURL string,
	passThroughBody bool,
	outbound transformer.Outbound,
	store *AnthropicThreadStore,
) (*gin.Engine, context.Context, int) {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:%s?mode=memory&_fk=0", strings.ReplaceAll(t.Name(), "/", "_")))
	t.Cleanup(func() { _ = client.Close() })

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("thread-project").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)
	ctx = contexts.WithAPIKey(ctx, &ent.APIKey{ID: 101})

	chatHandlers := setupErrorPassthroughHandler(
		t, ctx, client, chType, upstreamURL, passThroughBody,
		anthropic.NewInboundTransformer(), outbound, nil,
	)
	chatHandlers.sseHeartbeatFormat = sseHeartbeatAnthropic

	if store == nil {
		store = NewAnthropicThreadStore()
	}
	anthropicHandlers := &AnthropicHandlers{
		ChatCompletionHandlers: chatHandlers,
		ThreadStore:            store,
	}

	transport := middleware.NewRequestBodyTransport()
	router := gin.New()
	router.Use(middleware.WithDecodedRequestBody(transport))
	router.POST("/v1/messages", anthropicHandlers.CreateMessage)
	router.POST("/v1/messages/count_tokens", anthropicHandlers.CountTokens)
	router.POST("/anthropic/v1/messages", anthropicHandlers.CreateMessage)
	router.POST("/anthropic/v1/messages/count_tokens", anthropicHandlers.CountTokens)

	return router, ctx, project.ID
}

func postJSON(
	t *testing.T,
	router *gin.Engine,
	ctx context.Context,
	path string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func threadSSEEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func writeAnthropicSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, ev := range events {
		_, _ = io.WriteString(w, ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func TestAnthropicMessageThreads_MultiTurnPassThroughSSE(t *testing.T) {
	var (
		mu       sync.Mutex
		received []capturedUpstreamReq
		turnIdx  int
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		received = append(received, capturedUpstreamReq{body: body, headers: r.Header.Clone()})
		turnIdx++
		cur := turnIdx
		mu.Unlock()

		switch cur {
		case 1:
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_turn1","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Checking video and "}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"calling tool."}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_gemini_"}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"exact_==/+"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"enc_redacted_1"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Let me look up Tokyo."}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":2}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"Tokyo\"}"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":3}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		case 2:
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_turn2","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":20,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Tokyo is 22C and sunny."}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":10}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		case 3:
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_turn3","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":30,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"All done!"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		}
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)

	headers := map[string]string{
		"anthropic-beta": "message-threads-2026-08-12, interleaved-thinking-2025-05-14",
	}

	// Turn 1: create with system, tools, thinking, video & audio blocks.
	turn1Body := `{
		"model": "fixture-model",
		"max_tokens": 2048,
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 4096},
		"system": [{"type": "text", "text": "You are terse.", "cache_control": {"type": "ephemeral"}}],
		"tools": [{"name": "get_weather", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}}],
		"messages": [{
			"role": "user",
			"content": [
				{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": "AAAAVIDEOBASE64"}},
				{"type": "audio", "source": {"type": "base64", "media_type": "audio/mp3", "data": "BBBBAAUDIOBASE64"}},
				{"type": "text", "text": "Check weather in video city", "cache_control": {"type": "ephemeral"}}
			]
		}],
		"thread": {"type": "create"}
	}`
	rec1 := postJSON(t, router, ctx, "/v1/messages", turn1Body, headers)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	require.Contains(t, rec1.Body.String(), `"id":"msg_turn1"`)

	// Turn 2: continue with tool_result, updated Free field max_tokens=4096, no system/tools.
	turn2Body := `{
		"model": "fixture-model",
		"max_tokens": 4096,
		"stream": true,
		"thinking": {"budget_tokens": 4096, "type": "enabled"},
		"messages": [{
			"role": "user",
			"content": [
				{"type": "tool_result", "tool_use_id": "toolu_01", "content": "22C sunny", "cache_control": {"type": "ephemeral"}}
			]
		}],
		"thread": {"type": "continue", "previous_message_id": "msg_turn1"}
	}`
	rec2 := postJSON(t, router, ctx, "/v1/messages", turn2Body, headers)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Contains(t, rec2.Body.String(), `"id":"msg_turn2"`)

	// Turn 3: continue via /anthropic/v1/messages alias.
	turn3Body := `{
		"model": "fixture-model",
		"max_tokens": 1024,
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 4096},
		"messages": [{"role": "user", "content": "Thanks!"}],
		"thread": {"type": "continue", "previous_message_id": "msg_turn2"}
	}`
	rec3 := postJSON(t, router, ctx, "/anthropic/v1/messages", turn3Body, map[string]string{
		"Anthropic-Beta": "interleaved-thinking-2025-05-14, message-threads-2026-08-12",
	})
	require.Equal(t, http.StatusOK, rec3.Code, rec3.Body.String())
	require.Contains(t, rec3.Body.String(), `"id":"msg_turn3"`)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 3)

	// Verify outbound Turn 1 stripped thread and threads beta, kept other beta.
	require.False(t, gjson.GetBytes(received[0].body, "thread").Exists())
	require.Equal(t, "interleaved-thinking-2025-05-14", received[0].headers.Get("Anthropic-Beta"))

	// Verify outbound Turn 2 materialized full conversation with lossless video/audio/thinking signature.
	req2 := received[1]
	require.False(t, gjson.GetBytes(req2.body, "thread").Exists())
	require.Equal(t, "interleaved-thinking-2025-05-14", req2.headers.Get("Anthropic-Beta"))
	require.Equal(t, int64(4096), gjson.GetBytes(req2.body, "max_tokens").Int())
	require.Equal(t, "You are terse.", gjson.GetBytes(req2.body, "system.0.text").String())
	require.Equal(t, "get_weather", gjson.GetBytes(req2.body, "tools.0.name").String())

	msgs2 := gjson.GetBytes(req2.body, "messages").Array()
	require.Len(t, msgs2, 3)
	require.Equal(t, "video", msgs2[0].Get("content.0.type").String())
	require.Equal(t, "AAAAVIDEOBASE64", msgs2[0].Get("content.0.source.data").String())
	require.Equal(t, "audio", msgs2[0].Get("content.1.type").String())
	require.Equal(t, "BBBBAAUDIOBASE64", msgs2[0].Get("content.1.source.data").String())

	require.Equal(t, "assistant", msgs2[1].Get("role").String())
	require.Equal(t, "thinking", msgs2[1].Get("content.0.type").String())
	require.Equal(t, "Checking video and calling tool.", msgs2[1].Get("content.0.thinking").String())
	require.Equal(t, "sig_gemini_exact_==/+", msgs2[1].Get("content.0.signature").String())
	require.Equal(t, "redacted_thinking", msgs2[1].Get("content.1.type").String())
	require.Equal(t, "enc_redacted_1", msgs2[1].Get("content.1.data").String())
	require.Equal(t, "text", msgs2[1].Get("content.2.type").String())
	require.Equal(t, "Let me look up Tokyo.", msgs2[1].Get("content.2.text").String())
	require.Equal(t, "tool_use", msgs2[1].Get("content.3.type").String())
	require.Equal(t, "toolu_01", msgs2[1].Get("content.3.id").String())
	require.Equal(t, "Tokyo", msgs2[1].Get("content.3.input.city").String())

	require.Equal(t, "tool_result", msgs2[2].Get("content.0.type").String())
	require.Equal(t, "22C sunny", msgs2[2].Get("content.0.content").String())

	// Verify outbound Turn 3 has 5 messages total.
	msgs3 := gjson.GetBytes(received[2].body, "messages").Array()
	require.Len(t, msgs3, 5)
	require.Equal(t, "Tokyo is 22C and sunny.", msgs3[3].Get("content.0.text").String())
	require.Equal(t, "Thanks!", msgs3[4].Get("content").String())
}

func TestAnthropicMessageThreads_NonStreamAndTransformedOpenAIChannel(t *testing.T) {
	var (
		mu       sync.Mutex
		received []capturedUpstreamReq
		callNum  int
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		received = append(received, capturedUpstreamReq{body: body, headers: r.Header.Clone()})
		callNum++
		cur := callNum
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch cur {
		case 1:
			// Upstream OpenAI returns non-Anthropic id "chatcmpl-123".
			_, _ = io.WriteString(w, `{"id":"chatcmpl-123","object":"chat.completion","created":1700000000,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"Your name is Zorblax."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		case 2:
			_, _ = io.WriteString(w, `{"id":"chatcmpl-456","object":"chat.completion","created":1700000001,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"Zorblax"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22}}`)
		}
	}))
	defer upstream.Close()

	outbound, err := openai.NewOutboundTransformer(upstream.URL+"/v1", "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeOpenai, upstream.URL+"/v1", false, outbound, nil)

	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	rec1 := postJSON(t, router, ctx, "/v1/messages", `{
		"model": "fixture-model",
		"max_tokens": 64,
		"system": "My name is Zorblax.",
		"messages": [{"role": "user", "content": "Remember my name."}],
		"thread": {"type": "create"}
	}`, headers)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	msg1ID := gjson.GetBytes(rec1.Body.Bytes(), "id").String()
	require.True(t, strings.HasPrefix(msg1ID, "msg_"), "expected normalized msg_ id, got %q", msg1ID)

	rec2 := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{
		"model": "fixture-model",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "What is my name?"}],
		"thread": {"type": "continue", "previous_message_id": %q}
	}`, msg1ID), headers)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Equal(t, "Zorblax", gjson.GetBytes(rec2.Body.Bytes(), "content.0.text").String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 2)
	// OpenAI upstream received full transformed history: system + user1 + assistant1 + user2.
	msgs := gjson.GetBytes(received[1].body, "messages").Array()
	require.Len(t, msgs, 4)
	require.Equal(t, "system", msgs[0].Get("role").String())
	require.Equal(t, "My name is Zorblax.", msgs[0].Get("content").String())
	require.Equal(t, "Remember my name.", msgs[1].Get("content").String())
	require.Equal(t, "Your name is Zorblax.", msgs[2].Get("content").String())
	require.Equal(t, "What is my name?", msgs[3].Get("content").String())
	require.Empty(t, received[1].headers.Get("Anthropic-Beta"))
}

func TestAnthropicMessageThreads_CreateOnlyFieldsProhibitedOnContinue(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"msg_ok_%d","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, calls))
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	rec1 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"system":"orig","tools":[{"name":"t","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Equal(t, 1, calls)

	for name, body := range map[string]string{
		"system string": `{"model":"fixture-model","max_tokens":64,"system":"new","messages":[{"role":"user","content":"next"}],"thread":{"type":"continue","previous_message_id":"msg_ok_1"}}`,
		"system null":   `{"model":"fixture-model","max_tokens":64,"system":null,"messages":[{"role":"user","content":"next"}],"thread":{"type":"continue","previous_message_id":"msg_ok_1"}}`,
		"tools array":   `{"model":"fixture-model","max_tokens":64,"tools":[],"messages":[{"role":"user","content":"next"}],"thread":{"type":"continue","previous_message_id":"msg_ok_1"}}`,
		"tools null":    `{"model":"fixture-model","max_tokens":64,"tools":null,"messages":[{"role":"user","content":"next"}],"thread":{"type":"continue","previous_message_id":"msg_ok_1"}}`,
		"bad type":      `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"next"}],"thread":{"type":"fork"}}`,
		"empty prev id": `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"next"}],"thread":{"type":"continue","previous_message_id":""}}`,
		"null thread":   `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"next"}],"thread":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postJSON(t, router, ctx, "/v1/messages", body, headers)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, "error", gjson.GetBytes(rec.Body.Bytes(), "type").String())
			require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
		})
	}
	require.Equal(t, 1, calls)

	// Valid continue still works after rejected attempts.
	rec2 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"next"}],"thread":{"type":"continue","previous_message_id":"msg_ok_1"}}`, headers)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Equal(t, 2, calls)
}

func TestAnthropicMessageThreads_ScopeIsolationAndExpiration(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store := newAnthropicThreadStoreWithConfig(
		10*time.Minute,
		time.Hour,
		defaultAnthropicThreadMaxBytes,
		defaultAnthropicThreadMaxScopeBytes,
		func() time.Time { return now },
	)

	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"msg_s_%d","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, calls))
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, projectID := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, store)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	// Unknown id -> 404 thread_not_found.
	recMissing := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thread":{"type":"continue","previous_message_id":"msg_missing"}}`, headers)
	require.Equal(t, http.StatusNotFound, recMissing.Code)
	require.Equal(t, "not_found_error", gjson.GetBytes(recMissing.Body.Bytes(), "error.type").String())
	require.Equal(t, "thread_not_found", gjson.GetBytes(recMissing.Body.Bytes(), "error.details.error_code").String())

	// Create under (projectID, key=101).
	rec1 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"secret"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Cross-API-key -> 404 thread_not_found.
	otherKeyCtx := contexts.WithAPIKey(ctx, &ent.APIKey{ID: 202})
	recOtherKey := postJSON(t, router, otherKeyCtx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"steal"}],"thread":{"type":"continue","previous_message_id":"msg_s_1"}}`, headers)
	require.Equal(t, http.StatusNotFound, recOtherKey.Code)
	require.Equal(t, "thread_not_found", gjson.GetBytes(recOtherKey.Body.Bytes(), "error.details.error_code").String())

	// Cross-project -> 404 thread_not_found.
	otherProjCtx := contexts.WithProjectID(ctx, projectID+999)
	recOtherProj := postJSON(t, router, otherProjCtx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"steal"}],"thread":{"type":"continue","previous_message_id":"msg_s_1"}}`, headers)
	require.Equal(t, http.StatusNotFound, recOtherProj.Code)
	require.Equal(t, "thread_not_found", gjson.GetBytes(recOtherProj.Body.Bytes(), "error.details.error_code").String())

	// Idle TTL expiry (> 10 minutes).
	now = now.Add(11 * time.Minute)
	recExpired := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"late"}],"thread":{"type":"continue","previous_message_id":"msg_s_1"}}`, headers)
	require.Equal(t, http.StatusNotFound, recExpired.Code)
	require.Equal(t, "thread_not_found", gjson.GetBytes(recExpired.Body.Bytes(), "error.details.error_code").String())

	// Max age expiry (> 1 hour despite active turns every 8 minutes).
	recFresh := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"start"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, recFresh.Code)
	lastID := gjson.GetBytes(recFresh.Body.Bytes(), "id").String()
	for range 7 {
		now = now.Add(8 * time.Minute)
		rec := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"step"}],"thread":{"type":"continue","previous_message_id":%q}}`, lastID), headers)
		require.Equal(t, http.StatusOK, rec.Code)
		lastID = gjson.GetBytes(rec.Body.Bytes(), "id").String()
	}
	// Total elapsed = 56m + 6m = 62m > 1h maxAge (even though idle is only 6m < 10m).
	now = now.Add(6 * time.Minute)
	recMaxAge := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"past max age"}],"thread":{"type":"continue","previous_message_id":%q}}`, lastID), headers)
	require.Equal(t, http.StatusNotFound, recMaxAge.Code)
	require.Equal(t, "thread_not_found", gjson.GetBytes(recMaxAge.Body.Bytes(), "error.details.error_code").String())
}

func TestAnthropicMessageThreads_InFlightAndAlreadyContinued(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	var mu sync.Mutex

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		cur := calls
		mu.Unlock()

		if cur == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_base","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"base"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
			return
		}
		close(started)
		<-release
		writeAnthropicSSE(w,
			threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_next","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
			threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`),
			threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
			threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`),
			threadSSEEvent("message_stop", `{"type":"message_stop"}`),
		)
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	rec1 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, rec1.Code)

	continueBody := `{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"follow up"}],"thread":{"type":"continue","previous_message_id":"msg_base"}}`
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- postJSON(t, router, ctx, "/v1/messages", continueBody, headers)
	}()

	<-started
	// Second concurrent continue while first is in flight -> 400 thread_continue_in_flight.
	recInFlight := postJSON(t, router, ctx, "/v1/messages", continueBody, headers)
	require.Equal(t, http.StatusBadRequest, recInFlight.Code)
	require.Equal(t, "thread_continue_in_flight", gjson.GetBytes(recInFlight.Body.Bytes(), "error.details.error_code").String())

	close(release)
	recFirst := <-firstDone
	require.Equal(t, http.StatusOK, recFirst.Code)

	// Replaying continue after first completed -> 400 thread_already_continued.
	recReplay := postJSON(t, router, ctx, "/v1/messages", continueBody, headers)
	require.Equal(t, http.StatusBadRequest, recReplay.Code)
	require.Equal(t, "thread_already_continued", gjson.GetBytes(recReplay.Body.Bytes(), "error.details.error_code").String())
}

func TestAnthropicMessageThreads_DroppedStreamReleasesInFlightForRetry(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_root","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"root"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
		case 2:
			// Stream terminates with an SSE error event before message_stop.
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_broken","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
				threadSSEEvent("error", `{"type":"error","error":{"type":"api_error","message":"stream dropped"}}`),
			)
		case 3:
			// Retry succeeds with message_stop.
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_recovered","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"recovered"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		}
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	rec1 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"start"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, rec1.Code)

	contBody := `{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"go"}],"thread":{"type":"continue","previous_message_id":"msg_root"}}`
	recDrop := postJSON(t, router, ctx, "/v1/messages", contBody, headers)
	require.Equal(t, http.StatusOK, recDrop.Code)

	// msg_broken must NOT have been stored.
	recBroken := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"x"}],"thread":{"type":"continue","previous_message_id":"msg_broken"}}`, headers)
	require.Equal(t, http.StatusNotFound, recBroken.Code)
	require.Equal(t, "thread_not_found", gjson.GetBytes(recBroken.Body.Bytes(), "error.details.error_code").String())

	// Retrying msg_root MUST succeed because in_flight was released!
	recRetry := postJSON(t, router, ctx, "/v1/messages", contBody, headers)
	require.Equal(t, http.StatusOK, recRetry.Code)
	require.Contains(t, recRetry.Body.String(), `"id":"msg_recovered"`)
}

func TestAnthropicMessageThreads_FingerprintMismatch(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"msg_fp_%d","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, calls))
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)

	baseHeaders := map[string]string{
		"Anthropic-Beta": "message-threads-2026-08-12, mcp-client-2025-11-20",
	}
	createBody := `{
		"model": "fixture-model",
		"max_tokens": 128,
		"tool_choice": {"type": "auto"},
		"thinking": {"type": "enabled", "budget_tokens": 2048},
		"mcp_servers": [{"type": "url", "url": "https://mcp.example.com", "name": "srv", "authorization_token": "tok_v1"}],
		"context_management": {"edits": [{"type": "clear_tool_uses_20250919"}]},
		"output_config": {"effort": "high"},
		"container": {"id": "cntr_1", "skills": [{"type": "custom", "skill_id": "sk_1"}]},
		"cache_control": {"type": "ephemeral"},
		"messages": [{"role": "user", "content": "hi"}],
		"thread": {"type": "create"}
	}`
	rec1 := postJSON(t, router, ctx, "/v1/messages", createBody, baseHeaders)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())

	buildContinue := func(overrides map[string]any) string {
		m := map[string]any{
			"model":              "fixture-model",
			"max_tokens":         256,
			"tool_choice":        map[string]any{"type": "auto"},
			"thinking":           map[string]any{"type": "enabled", "budget_tokens": 2048},
			"mcp_servers":        []any{map[string]any{"type": "url", "url": "https://mcp.example.com", "name": "srv", "authorization_token": "tok_v2_rotated"}},
			"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_tool_uses_20250919"}}},
			"output_config":      map[string]any{"effort": "high"},
			"container":          map[string]any{"id": "cntr_2_changed", "skills": []any{map[string]any{"type": "custom", "skill_id": "sk_1"}}},
			"cache_control":      map[string]any{"type": "ephemeral"},
			"messages":           []any{map[string]any{"role": "user", "content": "next"}},
			"thread":             map[string]any{"type": "continue", "previous_message_id": "msg_fp_1"},
		}
		for k, v := range overrides {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		b, err := json.Marshal(m)
		require.NoError(t, err)
		return string(b)
	}

	mismatchCases := []struct {
		name      string
		overrides map[string]any
		headers   map[string]string
		wantField string
	}{
		{"model", map[string]any{"model": "other-model"}, baseHeaders, "model"},
		{"tool_choice", map[string]any{"tool_choice": map[string]any{"type": "any"}}, baseHeaders, "tool_choice"},
		{"thinking", map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 4096}}, baseHeaders, "thinking"},
		{"mcp_servers url", map[string]any{"mcp_servers": []any{map[string]any{"type": "url", "url": "https://other.example.com", "name": "srv"}}}, baseHeaders, "mcp_servers"},
		{"context_management", map[string]any{"context_management": nil}, baseHeaders, "context_management"},
		{"output_config", map[string]any{"output_config": map[string]any{"effort": "low"}}, baseHeaders, "output_config"},
		{"container.skills", map[string]any{"container": map[string]any{"id": "cntr_1", "skills": []any{map[string]any{"type": "custom", "skill_id": "sk_2"}}}}, baseHeaders, "container.skills"},
		{"cache_control", map[string]any{"cache_control": nil}, baseHeaders, "cache_control"},
		{"anthropic-beta", nil, map[string]string{"Anthropic-Beta": "message-threads-2026-08-12"}, "anthropic-beta"},
	}
	for _, tc := range mismatchCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, router, ctx, "/v1/messages", buildContinue(tc.overrides), tc.headers)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, "thread_fingerprint_mismatch", gjson.GetBytes(rec.Body.Bytes(), "error.details.error_code").String())
			require.Contains(t, gjson.GetBytes(rec.Body.Bytes(), "error.message").String(), tc.wantField)
		})
	}
	require.Equal(t, 1, calls)

	// Matching fingerprint with rotated mcp authorization_token, changed container.id,
	// and reordered anthropic-beta tokens succeeds!
	recOK := postJSON(t, router, ctx, "/v1/messages", buildContinue(nil), map[string]string{
		"Anthropic-Beta": "mcp-client-2025-11-20,message-threads-2026-08-12",
	})
	require.Equal(t, http.StatusOK, recOK.Code, recOK.Body.String())
	require.Equal(t, 2, calls)
}

func TestAnthropicMessageThreads_UnsupportedFeaturesAndCountTokens(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_u_1","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)

	// Compaction param rejected.
	recCompaction := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"compaction":{"trigger":10000},"messages":[{"role":"user","content":"hi"}],"thread":{"type":"create"}}`, map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta})
	require.Equal(t, http.StatusBadRequest, recCompaction.Code)
	require.Equal(t, "thread_unsupported_request", gjson.GetBytes(recCompaction.Body.Bytes(), "error.details.error_code").String())
	require.Equal(t, "context_management", gjson.GetBytes(recCompaction.Body.Bytes(), "error.details.unsupported.0").String())

	// compact-2026-09-04 beta header rejected.
	recCompactBeta := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thread":{"type":"create"}}`, map[string]string{"Anthropic-Beta": "message-threads-2026-08-12, compact-2026-09-04"})
	require.Equal(t, http.StatusBadRequest, recCompactBeta.Code)
	require.Equal(t, "thread_unsupported_request", gjson.GetBytes(recCompactBeta.Body.Bytes(), "error.details.error_code").String())
	require.Equal(t, "context_management", gjson.GetBytes(recCompactBeta.Body.Bytes(), "error.details.unsupported.0").String())

	// >4 cache_control breakpoints rejected.
	fiveBreakpoints := `{
		"model":"fixture-model","max_tokens":64,
		"cache_control":{"type":"ephemeral"},
		"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"t1","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}
		]}],
		"thread":{"type":"create"}
	}`
	recTooManyCC := postJSON(t, router, ctx, "/v1/messages", fiveBreakpoints, map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta})
	require.Equal(t, http.StatusBadRequest, recTooManyCC.Code)
	require.Equal(t, "thread_unsupported_request", gjson.GetBytes(recTooManyCC.Body.Bytes(), "error.details.error_code").String())
	require.Equal(t, "cache_control", gjson.GetBytes(recTooManyCC.Body.Bytes(), "error.details.unsupported.0").String())

	// /v1/messages/count_tokens and /anthropic/v1/messages/count_tokens reject "thread".
	for _, path := range []string{"/v1/messages/count_tokens", "/anthropic/v1/messages/count_tokens"} {
		rec := postJSON(t, router, ctx, path, `{"model":"fixture-model","messages":[{"role":"user","content":"hi"}],"thread":{"type":"create"}}`, map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	}
	require.Equal(t, 0, calls)
}

func TestAnthropicMessageThreads_StorageExclusionsAndPauseTurn(t *testing.T) {
	var (
		mu       sync.Mutex
		received []capturedUpstreamReq
		calls    int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, capturedUpstreamReq{body: body, headers: r.Header.Clone()})
		calls++
		cur := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch cur {
		case 1:
			// Refusal -> must NOT be stored.
			_, _ = io.WriteString(w, `{"id":"msg_refused","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"I cannot."}],"stop_reason":"refusal","usage":{"input_tokens":5,"output_tokens":3}}`)
		case 2:
			// max_tokens ending in cut-off tool_use -> must NOT be stored.
			_, _ = io.WriteString(w, `{"id":"msg_cutoff_tool","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"tool_use","id":"toolu_1","name":"fn","input":{}}],"stop_reason":"max_tokens","usage":{"input_tokens":5,"output_tokens":64}}`)
		case 3:
			// fallback_message iteration -> must NOT be stored.
			_, _ = io.WriteString(w, `{"id":"msg_fallback","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"fallback"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3,"iterations":[{"type":"fallback_message"}]}}`)
		case 4:
			// pause_turn -> MUST be stored and allow empty messages: [] on continue.
			_, _ = io.WriteString(w, `{"id":"msg_paused","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"working..."}],"stop_reason":"pause_turn","usage":{"input_tokens":5,"output_tokens":3}}`)
		case 5:
			_, _ = io.WriteString(w, `{"id":"msg_resumed","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"finished"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)
		}
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	for _, expectedID := range []string{"msg_refused", "msg_cutoff_tool", "msg_fallback"} {
		rec := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"go"}],"thread":{"type":"create"}}`, headers)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, expectedID, gjson.GetBytes(rec.Body.Bytes(), "id").String())

		recCont := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"again"}],"thread":{"type":"continue","previous_message_id":%q}}`, expectedID), headers)
		require.Equal(t, http.StatusNotFound, recCont.Code)
		require.Equal(t, "thread_not_found", gjson.GetBytes(recCont.Body.Bytes(), "error.details.error_code").String())
	}

	// pause_turn create -> continue with empty messages: [].
	recPaused := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"run long task"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, recPaused.Code)
	recResumed := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[],"thread":{"type":"continue","previous_message_id":"msg_paused"}}`, headers)
	require.Equal(t, http.StatusOK, recResumed.Code, recResumed.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 5)
	msgs := gjson.GetBytes(received[4].body, "messages").Array()
	require.Len(t, msgs, 2)
	require.Equal(t, "run long task", msgs[0].Get("content").String())
	require.Equal(t, "working...", msgs[1].Get("content.0.text").String())
}

func TestAnthropicMessageThreads_CacheControlPruningAcross6TurnsAndNestedFields(t *testing.T) {
	var (
		mu       sync.Mutex
		received []capturedUpstreamReq
		turn     int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, capturedUpstreamReq{body: body, headers: r.Header.Clone()})
		turn++
		cur := turn
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if cur == 1 {
			// Include a tool_use whose free-form input has a "cache_control" key!
			// Adapted from anthropic-sdk-go TestBetaMessageThread_Send_AReplayDropsTheCacheControlOfNestedBlocks.
			_, _ = io.WriteString(w, `{"id":"msg_cc_1","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"tool_use","id":"toolu_cc","name":"lookup","input":{"cache_control":"no-store"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"msg_cc_%d","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"reply %d"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, cur, cur))
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	// Turn 1: tools(1) + system(1) + user1(1) = 3 breakpoints.
	createBody := `{
		"model":"fixture-model","max_tokens":64,
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}},"cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"turn 1","cache_control":{"type":"ephemeral"}}]}],
		"thread":{"type":"create"}
	}`
	rec := postJSON(t, router, ctx, "/v1/messages", createBody, headers)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	prevID := gjson.GetBytes(rec.Body.Bytes(), "id").String()

	// Turns 2..6: every turn adds a cache_control breakpoint on its new user message.
	for i := 2; i <= 6; i++ {
		var msgJSON string
		if i == 2 {
			msgJSON = `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_cc","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]}]}]`
		} else {
			msgJSON = fmt.Sprintf(`[{"role":"user","content":[{"type":"text","text":"turn %d","cache_control":{"type":"ephemeral"}}]}]`, i)
		}
		body := fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"messages":%s,"thread":{"type":"continue","previous_message_id":%q}}`, msgJSON, prevID)
		rec = postJSON(t, router, ctx, "/v1/messages", body, headers)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		prevID = gjson.GetBytes(rec.Body.Bytes(), "id").String()
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 6)

	for i, req := range received {
		sysBP := countRawCacheControl(json.RawMessage(gjson.GetBytes(req.body, "system").Raw))
		toolsBP := countRawCacheControl(json.RawMessage(gjson.GetBytes(req.body, "tools").Raw))
		msgsBP := countRawCacheControl(json.RawMessage(gjson.GetBytes(req.body, "messages").Raw))
		totalBP := sysBP + toolsBP + msgsBP
		require.Equal(t, 1, sysBP, "turn %d must preserve system cache_control", i+1)
		require.Equal(t, 1, toolsBP, "turn %d must preserve tools cache_control", i+1)
		require.LessOrEqual(t, totalBP, 4, "turn %d exceeded 4 cache_control breakpoints: %s", i+1, string(req.body))
	}

	// On turn 6: newest user message keeps cache_control, turn 5 user keeps cache_control (total = 4),
	// turns 1..4 user messages have cache_control stripped, and tool_use.input.cache_control is preserved!
	turn6 := received[5].body
	msgs6 := gjson.GetBytes(turn6, "messages").Array()
	require.Len(t, msgs6, 11)
	require.False(t, msgs6[0].Get("content.0.cache_control").Exists(), "turn 1 cache_control should be stripped")
	require.Equal(t, "no-store", msgs6[1].Get("content.0.input.cache_control").String(), "free-form tool_use.input.cache_control must survive")
	require.False(t, msgs6[2].Get("content.0.content.0.cache_control").Exists(), "turn 2 nested tool_result cache_control should be stripped")
	require.True(t, msgs6[8].Get("content.0.cache_control").Exists(), "most recent prior user turn keeps breakpoint within budget")
	require.True(t, msgs6[10].Get("content.0.cache_control").Exists(), "newest user turn keeps breakpoint")
}

func TestAnthropicMessageThreads_WithDecodedRequestBodyZstd(t *testing.T) {
	var (
		mu       sync.Mutex
		received []capturedUpstreamReq
		calls    int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, capturedUpstreamReq{body: body, headers: r.Header.Clone()})
		calls++
		cur := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"msg_zstd_%d","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"ok %d"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, cur, cur))
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)

	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	defer func() { _ = encoder.Close() }()

	postZstd := func(payload string) *httptest.ResponseRecorder {
		compressed := encoder.EncodeAll([]byte(payload), nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(compressed)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "zstd")
		req.Header.Set("Anthropic-Beta", anthropicMessageThreadsBeta)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec1 := postZstd(`{"model":"fixture-model","max_tokens":64,"system":"zstd sys","messages":[{"role":"user","content":"compressed create"}],"thread":{"type":"create"}}`)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())

	rec2 := postZstd(`{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"compressed continue"}],"thread":{"type":"continue","previous_message_id":"msg_zstd_1"}}`)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 2)
	require.Equal(t, "zstd sys", gjson.GetBytes(received[1].body, "system").String())
	msgs := gjson.GetBytes(received[1].body, "messages").Array()
	require.Len(t, msgs, 3)
	require.Equal(t, "compressed create", msgs[0].Get("content").String())
	require.Equal(t, "ok 1", msgs[1].Get("content.0.text").String())
	require.Equal(t, "compressed continue", msgs[2].Get("content").String())
}

func TestAnthropicThreadStore_FingerprintCountsTowardScopeLimit(t *testing.T) {
	store := newAnthropicThreadStoreWithConfig(time.Minute, time.Hour, 4096, 1024, time.Now)
	root := &anthropicThreadRoot{
		scope:       anthropicThreadScope{projectID: 1, apiKeyID: 1},
		fingerprint: anthropicThreadFingerprint{outputConfig: strings.Repeat("x", 1024)},
	}
	require.False(t, store.commitCreate(root, "msg_oversized_fingerprint", nil, "end_turn"))
	require.Zero(t, store.totalBytes)
}

func TestAnthropicThreadStore_BoundedMemoryLRUEvictionAndSharedPrefix(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	store := newAnthropicThreadStoreWithConfig(10*time.Minute, time.Hour, 4096, 2048, func() time.Time { return now })

	scope := anthropicThreadScope{projectID: 1, apiKeyID: 1}
	bigMsg := anthropicStoredMessage{raw: json.RawMessage(`{"role":"user","content":"` + strings.Repeat("x", 900) + `"}`)}
	root1 := &anthropicThreadRoot{scope: scope}
	require.True(t, store.commitCreate(root1, "msg_1", []anthropicStoredMessage{bigMsg}, "end_turn"))

	initialBytes := store.totalBytes
	// Continuing 5 turns with tiny deltas shares root1 + bigMsg without O(N^2) byte growth.
	parentID := "msg_1"
	for i := 2; i <= 6; i++ {
		res, err := store.reserveContinue(scope, parentID, anthropicThreadFingerprint{}, nil)
		require.Nil(t, err)
		nextID := fmt.Sprintf("msg_%d", i)
		tiny := anthropicStoredMessage{raw: json.RawMessage(`{"role":"user","content":"a"}`)}
		require.True(t, store.commitContinue(res.parent, nextID, []anthropicStoredMessage{tiny}, "end_turn"))
		parentID = nextID
	}
	// Total bytes grew linearly by only 5 tiny deltas, NOT 5x bigMsg!
	require.Less(t, store.totalBytes, initialBytes+500)

	// Adding a second large thread in the same scope exceeds maxScopeBytes (2048) and LRU-evicts the first thread.
	root2 := &anthropicThreadRoot{scope: scope}
	require.True(t, store.commitCreate(root2, "msg_other", []anthropicStoredMessage{bigMsg}, "end_turn"))
	require.LessOrEqual(t, store.scopeBytes[scope], 2048)
	_, errEvicted := store.reserveContinue(scope, parentID, anthropicThreadFingerprint{}, nil)
	require.NotNil(t, errEvicted)
	require.Equal(t, "thread_not_found", errEvicted.errorCode)

	// Confirm the newly added thread is present.
	resOther, errOther := store.reserveContinue(scope, "msg_other", anthropicThreadFingerprint{}, nil)
	require.Nil(t, errOther)
	store.releaseContinue(resOther.parent)
}

func TestAnthropicMessageThreads_StreamingIDNormalizationAndExclusions(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			// Non-Anthropic stream ID "chatcmpl-stream-1" must be normalized to "msg_..." in both SSE and store.
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"chatcmpl-stream-1","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		case 2:
			// Streaming refusal on continue: NOT stored, and releases parent's in_flight so parent can still be continued.
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_stream_refusal","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"nope"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null},"usage":{"output_tokens":2}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		case 3:
			// Streaming max_tokens cutting off a tool_use block: NOT stored, parent remains continuable.
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_stream_cutoff_tool","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cut","name":"bash","input":{}}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"echo"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		case 4:
			// Streaming max_tokens on plain text: IS stored and continuable!
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_stream_max_text","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"truncated text"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		case 5:
			// Streaming fallback_message iteration in message_delta: NOT stored.
			writeAnthropicSSE(w,
				threadSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_stream_fallback","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`),
				threadSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				threadSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fallback"}}`),
				threadSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
				threadSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2,"iterations":[{"type":"fallback_message"}]}}`),
				threadSSEEvent("message_stop", `{"type":"message_stop"}`),
			)
		}
	}))
	defer upstream.Close()

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	router, ctx, _ := setupAnthropicThreadTestRouter(t, channel.TypeAnthropic, upstream.URL, true, outbound, nil)
	headers := map[string]string{"Anthropic-Beta": anthropicMessageThreadsBeta}

	// Call 1: extract normalized msg_... ID from SSE message_start.
	rec1 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}],"thread":{"type":"create"}}`, headers)
	require.Equal(t, http.StatusOK, rec1.Code)
	var normalizedID string
	for line := range strings.SplitSeq(rec1.Body.String(), "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			payload := strings.TrimSpace(data)
			if gjson.Get(payload, "type").String() == "message_start" {
				normalizedID = gjson.Get(payload, "message.id").String()
				break
			}
		}
	}
	require.True(t, strings.HasPrefix(normalizedID, "msg_"), "expected patched msg_ ID in SSE, got %q", normalizedID)

	// Call 2: refusal on continue -> not stored, parent stays continuable.
	rec2 := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"bad"}],"thread":{"type":"continue","previous_message_id":%q}}`, normalizedID), headers)
	require.Equal(t, http.StatusOK, rec2.Code)
	recRefusedCheck := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"x"}],"thread":{"type":"continue","previous_message_id":"msg_stream_refusal"}}`, headers)
	require.Equal(t, http.StatusNotFound, recRefusedCheck.Code)

	// Call 3: max_tokens cut-off tool_use -> not stored, parent STILL continuable.
	rec3 := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"tool"}],"thread":{"type":"continue","previous_message_id":%q}}`, normalizedID), headers)
	require.Equal(t, http.StatusOK, rec3.Code)
	recCutoffCheck := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"x"}],"thread":{"type":"continue","previous_message_id":"msg_stream_cutoff_tool"}}`, headers)
	require.Equal(t, http.StatusNotFound, recCutoffCheck.Code)

	// Call 4: max_tokens on text -> IS stored!
	rec4 := postJSON(t, router, ctx, "/v1/messages", fmt.Sprintf(`{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"long"}],"thread":{"type":"continue","previous_message_id":%q}}`, normalizedID), headers)
	require.Equal(t, http.StatusOK, rec4.Code)

	// Call 5: continue from msg_stream_max_text -> returns fallback_message iteration -> NOT stored.
	rec5 := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"continue text"}],"thread":{"type":"continue","previous_message_id":"msg_stream_max_text"}}`, headers)
	require.Equal(t, http.StatusOK, rec5.Code)
	recFallbackCheck := postJSON(t, router, ctx, "/v1/messages", `{"model":"fixture-model","max_tokens":64,"messages":[{"role":"user","content":"x"}],"thread":{"type":"continue","previous_message_id":"msg_stream_fallback"}}`, headers)
	require.Equal(t, http.StatusNotFound, recFallbackCheck.Code)
}
