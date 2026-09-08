package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
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
