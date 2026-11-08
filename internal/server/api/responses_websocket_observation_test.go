package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type wsObservationFixture struct {
	client   *ent.Client
	ctx      context.Context
	writer   *biz.ForwardingObservationWriter
	server   *httptest.Server
	handler  *ResponsesWebSocketHandler
	keys     *biz.APIKeyService
	key      *ent.APIKey
	calls    atomic.Int32
	received chan []byte
	started  chan struct{}
	canceled chan struct{}
	selector *wsObservationSelector
	routes   []*orchestrator.ChannelModelsCandidate
}

type wsObservationSelector struct {
	selected atomic.Pointer[orchestrator.ChannelModelsCandidate]
}

func (s *wsObservationSelector) Select(context.Context, *llm.Request) ([]*orchestrator.ChannelModelsCandidate, error) {
	return []*orchestrator.ChannelModelsCandidate{s.selected.Load()}, nil
}

// Unlike the original pipeline fixture, this includes the production route's
// thread/trace middleware and asynchronous observation writer. The only network
// peer is a loopback synthetic Responses provider.
func newWSObservationFixture(t *testing.T) *wsObservationFixture {
	t.Helper()
	return newWSObservationFixtureWithLimit(t, 0)
}

func newWSObservationFixtureWithLimit(t *testing.T, maxItems int) *wsObservationFixture {
	t.Helper()
	f := &wsObservationFixture{received: make(chan []byte, 32), started: make(chan struct{}, 4), canceled: make(chan struct{}, 4)}
	f.client = enttest.NewEntClient(t, "sqlite3", "file:ws-observation-"+uuid.NewString()+"?mode=memory&_fk=1")
	t.Cleanup(func() { require.NoError(t, f.client.Close()) })
	f.ctx = ent.NewContext(authz.WithTestBypass(t.Context()), f.client)
	proj := f.client.Project.Create().SetName("observation-project").SetStatus(project.StatusActive).SaveX(f.ctx)
	owner := f.client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("test").SetStatus(user.StatusActivated).SaveX(f.ctx)
	f.key = f.client.APIKey.Create().SetName("ws-observation").SetKey("ah-" + uuid.NewString()).SetType(apikey.TypeUser).
		SetStatus(apikey.StatusEnabled).SetProjectID(proj.ID).SetUserID(owner.ID).SaveX(f.ctx)
	f.client.DataStorage.Create().SetName("primary").SetDescription("synthetic test storage").SetType(datastorage.TypeDatabase).SetPrimary(true).
		SetStatus(datastorage.StatusActive).SetSettings(&objects.DataStorageSettings{}).SaveX(f.ctx)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: f.client})
	require.NoError(t, system.SetStoragePolicy(f.ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true}))
	projects := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cache, Ent: f.client})
	f.keys = biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cache, Ent: f.client, ProjectService: projects, KeyPrefix: "ah"})
	t.Cleanup(f.keys.Stop)
	auth := &biz.AuthService{APIKeyService: f.keys}
	channels, legacy, _, usage := setupSpeechAPITestServices(t, f.client)
	f.writer = biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{MaxItems: maxItems, AttemptTimeout: time.Second})
	requests := biz.NewRequestServiceWithObservationWriter(f.client, system, usage, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, f.writer)
	require.NoError(t, f.writer.Start(f.ctx))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, f.writer.Stop(ctx))
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		f.received <- body
		var payload struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))
		account := "synthetic-0"
		if payload.Model == "other-upstream-m" {
			account = "synthetic-1"
		}
		require.Equal(t, "Bearer "+account, r.Header.Get("Authorization"))
		if strings.Contains(string(body), "force_failure") {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"server_error","message":"synthetic failure"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(body), "wait_for_cancel") {
			fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_%d\",\"model\":\"upstream-m\",\"status\":\"in_progress\",\"output\":[]}}\n\n", n)
			w.(http.Flusher).Flush()
			f.started <- struct{}{}
			<-r.Context().Done()
			f.canceled <- struct{}{}
			return
		}
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"object\":\"response\",\"model\":%q,\"status\":\"completed\",\"created_at\":1,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_%d\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}],\"usage\":{\"input_tokens\":103,\"input_tokens_details\":{\"cached_tokens\":64},\"output_tokens\":2,\"total_tokens\":105}}}\n\n", n, payload.Model, n)
	}))
	t.Cleanup(upstream.Close)
	f.selector = &wsObservationSelector{}
	for i, actualModel := range []string{"upstream-m", "other-upstream-m"} {
		account := fmt.Sprintf("synthetic-%d", i)
		row := f.client.Channel.Create().SetType(channel.TypeOpenai).SetName(account).SetBaseURL(upstream.URL).
			SetCredentials(objects.ChannelCredentials{APIKey: account}).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SaveX(f.ctx)
		provider, err := responses.NewOutboundTransformer(upstream.URL, account)
		require.NoError(t, err)
		f.routes = append(f.routes, &orchestrator.ChannelModelsCandidate{
			Channel: &biz.Channel{Channel: row, Outbound: provider}, Models: []biz.ChannelModelEntry{{RequestModel: "m", ActualModel: actualModel, Source: "direct"}},
		})
	}
	f.selector.selected.Store(f.routes[0])
	processor := orchestrator.NewChatCompletionOrchestrator(channels, nil, requests, httpclient.NewHttpClient(), responses.NewInboundTransformer(), system, usage, nil, nil, nil,
		legacy.LiveStreamRegistry, orchestrator.NewChannelLimiterManager(), nil).WithChannelSelector(f.selector)
	processor.PromptProtecter = nil
	processor.PromptProvider = observationNoPrompts{}
	f.handler = newResponsesWebSocketHandler(NewChatCompletionHandlers(processor), auth)
	traces := biz.NewTraceService(biz.TraceServiceParams{Ent: f.client, RequestService: requests})
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Request = c.Request.WithContext(ent.NewContext(c.Request.Context(), f.client)) })
	router.Use(middleware.WithAPIKeyConfig(auth, nil), middleware.WithSource(request.SourceAPI),
		middleware.WithThread(tracing.Config{}, biz.NewThreadService(f.client, traces)), middleware.WithTrace(tracing.Config{}, traces))
	router.GET("/v1/responses", f.handler.Serve)
	f.server = httptest.NewServer(router)
	t.Cleanup(f.server.Close)
	return f
}

func (f *wsObservationFixture) dial(t *testing.T, key *ent.APIKey) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/responses", http.Header{
		"Authorization": {"Bearer " + key.Key}, "Ah-Thread-Id": {fmt.Sprintf("thread-%d", key.ID)},
	})
	if err != nil && resp != nil {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("handshake %d: %s: %v", resp.StatusCode, body, err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	return conn
}

func wsObservationBarrier(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel"}`)))
	require.Equal(t, "unsupported_event", wsErrorCode(wsRead(t, conn)))
}

func (f *wsObservationFixture) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, f.writer.Wait(ctx))
}

func (f *wsObservationFixture) assertCompleted(t *testing.T, key *ent.APIKey, count int) {
	t.Helper()
	parents := f.client.Request.Query().Where(request.APIKeyIDEQ(key.ID), request.StatusEQ(request.StatusCompleted)).AllX(f.ctx)
	require.Len(t, parents, count, "each generation must have its own persisted Request")
	var traceID int
	for _, parent := range parents {
		require.Equal(t, key.ProjectID, parent.ProjectID)
		require.Positive(t, parent.TraceID, "the handshake's asynchronous trace reference must resolve in each generation")
		if traceID == 0 {
			traceID = parent.TraceID
		}
		require.Equal(t, traceID, parent.TraceID)
		execution := f.client.RequestExecution.Query().Where(requestexecution.RequestIDEQ(parent.ID)).OnlyX(f.ctx)
		require.Equal(t, key.ProjectID, execution.ProjectID)
		require.Equal(t, requestexecution.StatusCompleted, execution.Status)
		billing := parent.QueryUsageLogs().OnlyX(f.ctx)
		require.Equal(t, key.ID, billing.APIKeyID)
		require.Equal(t, key.ProjectID, billing.ProjectID)
		require.Equal(t, execution.ChannelID, parent.ChannelID)
		require.Equal(t, execution.ChannelID, billing.ChannelID)
		require.Equal(t, execution.ModelID, billing.ModelID, "usage model_id is the execution's ActualModelID")
		require.Equal(t, int64(103), billing.PromptTokens)
		require.Equal(t, int64(64), billing.PromptCachedTokens)
		require.Equal(t, int64(2), billing.CompletionTokens)
		require.Equal(t, int64(105), billing.TotalTokens)
	}
	trace := f.client.Trace.GetX(f.ctx, traceID)
	require.Equal(t, key.ProjectID, trace.ProjectID)
	require.Positive(t, trace.ThreadID)
	require.Equal(t, fmt.Sprintf("thread-%d", key.ID), f.client.Thread.GetX(f.ctx, trace.ThreadID).ThreadID)
}

func TestResponsesWebSocketObservationEachGeneration(t *testing.T) {
	f := newWSObservationFixture(t)
	conn := f.dial(t, f.key)
	for _, frame := range []string{
		`{"type":"response.create","stream_id":"a","model":"m","input":[{"role":"user","content":"first full"}]}`,
		`{"type":"response.create","stream_id":"a","model":"m","previous_response_id":"resp_1","input":[{"role":"user","content":"suffix"}]}`,
		`{"type":"response.create","stream_id":"b","model":"m","input":[{"role":"user","content":"new full"}]}`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
		require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	}
	wsObservationBarrier(t, conn)
	f.drain(t)
	f.assertCompleted(t, f.key, 3)
	<-f.received
	var delta, full struct {
		Input []json.RawMessage `json:"input"`
	}
	require.NoError(t, json.Unmarshal(<-f.received, &delta))
	require.Len(t, delta.Input, 3)
	require.Contains(t, string(delta.Input[1]), "answer")
	require.Contains(t, string(delta.Input[2]), "suffix")
	require.NoError(t, json.Unmarshal(<-f.received, &full))
	require.Len(t, full.Input, 1)
	require.Contains(t, string(full.Input[0]), "new full")
	// Both lanes may be queued concurrently, but the socket executes FIFO. Each
	// generation has separate persistence even while their histories differ.
	for _, frame := range []string{
		`{"type":"response.create","stream_id":"a","model":"m","previous_response_id":"resp_2","input":"lane-a"}`,
		`{"type":"response.create","stream_id":"b","model":"m","previous_response_id":"resp_3","input":"lane-b"}`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
	}
	for _, lane := range []string{"a", "b"} {
		event := wsRead(t, conn)
		require.Equal(t, "response.completed", wsEventString(event, "type"))
		require.Equal(t, lane, wsEventString(event, "stream_id"))
	}
	wsObservationBarrier(t, conn)
	f.drain(t)
	f.assertCompleted(t, f.key, 5)
	require.Equal(t, 5, f.client.Request.Query().CountX(f.ctx))
	require.Equal(t, 5, f.client.RequestExecution.Query().CountX(f.ctx))
	require.Equal(t, 5, f.client.UsageLog.Query().CountX(f.ctx))
}

func TestResponsesWebSocketObservationFailureAndTimeoutRecovery(t *testing.T) {
	f := newWSObservationFixture(t)
	f.handler.turnTimeout = 200 * time.Millisecond
	conn := f.dial(t, f.key)
	for _, input := range []string{"force_failure", "wait_for_cancel"} {
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": input}))
		for {
			event := wsRead(t, conn)
			if wsEventString(event, "type") == "error" {
				require.NotContains(t, event, "execution", "an attempted generation remains ambiguous")
				break
			}
		}
		wsObservationBarrier(t, conn)
		f.drain(t)
	}
	select {
	case <-f.canceled:
	case <-time.After(time.Second):
		t.Fatal("generation deadline did not cancel the upstream request")
	}
	require.Equal(t, 2, f.client.Request.Query().Where(request.StatusEQ(request.StatusFailed)).CountX(f.ctx))
	require.Equal(t, 2, f.client.RequestExecution.Query().Where(requestexecution.StatusEQ(requestexecution.StatusFailed)).CountX(f.ctx))
	// A failed generation never leaves its ended scope or failed baseline on
	// the socket. Recovery is a new full request with its own billing lifecycle.
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "recovered full"}))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	wsObservationBarrier(t, conn)
	f.drain(t)
	f.assertCompleted(t, f.key, 1)
	require.Equal(t, 3, f.client.Request.Query().CountX(f.ctx))
	require.Equal(t, 3, f.client.RequestExecution.Query().CountX(f.ctx))
	require.Equal(t, 1, f.client.UsageLog.Query().CountX(f.ctx), "failures without provider usage must not manufacture billing")
}

func TestResponsesWebSocketObservationDisconnectAndIdentityIsolation(t *testing.T) {
	f := newWSObservationFixture(t)
	first := f.dial(t, f.key)
	require.NoError(t, first.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "wait_for_cancel"}))
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("first connection did not enter the provider")
	}
	// Another key/project completes while the first connection is in flight.
	otherProject := f.client.Project.Create().SetName("other-project").SetStatus(project.StatusActive).SaveX(f.ctx)
	// Create a real key through the same store and invalidate the key cache via
	// its service, just as a managed key becomes visible to authentication.
	otherKey := f.client.APIKey.Create().SetName("other").SetKey("ah-" + uuid.NewString()).SetType(apikey.TypeUser).
		SetStatus(apikey.StatusEnabled).SetProjectID(otherProject.ID).SetUserID(f.key.UserID).SaveX(f.ctx)
	f.keys.InvalidateAPIKeyCaches(f.ctx, otherKey.Key)
	second := f.dial(t, otherKey)
	for range 3 {
		require.NoError(t, second.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "other full"}))
		require.Equal(t, "response.completed", wsEventString(wsRead(t, second), "type"))
	}
	wsObservationBarrier(t, second)
	require.NoError(t, first.Close())
	select {
	case <-f.canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel the in-flight provider request")
	}
	f.drain(t)
	f.assertCompleted(t, otherKey, 3)
	canceled := f.client.Request.Query().Where(request.APIKeyIDEQ(f.key.ID)).OnlyX(f.ctx)
	require.Equal(t, f.key.ProjectID, canceled.ProjectID)
	execution := canceled.QueryExecutions().OnlyX(f.ctx)
	require.Equal(t, request.StatusCanceled, canceled.Status)
	require.Equal(t, requestexecution.StatusCanceled, execution.Status)
	require.Equal(t, f.key.ProjectID, execution.ProjectID)
	require.Zero(t, canceled.QueryUsageLogs().CountX(f.ctx))
	require.Equal(t, 4, f.client.Request.Query().CountX(f.ctx))
	require.Equal(t, 4, f.client.RequestExecution.Query().CountX(f.ctx))
	require.Equal(t, 3, f.client.UsageLog.Query().CountX(f.ctx))
}

// The real provider's completion is the first event. Hold the next read until
// this generation is canceled, fixing the completion-before-EOF race from F1.
type wsObservationTerminalCancellationStream struct {
	streams.Stream[*httpclient.StreamEvent]
	ctx    context.Context
	cancel context.CancelFunc
	first  bool
	closed *atomic.Int32
}

func (s *wsObservationTerminalCancellationStream) Next() bool {
	if s.first {
		if s.cancel != nil {
			s.cancel()
		}
		<-s.ctx.Done()
	}
	s.first = true
	return s.Stream.Next()
}

func (s *wsObservationTerminalCancellationStream) Close() error {
	s.closed.Add(1)
	return s.Stream.Close()
}

func TestResponsesWebSocketObservationCompletionSurvivesCancellationBeforeEOF(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newWSObservationFixture(t)
			f.handler.turnTimeout = 200 * time.Millisecond
			var cancelGeneration context.CancelFunc
			if mode == "cancel" {
				authenticate := f.handler.authenticate
				f.handler.authenticate = func(c *gin.Context) (context.Context, int, error) {
					ctx, status, err := authenticate(c)
					if err == nil {
						ctx, cancelGeneration = context.WithCancel(ctx)
					}
					return ctx, status, err
				}
			}
			var closed atomic.Int32
			process := f.handler.process
			f.handler.process = func(ctx context.Context, req *httpclient.Request, obs orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				result, err := process(ctx, req, obs)
				if err == nil && result.ChatCompletionStream != nil {
					result.ChatCompletionStream = &wsObservationTerminalCancellationStream{Stream: result.ChatCompletionStream, ctx: ctx, cancel: cancelGeneration, closed: &closed}
				}
				return result, err
			}
			conn := f.dial(t, f.key)
			for _, frame := range []string{
				`{"type":"response.create","model":"m","input":"full"}`,
				`{"type":"response.create","model":"m","previous_response_id":"resp_1","input":"suffix"}`,
			} {
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
				require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
				wsObservationBarrier(t, conn) // No second incomplete_stream may precede this barrier.
			}
			f.drain(t)
			f.assertCompleted(t, f.key, 2)
			require.Equal(t, int32(2), f.calls.Load(), "suffix must reach the provider again")
			require.Equal(t, int32(2), closed.Load(), "each raw stream must close/join exactly once")
			require.Equal(t, 2, f.client.Request.Query().CountX(f.ctx))
			require.Equal(t, 2, f.client.RequestExecution.Query().CountX(f.ctx))
			require.Equal(t, 2, f.client.UsageLog.Query().CountX(f.ctx))
			<-f.received
			var expanded struct {
				Input []json.RawMessage `json:"input"`
			}
			require.NoError(t, json.Unmarshal(<-f.received, &expanded))
			require.Len(t, expanded.Input, 3)
			require.Contains(t, string(expanded.Input[1]), "answer")
			require.Contains(t, string(expanded.Input[2]), "suffix")
		})
	}
}

func TestResponsesWebSocketObservationChannelSwitchPerGeneration(t *testing.T) {
	f := newWSObservationFixture(t)
	conn := f.dial(t, f.key)
	for i, routeIndex := range []int{0, 1, 0} {
		route := f.routes[routeIndex]
		f.selector.selected.Store(route)
		frame := map[string]any{"type": "response.create", "model": "m", "input": fmt.Sprintf("turn-%d", i)}
		if i > 0 {
			frame["previous_response_id"] = fmt.Sprintf("resp_%d", i)
		}
		require.NoError(t, conn.WriteJSON(frame))
		require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
		wsObservationBarrier(t, conn)
		f.drain(t)
		parent := f.client.Request.Query().Where(request.ExternalIDEQ(fmt.Sprintf("resp_%d", i+1))).OnlyX(f.ctx)
		execution := parent.QueryExecutions().OnlyX(f.ctx)
		billing := parent.QueryUsageLogs().OnlyX(f.ctx)
		require.Equal(t, route.Channel.ID, parent.ChannelID)
		require.Equal(t, route.Channel.ID, execution.ChannelID)
		require.Equal(t, route.Channel.ID, billing.ChannelID)
		require.Equal(t, route.Models[0].ActualModel, execution.ModelID)
		require.Equal(t, route.Models[0].ActualModel, billing.ModelID)
	}
	f.assertCompleted(t, f.key, 3)
	require.Equal(t, int32(3), f.calls.Load())
	require.Equal(t, 3, f.client.UsageLog.Query().CountX(f.ctx))
}

func TestResponsesWebSocketObservationUnavailableStillContinues(t *testing.T) {
	for _, unavailable := range []string{"full", "stopped"} {
		t.Run(unavailable, func(t *testing.T) {
			f := newWSObservationFixtureWithLimit(t, 1)
			if unavailable == "full" {
				held := f.writer.WithScope(f.ctx)
				t.Cleanup(func() { biz.EndForwardingObservation(held) })
			} else {
				require.NoError(t, f.writer.Stop(f.ctx))
			}
			conn := f.dial(t, f.key)
			for _, frame := range []string{
				`{"type":"response.create","model":"m","input":[{"role":"user","content":"first"}]}`,
				`{"type":"response.create","model":"m","previous_response_id":"resp_1","input":[{"role":"user","content":"suffix"}]}`,
			} {
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
				require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
			}
			wsObservationBarrier(t, conn)
			require.Equal(t, int32(2), f.calls.Load(), "one provider call per generation, without replay")
			<-f.received
			var resumed struct {
				Input []json.RawMessage `json:"input"`
			}
			require.NoError(t, json.Unmarshal(<-f.received, &resumed))
			require.Len(t, resumed.Input, 3)
			require.Contains(t, string(resumed.Input[1]), "answer")
			require.Contains(t, string(resumed.Input[2]), "suffix")
			require.Zero(t, f.client.Request.Query().CountX(f.ctx))
			require.Zero(t, f.client.UsageLog.Query().CountX(f.ctx))
		})
	}
}
