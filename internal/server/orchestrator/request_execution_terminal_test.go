package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

func setupRequestExecutionTerminalMiddleware(
	t *testing.T,
) (
	context.Context,
	*ent.Client,
	*biz.RequestService,
	*persistRequestExecutionMiddleware,
	*ent.RequestExecution,
) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	ctx := authz.WithTestBypass(context.Background())
	ctx = ent.NewContext(ctx, client)
	createOutboundTestPrimaryDataStorage(t, ctx, client)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	_, requestService, _, _ := setupTestServices(t, client)

	req, err := client.Request.Create().
		SetProjectID(project.ID).
		SetChannelID(channel.ID).
		SetModelID("gpt-4.1").
		SetStatus(request.StatusProcessing).
		SetRequestBody([]byte(`{"model":"gpt-4.1"}`)).
		Save(ctx)
	require.NoError(t, err)

	execution, err := client.RequestExecution.Create().
		SetProjectID(project.ID).
		SetRequestID(req.ID).
		SetChannelID(channel.ID).
		SetModelID("gpt-4.1").
		SetFormat("openai/chat_completions").
		SetRequestBody([]byte(`{"model":"gpt-4.1"}`)).
		SetStatus(requestexecution.StatusProcessing).
		Save(ctx)
	require.NoError(t, err)

	perfStart := time.Now().Add(-50 * time.Millisecond)
	outbound := &PersistentOutboundTransformer{
		state: &PersistenceState{
			RequestService: requestService,
			RequestExec:    execution,
			Perf: &biz.PerformanceRecord{
				StartTime:        perfStart,
				EndTime:          perfStart.Add(50 * time.Millisecond),
				RequestCompleted: true,
			},
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: &biz.Channel{Channel: channel},
			},
		},
	}
	middleware := persistRequestExecution(outbound).(*persistRequestExecutionMiddleware)
	_, err = middleware.OnOutboundRawResponse(ctx, &httpclient.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"id":"provider-response","choices":[]}`),
	})
	require.NoError(t, err)

	return ctx, client, requestService, middleware, execution
}

func TestFailureLatencyMetrics(t *testing.T) {
	t.Run("completed uses recorded end time and clamps", func(t *testing.T) {
		start := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
		metrics := failureLatencyMetrics(&biz.PerformanceRecord{
			StartTime: start,
			EndTime:   start.Add(time.Millisecond),
		})

		require.NotNil(t, metrics)
		require.NotNil(t, metrics.LatencyMs)
		require.EqualValues(t, biz.MinLatencyMs, *metrics.LatencyMs)
	})

	t.Run("unfinished uses current time", func(t *testing.T) {
		metrics := failureLatencyMetrics(&biz.PerformanceRecord{
			StartTime: time.Now().Add(-50 * time.Millisecond),
		})

		require.NotNil(t, metrics)
		require.NotNil(t, metrics.LatencyMs)
		require.GreaterOrEqual(t, *metrics.LatencyMs, int64(40))
	})

	require.Nil(t, failureLatencyMetrics(nil))
	require.Nil(t, failureLatencyMetrics(&biz.PerformanceRecord{}))
}

func TestPersistRequestExecutionMiddleware_TerminalOrdering(t *testing.T) {
	t.Run("post-response transform failure supersedes provisional completion", func(t *testing.T) {
		ctx, client, _, middleware, execution := setupRequestExecutionTerminalMiddleware(t)
		defer client.Close()

		_, err := middleware.OnOutboundLlmResponse(ctx, &llm.Response{ID: "provider-response"})
		require.NoError(t, err)

		execution, err = client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCompleted, execution.Status)

		transformErr := errors.New("failed to transform final response")
		middleware.OnOutboundRawError(ctx, transformErr)

		execution, err = client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, transformErr.Error(), execution.ErrorMessage)
	})

	t.Run("raw failure records latency from performance interval", func(t *testing.T) {
		ctx, client, _, middleware, execution := setupRequestExecutionTerminalMiddleware(t)
		defer client.Close()

		middleware.OnOutboundRawError(ctx, errors.New("upstream returned 502"))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.NotNil(t, execution.MetricsLatencyMs)
		require.EqualValues(t, 50, *execution.MetricsLatencyMs)
	})

	t.Run("late completion cannot overwrite earlier transform failure", func(t *testing.T) {
		ctx, client, _, middleware, execution := setupRequestExecutionTerminalMiddleware(t)
		defer client.Close()

		transformErr := errors.New("provider response transform failed")
		middleware.OnOutboundRawError(ctx, transformErr)
		_, err := middleware.OnOutboundLlmResponse(ctx, &llm.Response{ID: "late-provider-response"})
		require.NoError(t, err)

		execution, err = client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, transformErr.Error(), execution.ErrorMessage)
		require.Empty(t, execution.ExternalID)
		require.Empty(t, execution.ResponseBody)
	})

	t.Run("first-event timeout refines close cancellation evidence", func(t *testing.T) {
		ctx, client, requestService, middleware, execution := setupRequestExecutionTerminalMiddleware(t)
		defer client.Close()

		// Stream close can surface this weak cleanup error before the timeout
		// guard reports its specific causal failure.
		middleware.OnOutboundRawError(ctx, context.Canceled)

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, context.Canceled.Error(), execution.ErrorMessage)

		require.NoError(t, requestService.UpdateRequestExecutionStatusFromError(
			ctx,
			execution.ID,
			pipeline.ErrStreamFirstEventTimeout,
			nil,
		))

		execution, err = client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, pipeline.ErrStreamFirstEventTimeout.Error(), execution.ErrorMessage)
	})
}
