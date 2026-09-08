package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	serverdb "github.com/looplj/axonhub/internal/server/db"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

type observationBarrierExecutor struct {
	entered    <-chan struct{}
	called     chan struct{}
	retry      bool
	calls      atomic.Int32
	longStream bool
}

func (e *observationBarrierExecutor) Do(ctx context.Context, _ *httpclient.Request) (*httpclient.Response, error) {
	select {
	case <-e.entered:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.calls.Add(1) == 1 {
		close(e.called)
		if e.retry {
			return nil, &httpclient.Error{StatusCode: 500, Status: "500 Internal Server Error", Body: []byte(`{"error":{"message":"first attempt failed","type":"api_error"}}`)}
		}
	}
	return &httpclient.Response{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}}, Body: buildMockOpenAIResponse("observation-test", "gpt-4", "ready", 2, 3)}, nil
}

func (e *observationBarrierExecutor) DoStream(ctx context.Context, _ *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	select {
	case <-e.entered:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if e.calls.Add(1) == 1 {
		close(e.called)
	}
	content, count := "ready", 1
	if e.longStream {
		content, count = strings.Repeat("x", 1024), 20000
	}
	chunk := &httpclient.StreamEvent{Data: []byte(fmt.Sprintf(`{"id":"observation-test","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":"%s"},"finish_reason":null}]}`, content))}
	events := make([]*httpclient.StreamEvent, count, count+2)
	for i := range events {
		events[i] = chunk
	}
	events = append(events,
		&httpclient.StreamEvent{Data: []byte(`{"id":"observation-test","object":"chat.completion.chunk","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)},
		&httpclient.StreamEvent{Data: []byte(`[DONE]`)},
	)
	return streams.SliceStream(events), nil
}

func TestForwardingObservationCoreBarrier(t *testing.T) {
	for _, tc := range []struct{ stream, postgres, retry, disabled, longStream bool }{{false, false, false, false, false}, {true, false, false, false, false}, {true, true, false, false, false}, {false, false, true, false, false}, {true, false, false, true, false}, {true, false, false, false, true}, {true, false, false, true, true}} {
		stream := tc.stream
		t.Run(fmt.Sprintf("stream=%v/postgres=%v/retry=%v/disabled=%v/long=%v", stream, tc.postgres, tc.retry, tc.disabled, tc.longStream), func(t *testing.T) {
			var client *ent.Client
			if tc.postgres {
				dsn := os.Getenv("AXONHUB_TEST_PG_DSN")
				if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
					t.Skip("requires disposable PostgreSQL")
				}
				client = serverdb.NewEntClient(serverdb.Config{Dialect: "postgres", DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 4})
			} else {
				client = enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:observation-core-%v?mode=memory&_fk=0", stream))
			}
			defer client.Close()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unlock := func() {}
			releaseBarrier := func() { unlock(); close(release) }
			defer releaseOnce.Do(releaseBarrier)
			executor := &observationBarrierExecutor{entered: entered, called: make(chan struct{}), retry: tc.retry, longStream: tc.longStream}
			orchestrator, ctx := setupAsyncManagedBodyOrchestrator(t, client, nil, executor)
			if tc.disabled {
				require.NoError(t, orchestrator.SystemService.SetStoragePolicy(ctx, &biz.StoragePolicy{}))
			} else if tc.stream && !tc.postgres {
				require.NoError(t, orchestrator.SystemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true}))
			}
			if tc.retry {
				require.NoError(t, orchestrator.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{Enabled: true, MaxSingleChannelRetries: 1, LoadBalancerStrategy: biz.LoadBalancerStrategyAdaptive}))
			}
			if tc.postgres {
				require.NoError(t, orchestrator.SystemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true, ManagedObservabilityHardMiB: lo.ToPtr(64), ManagedObservabilityLowMiB: lo.ToPtr(48)}))
				tx, err := client.Tx(ctx)
				require.NoError(t, err)
				_, err = tx.ManagedObservabilityState.Query().Modify(func(s *entsql.Selector) { s.ForUpdate() }).Only(ctx)
				require.NoError(t, err)
				unlock = func() { require.NoError(t, tx.Rollback()) }
				// A real PG row lock is held for the entire forwarding proof.
				close(entered)
			}
			writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{AttemptTimeout: 30 * time.Second})
			old := orchestrator.RequestService
			orchestrator.RequestService = biz.NewRequestServiceWithObservationWriter(client, orchestrator.SystemService, orchestrator.UsageLogService, old.DataStorageService, old.LiveStreamRegistry, nil, writer)
			require.NoError(t, writer.Start(ctx))
			defer func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = writer.Stop(stopCtx)
			}()
			channel := orchestrator.channelSelector.(*staticChannelSelector).candidates[0].Channel
			channel.Settings = &objects.ChannelSettings{RateLimit: &objects.ChannelRateLimit{MaxConcurrent: lo.ToPtr(int64(1))}}
			if tc.retry {
				channel.Settings.PassThroughBody = lo.ToPtr(true)
				channel.Settings.StoreExecutionResponseBody = lo.ToPtr(true)
			}
			limiter := orchestrator.channelLimiterManager.GetOrCreate(channel)
			var enteredOnce sync.Once
			if !tc.postgres {
				client.Request.Use(func(next ent.Mutator) ent.Mutator {
					return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
						if mutation.Op().Is(ent.OpCreate) {
							enteredOnce.Do(func() { close(entered) })
							select {
							case <-release:
							case <-hookCtx.Done():
								return nil, hookCtx.Err()
							}
						}
						return next.Mutate(hookCtx, mutation)
					})
				})
			}
			finished := make(chan error, 1)
			first := make(chan struct{})
			go func() {
				defer func() {
					if p := recover(); p != nil {
						finished <- fmt.Errorf("panic: %v", p)
					}
				}()
				result, err := orchestrator.Process(ctx, buildTestRequest("gpt-4", "barrier", stream))
				if err == nil && stream {
					seen := false
					count := 0
					for result.ChatCompletionStream.Next() {
						_ = result.ChatCompletionStream.Current()
						count++
						if !seen {
							seen = true
							close(first)
						}
					}
					if tc.longStream && count < 20000 {
						finished <- fmt.Errorf("forwarded only %d of 20000 content frames", count)
						return
					}
					err = result.ChatCompletionStream.Close()
					if err == nil {
						err = result.ChatCompletionStream.Err()
					}
				}
				finished <- err
			}()
			select {
			case <-executor.called:
			case <-time.After(3 * time.Second):
				t.Fatal("provider dispatch waited on request INSERT")
			}
			if stream {
				select {
				case <-first:
				case <-time.After(time.Second):
					t.Fatal("first event waited on request INSERT")
				}
			}
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("terminal/close waited on request INSERT")
			}
			inFlight, _ := limiter.Stats()
			require.Zero(t, inFlight, "channel slot must be released before storage barrier")
			if tc.postgres {
				require.Eventually(t, func() bool {
					var rows entsql.Rows
					err := client.Driver().Query(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query ILIKE '%managed_observability_states%')", []any{}, &rows)
					if err != nil {
						return false
					}
					defer rows.Close()
					var blocked bool
					return rows.Next() && rows.Scan(&blocked) == nil && blocked
				}, 2*time.Second, 10*time.Millisecond, "worker must actually wait on the held state row lock")
			}
			releaseOnce.Do(releaseBarrier)
			drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, writer.Wait(drainCtx))
			parent := client.Request.Query().OnlyX(ctx)
			executions := client.RequestExecution.Query().Order(ent.Asc(requestexecution.FieldCreatedAt)).AllX(ctx)
			if tc.retry {
				require.Len(t, executions, 2)
				require.Equal(t, requestexecution.StatusFailed, executions[0].Status)
				require.Contains(t, string(executions[0].ResponseBody), "first attempt failed")
				require.Equal(t, int32(2), executor.calls.Load())
			} else {
				require.Len(t, executions, 1)
			}
			execution := executions[len(executions)-1]
			require.Equal(t, request.StatusCompleted, parent.Status)
			require.Equal(t, requestexecution.StatusCompleted, execution.Status)
			require.Equal(t, parent.ID, execution.RequestID)
			usage := client.UsageLog.Query().OnlyX(ctx)
			require.Equal(t, parent.ID, usage.RequestID)
			require.Equal(t, int64(5), usage.TotalTokens)
			if tc.disabled {
				require.Equal(t, "omitted", parent.EvidenceDisposition.ResponseBody.Outcome)
				require.Equal(t, "omitted", execution.EvidenceDisposition.ResponseBody.Outcome)
				require.Equal(t, "omit", parent.EvidenceDisposition.ResponseChunks.Intent)
				require.Equal(t, "omit", execution.EvidenceDisposition.ResponseChunks.Intent)
			} else if tc.longStream {
				require.Equal(t, "unavailable", parent.EvidenceDisposition.ResponseBody.Outcome)
				require.Equal(t, "unavailable", execution.EvidenceDisposition.ResponseBody.Outcome)
				require.Equal(t, "unavailable", parent.EvidenceDisposition.ResponseChunks.Outcome)
				require.Equal(t, "unavailable", execution.EvidenceDisposition.ResponseChunks.Outcome)
			} else if tc.stream && !tc.postgres {
				require.Contains(t, string(parent.ResponseBody), "ready")
				require.Contains(t, string(execution.ResponseBody), "ready")
				require.NotEmpty(t, parent.ResponseChunks)
				require.NotEmpty(t, execution.ResponseChunks)
			}
		})
	}
}
