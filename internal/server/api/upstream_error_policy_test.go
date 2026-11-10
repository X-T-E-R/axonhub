package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type slowRetryPolicyRedis struct {
	gets atomic.Int32
}

func (h *slowRetryPolicyRedis) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *slowRetryPolicyRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *slowRetryPolicyRedis) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && cmd.Args()[1] == "axonhub:cache:system:value:system:retry_policy" {
			h.gets.Add(1)
			time.Sleep(10 * time.Millisecond)
		}
		return next(ctx, cmd)
	}
}

type retryPolicyFlushRecorder struct {
	gin.ResponseWriter
	getsAtFlush []int32
	redis       *slowRetryPolicyRedis
}

func (w *retryPolicyFlushRecorder) Flush() {
	w.getsAtFlush = append(w.getsAtFlush, w.redis.gets.Load())
	w.ResponseWriter.Flush()
}

func TestResponsesSSEDoesNotReadRetryPolicyPerDelta(t *testing.T) {
	const deltaCount = 80 // More than the raw pass-through channel can buffer.
	const completed = `{"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","model":"fixture-model","created_at":1,"error":null,"output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"fixture"}]}],"usage":{"input_tokens":3,"output_tokens":80,"total_tokens":83}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range deltaCount {
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_fixture\",\"output_index\":0,\"content_index\":0,\"sequence_number\":%d,\"delta\":\"x\"}\n\n", i)
		}
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", completed)
	}))
	defer upstream.Close()

	client := enttest.NewEntClient(t, "sqlite3", "file:responses-sse-retry-policy?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("p").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)
	outbound, err := responses.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	policy := &biz.RetryPolicy{UpstreamErrorPolicy: biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModeCustom, CustomMessage: "safe message"}}
	handler := setupErrorPassthroughHandler(t, ctx, client, channel.TypeOpenai, upstream.URL, true, responses.NewInboundTransformer(), outbound, policy)
	handler.sseHeartbeatFormat = sseHeartbeatOpenAI

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer redisClient.Close()
	system := handler.ChatCompletionOrchestrator.SystemService
	system.Cache = xcache.NewRedisWithDomain[ent.System](redisClient, xcache.DefaultNamespace, "system")
	_, err = system.RetryPolicy(ctx) // Populate the same Redis cache used by the live reader.
	require.NoError(t, err)
	slow := &slowRetryPolicyRedis{}
	redisClient.AddHook(slow)

	var flushes *retryPolicyFlushRecorder
	router := gin.New()
	router.Use(func(c *gin.Context) {
		flushes = &retryPolicyFlushRecorder{ResponseWriter: c.Writer, redis: slow}
		c.Writer = flushes
		c.Next()
	})
	router.POST("/v1/responses", handler.ChatCompletion)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"fixture-model","stream":true,"input":"hi"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, deltaCount, strings.Count(rec.Body.String(), `"type":"response.output_text.delta"`))
	require.Contains(t, rec.Body.String(), completed)
	require.Len(t, flushes.getsAtFlush, deltaCount+2) // Headers, deltas, terminal.
	require.Positive(t, flushes.getsAtFlush[0], "fixture must exercise retry-policy Redis during admission")
	require.Equal(t, int32(0), flushes.getsAtFlush[deltaCount+1]-flushes.getsAtFlush[0], "ordinary streaming frames must not wait for Redis retry policy")
	// HTTP pass-through persists its transformed drain independently of delivery.
	// Let it finish before closing this fixture's Redis connection and database.
	require.Eventually(t, func() bool {
		parent, parentErr := client.Request.Query().Only(ctx)
		execution, executionErr := client.RequestExecution.Query().Only(ctx)
		count, usageErr := client.UsageLog.Query().Count(ctx)
		return parentErr == nil && executionErr == nil && usageErr == nil &&
			parent.Status == request.StatusCompleted && execution.Status == requestexecution.StatusCompleted && count == 1
	}, time.Second, 5*time.Millisecond)
	usage := client.UsageLog.Query().OnlyX(ctx)
	require.Equal(t, int64(3), usage.PromptTokens)
	require.Equal(t, int64(deltaCount), usage.CompletionTokens)
}
