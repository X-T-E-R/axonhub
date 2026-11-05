package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type observationCloseBarrier struct {
	transformer.Inbound

	entered chan struct{}
	release chan struct{}
}

func (b *observationCloseBarrier) TransformStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*httpclient.StreamEvent], error) {
	s, err := b.Inbound.TransformStream(ctx, stream)
	if err != nil {
		return nil, err
	}
	return &observationCloseBarrierStream{Stream: s, barrier: b}, nil
}

type observationCloseBarrierStream struct {
	streams.Stream[*httpclient.StreamEvent]

	barrier *observationCloseBarrier
}

func (s *observationCloseBarrierStream) Close() error {
	close(s.barrier.entered)
	<-s.barrier.release
	return s.Stream.Close()
}

type observationNoPrompts struct{}

func (observationNoPrompts) GetEnabledPrompts(context.Context, int) ([]*ent.Prompt, error) {
	return nil, nil
}

// The raw response may finish and both HTTP middlewares unwind while the
// transformed pipeline has not yet submitted its execution, usage or parent.
func TestObservationScopeOutlivesHTTPPassThrough(t *testing.T) {
	for _, passThrough := range []bool{true, false} {
		t.Run(fmt.Sprintf("passthrough=%v", passThrough), func(t *testing.T) {
			client := enttest.NewEntClient(t, "sqlite3", "file:observation-http?mode=memory&_fk=0")
			defer client.Close()
			ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
			project := client.Project.Create().SetName("scope test").SaveX(ctx)
			ctx = contexts.WithProjectID(ctx, project.ID)
			row := client.Channel.Create().SetType(channel.TypeOpenai).SetName("scope mock").
				SetBaseURL("https://unused.invalid/v1").SetCredentials(objects.ChannelCredentials{APIKey: "fixture"}).
				SetSupportedModels([]string{"gpt-4"}).SetDefaultTestModel("gpt-4").
				SetSettings(&objects.ChannelSettings{PassThroughBody: lo.ToPtr(passThrough)}).SaveX(ctx)
			channelService, legacy, system, usage := setupSpeechAPITestServices(t, client)
			require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{}))
			writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{AttemptTimeout: time.Second})
			service := biz.NewRequestServiceWithObservationWriter(client, system, usage, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, writer)
			require.NoError(t, writer.Start(ctx))
			defer func() {
				stop, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				require.NoError(t, writer.Stop(stop))
			}()
			provider, err := responses.NewOutboundTransformer(row.BaseURL, "fixture")
			require.NoError(t, err)
			barrier := &observationCloseBarrier{Inbound: responses.NewInboundTransformer(), entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(barrier.release) })
			executor := &speechAPIExecutor{streamEvents: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp-scope","model":"gpt-4","status":"in_progress","output":[]}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp-scope","model":"gpt-4","status":"completed","output":[{"type":"message","id":"msg-scope","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ready","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`)},
			}}
			processor := orchestrator.NewChatCompletionOrchestrator(channelService, nil, service, httpclient.NewHttpClient(), barrier,
				system, usage, nil, nil, nil, legacy.LiveStreamRegistry, orchestrator.NewChannelLimiterManager(), nil).
				WithChannelSelector(&speechAPISelector{candidates: []*orchestrator.ChannelModelsCandidate{{
					Channel: &biz.Channel{Channel: row, Outbound: provider},
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4", Source: "direct"}},
				}}})
			processor.PipelineFactory = pipeline.NewFactory(executor)
			processor.PromptProvider = observationNoPrompts{}
			processor.PromptProtecter = nil
			handler := NewChatCompletionHandlers(processor)
			handler.sseHeartbeatFormat = sseHeartbeatOpenAI
			traceService := biz.NewTraceService(biz.TraceServiceParams{Ent: client, RequestService: service})
			router := gin.New()
			router.Use(middleware.WithThread(tracing.Config{}, biz.NewThreadService(client, traceService)), middleware.WithTrace(tracing.Config{}, traceService))
			router.POST("/v1/responses", func(c *gin.Context) {
				handler.ChatCompletionWithRequest(c, &httpclient.Request{
					Method: http.MethodPost, APIFormat: string(llm.APIFormatOpenAIResponse),
					Headers: http.Header{"Content-Type": {"application/json"}},
					Body:    []byte(`{"model":"gpt-4","input":"hi","stream":true}`),
				})
			})
			w := httptest.NewRecorder()
			httpDone := make(chan struct{})
			go func() {
				defer close(httpDone)
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("handler panic: %v", p)
					}
				}()
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")).WithContext(ctx)
				router.ServeHTTP(w, req)
			}()
			select {
			case <-barrier.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("pipeline did not reach Close barrier")
			}
			if passThrough {
				select {
				case <-httpDone:
				case <-time.After(time.Second):
					t.Fatal("HTTP terminal waited on transformed Close")
				}
				require.Contains(t, w.Body.String(), "response.completed")
			} else {
				select {
				case <-httpDone:
					t.Fatal("transformed handler returned before stream Close")
				default:
				}
			}
			release.Do(func() { close(barrier.release) })
			select {
			case <-httpDone:
			case <-time.After(time.Second):
				t.Fatal("HTTP handler did not finish")
			}
			drain, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, writer.Wait(drain))
			require.Equal(t, http.StatusOK, w.Code)
			parent := client.Request.Query().OnlyX(ctx)
			execution := client.RequestExecution.Query().OnlyX(ctx)
			require.Equal(t, request.StatusCompleted, parent.Status)
			require.Equal(t, requestexecution.StatusCompleted, execution.Status)
			billing := client.UsageLog.Query().OnlyX(ctx)
			require.Equal(t, int64(11), billing.PromptTokens)
			require.Equal(t, int64(7), billing.CompletionTokens)
			require.Equal(t, int64(18), billing.TotalTokens)
			require.Equal(t, parent.ID, billing.RequestID)
			require.Equal(t, parent.ID, execution.RequestID)
		})
	}
}

type observationLateStream struct {
	streams.Stream[*httpclient.StreamEvent]

	onClose func() error
	once    sync.Once
	err     error
}

func (s *observationLateStream) Close() error {
	s.once.Do(func() { s.err = s.onClose() })
	return s.err
}

func TestObservationScopeOutlivesAbandonedProcess(t *testing.T) {
	for _, lateStream := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-stream=%v", lateStream), func(t *testing.T) {
			client := enttest.NewEntClient(t, "sqlite3", "file:observation-abandon?mode=memory&_fk=0")
			defer client.Close()
			base := ent.NewContext(authz.WithTestBypass(context.Background()), client)
			project := client.Project.Create().SetName("abandon test").SaveX(base)
			base = contexts.WithProjectID(base, project.ID)
			_, legacy, system, usage := setupSpeechAPITestServices(t, client)
			writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{})
			service := biz.NewRequestServiceWithObservationWriter(client, system, usage, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, writer)
			require.NoError(t, writer.Start(base))
			defer func() {
				stop, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				require.NoError(t, writer.Stop(stop))
			}()
			scoped := service.ForwardingObservationContext(base)
			ctx, cancel := context.WithCancelCause(scoped)
			defer cancel(nil)
			session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{}, sseHeartbeatOpenAI, true)
			entered, release := make(chan struct{}), make(chan struct{})
			persisted := make(chan error, 1)
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			process := func(worker context.Context, _ *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				parent, err := service.CreateRequest(worker, &llm.Request{Model: "gpt-4", Stream: lo.ToPtr(true)}, &httpclient.Request{}, llm.APIFormatOpenAIResponse)
				if err != nil {
					persisted <- err
					return orchestrator.ChatCompletionResult{}, err
				}
				close(entered)
				<-release
				finish := func() error {
					defer biz.EndForwardingObservation(worker)
					err := service.UpdateRequestStatusFromError(worker, parent.ID, context.Canceled, context.Canceled)
					persisted <- err
					return err
				}
				if lateStream {
					return orchestrator.ChatCompletionResult{ChatCompletionStream: &observationLateStream{
						Stream: streams.SliceStream([]*httpclient.StreamEvent{}), onClose: finish,
					}}, nil
				}
				_ = finish()
				return orchestrator.ChatCompletionResult{}, context.Canceled
			}
			finished := make(chan bool, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("await process panic: %v", p)
					}
				}()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				_, _, aborted := session.awaitProcess(c, process, &httpclient.Request{})
				// The handler and both scope-ending middlewares are now finished.
				biz.EndForwardingObservation(scoped)
				finished <- aborted
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("process did not start")
			}
			cancel(context.Canceled)
			select {
			case aborted := <-finished:
				require.True(t, aborted)
			case <-time.After(time.Second):
				t.Fatal("handler waited for abandoned process")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-persisted:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("late process did not persist cancellation")
			}
			drain, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			require.NoError(t, writer.Wait(drain))
			require.Equal(t, request.StatusCanceled, client.Request.Query().OnlyX(base).Status)
			require.True(t, errors.Is(ctx.Err(), context.Canceled), "retention must not detach provider cancellation")
		})
	}
}

type observationPanicWriter struct {
	gin.ResponseWriter

	panicAt int
	flushes int
}

func (w *observationPanicWriter) FlushError() error {
	w.flushes++
	if w.flushes == w.panicAt {
		panic("fixture flush panic")
	}
	w.ResponseWriter.Flush()
	return nil
}

func TestObservationScopeReleasedAfterWriterPanic(t *testing.T) {
	for _, panicAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("flush=%d", panicAt), func(t *testing.T) {
			writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{MaxItems: 5})
			require.NoError(t, writer.Start(t.Context()))
			scoped := writer.WithScope(t.Context())
			ctx, cancel := context.WithCancelCause(scoped)
			session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}, sseHeartbeatOpenAI, true)
			release := make(chan struct{})
			closed := make(chan struct{})
			var releaseOnce sync.Once
			late := &observationLateStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{}), onClose: func() error {
				biz.EndForwardingObservation(scoped)
				close(closed)
				return nil
			}}
			t.Cleanup(func() {
				// Also join the worker when the pre-repair implementation fails.
				session.abandon()
				releaseOnce.Do(func() { close(release) })
				cancel(nil)
				biz.EndForwardingObservation(scoped)
				stop, stopCancel := context.WithTimeout(context.Background(), time.Second)
				defer stopCancel()
				require.NoError(t, writer.Stop(stop))
			})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			c.Writer = &observationPanicWriter{ResponseWriter: c.Writer, panicAt: panicAt}
			process := func(_ context.Context, _ *httpclient.Request, observer orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				observer.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{})
				<-release
				return orchestrator.ChatCompletionResult{ChatCompletionStream: late}, nil
			}
			require.PanicsWithValue(t, "fixture flush panic", func() {
				// Mirror ChatCompletionWithRequest and middleware unwind on panic.
				defer biz.EndForwardingObservation(scoped)
				defer cancel(nil)
				_, _, _ = session.awaitProcess(c, process, &httpclient.Request{})
			})
			releaseOnce.Do(func() { close(release) })
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("writer panic stranded the late stream and observation owner")
			}
			drain, drainCancel := context.WithTimeout(context.Background(), time.Second)
			defer drainCancel()
			// Wait includes all five lifecycle reservations, not just queued jobs.
			require.NoError(t, writer.Wait(drain), "panic must return the retained scope's capacity")
		})
	}
}

// Exercise the real Chat HTTP -> pipeline -> required writer path without bodies.
type observationAccountingFixture struct {
	ctx      context.Context
	db       *ent.Client
	writer   *biz.ForwardingObservationWriter
	handler  *ChatCompletionHandlers
	executor *observationAccountingExecutor
}

type observationAccountingExecutor struct {
	events []*httpclient.StreamEvent
	calls  atomic.Int32
	source func(context.Context) streams.Stream[*httpclient.StreamEvent]
}

func (e *observationAccountingExecutor) Do(context.Context, *httpclient.Request) (*httpclient.Response, error) {
	return nil, errors.New("unexpected non-stream request")
}
func (e *observationAccountingExecutor) DoStream(ctx context.Context, _ *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	e.calls.Add(1)
	if e.source != nil {
		return e.source(ctx), nil
	}
	return streams.SliceStream(e.events), nil
}

func newObservationAccountingFixture(t *testing.T, clustered, passThrough bool, events []*httpclient.StreamEvent) *observationAccountingFixture {
	t.Helper()
	db := enttest.NewEntClient(t, "sqlite3", "file:observation-accounting-"+uuid.NewString()+"?mode=memory&_fk=0")
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), db)
	project := db.Project.Create().SetName("accounting").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)
	row := db.Channel.Create().SetName("accounting").SetType(channel.TypeOpenai).SetStatus(channel.StatusEnabled).
		SetBaseURL("https://unused.invalid/v1").SetCredentials(objects.ChannelCredentials{APIKey: "fixture"}).
		SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").
		SetSettings(&objects.ChannelSettings{PassThroughBody: lo.ToPtr(passThrough)}).SaveX(ctx)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: db})
	require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{}))
	require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 2, MaxSingleChannelRetries: 2}))
	node := cluster.Config{Enabled: clustered, NodeID: "a", ControlNode: "a", Nodes: map[string]string{"a": "http://127.0.0.1:1"}, Channels: map[int]cluster.ChannelBinding{row.ID: {Owner: "a", Identity: "fixture"}}}
	httpClient := httpclient.NewHttpClient()
	channels := biz.NewChannelService(biz.ChannelServiceParams{CacheConfig: cache, Ent: db, SystemService: system, HttpClient: httpClient, WebhookNotifier: biz.NewWebhookNotifier(system, httpClient), Node: node})
	t.Cleanup(channels.Stop)
	usage := biz.NewUsageLogService(db, system, channels)
	storage := biz.NewDataStorageService(biz.DataStorageServiceParams{CacheConfig: cache, SystemService: system, Client: db})
	writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{AttemptTimeout: time.Second})
	service := biz.NewRequestServiceWithObservationWriter(db, system, usage, storage, biz.NewLiveStreamRegistry(), nil, writer)
	require.NoError(t, writer.Start(ctx))
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, writer.Stop(stop))
	})
	ctx = service.ForwardingObservationContext(ctx)
	t.Cleanup(func() { biz.EndForwardingObservation(ctx) })
	provider, err := openai.NewOutboundTransformer(row.BaseURL, "fixture")
	require.NoError(t, err)
	processor := orchestrator.NewChatCompletionOrchestrator(channels, nil, service, httpClient, openai.NewInboundTransformer(), system, usage, nil, nil, nil, nil, orchestrator.NewChannelLimiterManager(), nil).
		WithChannelSelector(&speechAPISelector{candidates: []*orchestrator.ChannelModelsCandidate{{Channel: &biz.Channel{Channel: row, Outbound: provider}, Models: []biz.ChannelModelEntry{{RequestModel: "m", ActualModel: "m"}}}}})
	processor.PromptProvider, processor.PromptProtecter = observationNoPrompts{}, nil
	executor := &observationAccountingExecutor{events: events}
	processor.PipelineFactory = pipeline.NewFactory(executor)
	handler := NewChatCompletionHandlers(processor)
	handler.sseHeartbeatFormat = sseHeartbeatOpenAI
	return &observationAccountingFixture{ctx: ctx, db: db, writer: writer, handler: handler, executor: executor}
}

func (f *observationAccountingFixture) run(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	path, format := "/v1/chat/completions", llm.APIFormatOpenAIChatCompletion
	if f.handler.sseHeartbeatFormat == sseHeartbeatAnthropic {
		path, format = "/v1/messages", llm.APIFormatAnthropicMessage
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")).WithContext(f.ctx)
	f.handler.ChatCompletionWithRequest(c, &httpclient.Request{
		Method: http.MethodPost, APIFormat: string(format), Headers: http.Header{"Content-Type": {"application/json"}},
		Body: []byte(`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`),
	})
	biz.EndForwardingObservation(f.ctx)
	drain, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, f.writer.Wait(drain))
	require.Equal(t, int32(1), f.executor.calls.Load(), "accounting must never replay the provider")
	readCtx := authz.WithTestBypass(context.Background())
	require.Zero(t, f.db.PendingUsage.Query().CountX(readCtx))
	for _, row := range f.db.Request.Query().AllX(readCtx) {
		require.Contains(t, []string{"", "{}"}, string(row.RequestBody))
		require.Empty(t, row.ResponseBody)
		require.Empty(t, row.ResponseChunks)
	}
	for _, row := range f.db.RequestExecution.Query().AllX(readCtx) {
		require.Contains(t, []string{"", "{}"}, string(row.RequestBody))
		require.Empty(t, row.ResponseBody)
		require.Empty(t, row.ResponseChunks)
	}
	require.Zero(t, f.db.ObservabilityPayload.Query().CountX(readCtx))
	return w.Body.String()
}

func observationAccountingEvent(data string) *httpclient.StreamEvent {
	return &httpclient.StreamEvent{Data: []byte(data)}
}

func TestObservationAccountingFreezesFinalChatUsage(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		for _, passThrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("cluster=%v/passthrough=%v", clustered, passThrough), func(t *testing.T) {
				events := []*httpclient.StreamEvent{
					observationAccountingEvent(`{"id":"chat-usage","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"a"},"finish_reason":null}],"usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}`),
					observationAccountingEvent(`{"id":"chat-usage","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`),
					observationAccountingEvent(`{"id":"chat-usage","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
					observationAccountingEvent(`[DONE]`),
				}
				f := newObservationAccountingFixture(t, clustered, passThrough, events)
				body := f.run(t)
				require.Contains(t, body, "[DONE]")
				bill := f.db.UsageLog.Query().OnlyX(f.ctx)
				require.Equal(t, int64(5), bill.TotalTokens)
				require.Nil(t, bill.TotalCost)
				t.Logf("provider=1 usage=1 final_tokens=%d pending=0", bill.TotalTokens)
			})
		}
	}
}

func TestObservationAccountingPrecedesSemanticCompletion(t *testing.T) {
	for _, passThrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%v", passThrough), func(t *testing.T) {
			events := []*httpclient.StreamEvent{
				observationAccountingEvent(`{"id":"chat-slow","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`),
				observationAccountingEvent(`{"id":"chat-slow","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
				observationAccountingEvent(`{"id":"chat-slow","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
				observationAccountingEvent(`[DONE]`),
			}
			f := newObservationAccountingFixture(t, true, passThrough, events)
			var mu sync.Mutex
			var frozen []string
			f.db.PendingUsage.Use(func(next ent.Mutator) ent.Mutator {
				return hook.PendingUsageFunc(func(ctx context.Context, m *ent.PendingUsageMutation) (ent.Value, error) {
					if m.Op().Is(ent.OpCreate) {
						id, _ := m.ID()
						stamp, _ := m.CapturedAt()
						tokens, _ := m.TotalTokens()
						cost, known := m.TotalCost()
						usage, _ := m.Usage()
						fact, err := json.Marshal([]any{id, stamp, tokens, cost, known, usage})
						if err != nil {
							return nil, err
						}
						mu.Lock()
						frozen = append(frozen, string(fact))
						first := len(frozen) == 1
						mu.Unlock()
						if first {
							time.Sleep(2500 * time.Millisecond)
							return nil, errors.New("slow pending publication failure")
						}
					}
					return next.Mutate(ctx, m)
				})
			})
			body := f.run(t)
			require.NotContains(t, body, "[DONE]", "semantic grace cannot bypass required accounting")
			require.NotContains(t, body, `"type":"response.completed"`)
			require.Contains(t, body, "event:error")
			require.Contains(t, body, "accounting failed after upstream execution")
			require.NotContains(t, body, `"execution":"not_executed"`)
			bill := f.db.UsageLog.Query().OnlyX(f.ctx)
			require.Equal(t, int64(5), bill.TotalTokens)
			require.Nil(t, bill.TotalCost)
			mu.Lock()
			defer mu.Unlock()
			require.GreaterOrEqual(t, len(frozen), 2)
			for _, fact := range frozen[1:] {
				require.Equal(t, frozen[0], fact)
			}
			t.Logf("provider=1 usage=1 final_tokens=5 pending=0 accounting_error=true publication_attempts=%d", len(frozen))
		})
	}
}

type observationAccountingCanceledStream struct {
	ctx     context.Context
	event   *httpclient.StreamEvent
	emitted bool
}

func (s *observationAccountingCanceledStream) Next() bool {
	if !s.emitted {
		s.emitted = true
		return true
	}
	<-s.ctx.Done()
	return false
}
func (s *observationAccountingCanceledStream) Current() *httpclient.StreamEvent { return s.event }
func (s *observationAccountingCanceledStream) Err() error                       { return context.Cause(s.ctx) }
func (s *observationAccountingCanceledStream) Close() error                     { return nil }

func TestObservationAccountingEOFAndCancellation(t *testing.T) {
	for _, passThrough := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/canceled=%v", passThrough, canceled), func(t *testing.T) {
				partial := observationAccountingEvent(`{"id":"chat-short","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`)
				f := newObservationAccountingFixture(t, true, passThrough, []*httpclient.StreamEvent{partial})
				if canceled {
					f.executor.source = func(ctx context.Context) streams.Stream[*httpclient.StreamEvent] {
						return &observationAccountingCanceledStream{ctx: ctx, event: partial}
					}
					ctx, cancel := context.WithTimeout(f.ctx, 30*time.Millisecond)
					defer cancel()
					f.ctx = ctx
				}
				start := time.Now()
				body := f.run(t)
				require.Less(t, time.Since(start), time.Second)
				require.NotContains(t, body, "[DONE]")
			})
		}
	}
}

func TestObservationAccountingGuardsTransformedMessagesTerminal(t *testing.T) {
	for _, failPublication := range []bool{false, true} {
		t.Run(fmt.Sprintf("publication_failure=%v", failPublication), func(t *testing.T) {
			events := []*httpclient.StreamEvent{
				observationAccountingEvent(`{"id":"chat-converted","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`),
				observationAccountingEvent(`{"id":"chat-converted","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`),
				observationAccountingEvent(`{"id":"chat-converted","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
				observationAccountingEvent(`[DONE]`),
			}
			f := newObservationAccountingFixture(t, true, false, events)
			f.handler.ChatCompletionOrchestrator.Inbound = anthropic.NewInboundTransformer()
			f.handler.sseHeartbeatFormat = sseHeartbeatAnthropic
			var attempts atomic.Int32
			f.db.PendingUsage.Use(func(next ent.Mutator) ent.Mutator {
				return hook.PendingUsageFunc(func(ctx context.Context, m *ent.PendingUsageMutation) (ent.Value, error) {
					if m.Op().Is(ent.OpCreate) && attempts.Add(1) == 1 && failPublication {
						return nil, errors.New("converted stream pending publication failure")
					}
					return next.Mutate(ctx, m)
				})
			})
			body := f.run(t)
			if failPublication {
				require.NotContains(t, body, "event:message_stop")
				require.Contains(t, body, "accounting failed after upstream execution")
			} else {
				require.Contains(t, body, "event:message_stop")
			}
			require.Equal(t, int64(5), f.db.UsageLog.Query().OnlyX(f.ctx).TotalTokens)
		})
	}
}

func TestObservationAccountingLostCommitACKDoesNotReplay(t *testing.T) {
	for _, passThrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%v", passThrough), func(t *testing.T) {
			events := []*httpclient.StreamEvent{
				observationAccountingEvent(`{"id":"chat-commit","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`),
				observationAccountingEvent(`[DONE]`),
			}
			f := newObservationAccountingFixture(t, true, passThrough, events)
			var publications, commits atomic.Int32
			f.db.PendingUsage.Use(func(next ent.Mutator) ent.Mutator {
				return hook.PendingUsageFunc(func(ctx context.Context, m *ent.PendingUsageMutation) (ent.Value, error) {
					if m.Op().Is(ent.OpCreate) {
						publications.Add(1)
					}
					return next.Mutate(ctx, m)
				})
			})
			f.db.UsageLog.Use(func(next ent.Mutator) ent.Mutator {
				return hook.UsageLogFunc(func(ctx context.Context, m *ent.UsageLogMutation) (ent.Value, error) {
					tx := ent.TxFromContext(ctx)
					if tx == nil {
						return nil, errors.New("usage write bypassed the handoff transaction")
					}
					tx.OnCommit(func(next ent.Committer) ent.Committer {
						return ent.CommitFunc(func(ctx context.Context, tx *ent.Tx) error {
							if err := next.Commit(ctx, tx); err != nil {
								return err
							}
							commits.Add(1)
							return context.DeadlineExceeded
						})
					})
					return next.Mutate(ctx, m)
				})
			})
			body := f.run(t) // Checks provider=1, pending=0 and no stored bodies.
			require.Contains(t, body, "[DONE]")
			require.Equal(t, int32(1), commits.Load())
			require.Equal(t, int32(1), publications.Load())
			require.Equal(t, int64(5), f.db.UsageLog.Query().OnlyX(authz.WithTestBypass(context.Background())).TotalTokens)
		})
	}
}
