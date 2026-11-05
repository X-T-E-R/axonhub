package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestExecutedAccountingErrorDoesNotRetryOrDegradeProvider(t *testing.T) {
	ctx := context.Background()
	channel := &biz.Channel{Channel: &ent.Channel{ID: 7, Settings: &objects.ChannelSettings{
		RetryableErrorPatterns: []objects.RetryableErrorPattern{{Pattern: "accounting"}},
	}}}
	err := fmt.Errorf("stream close: %w", &biz.ExecutedAccountingError{Cause: context.DeadlineExceeded})
	t.Run("retry", func(t *testing.T) {
		require.False(t, isRetryableError(err), "a DB timeout is not permission to repeat the provider")
		require.False(t, isRetryableErrorForChannel(err, channel), "channel patterns cannot override executed accounting")
		outbound := &PersistentOutboundTransformer{state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: channel, Models: []biz.ChannelModelEntry{{ActualModel: "m"}, {ActualModel: "alternate"}}},
		}}
		require.False(t, outbound.CanRetry(err), "an alternate model is not permission to repeat inference")
		outbound.state.FailurePolicyRoutingChanged = true
		require.False(t, outbound.CanRetry(err), "a failure-policy change is not permission to repeat inference")
	})
	t.Run("cooldown", func(t *testing.T) {
		tracker := NewChannelRequestTracker()
		m := &rateLimitTracking{tracker: tracker, outbound: &PersistentOutboundTransformer{state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
		}}}
		rateLimit := &httpclient.Error{StatusCode: http.StatusTooManyRequests, Headers: http.Header{"Retry-After": {"30"}}}
		m.OnOutboundRawError(ctx, &biz.ExecutedAccountingError{Cause: rateLimit})
		require.False(t, tracker.IsCoolingDown(channel.ID))
		m.OnOutboundRawError(ctx, rateLimit)
		require.True(t, tracker.IsCoolingDown(channel.ID), "a real upstream 429 still sets cooldown")
	})
	t.Run("circuit-breaker", func(t *testing.T) {
		breaker := biz.NewModelCircuitBreaker()
		state := &PersistenceState{OriginalModel: "m", CurrentCandidate: &ChannelModelsCandidate{Channel: channel}}
		tracker := &modelCircuitBreakerTracker{outbound: &PersistentOutboundTransformer{state: state}, modelCircuitBreaker: breaker}
		for range 5 {
			tracker.OnOutboundRawError(ctx, err)
		}
		stats := breaker.GetModelCircuitBreakerStats(ctx, channel.ID, "m")
		require.Equal(t, 0, stats.ConsecutiveFailures)
		require.Equal(t, biz.StateClosed, stats.State)
	})
	t.Run("performance", func(t *testing.T) {
		perf := &biz.PerformanceRecord{Success: true, RequestCompleted: true, CompletionTokens: 5}
		state := &PersistenceState{Perf: perf}
		(&performanceRecording{outbound: &PersistentOutboundTransformer{state: state}}).OnOutboundRawError(ctx, err)
		require.True(t, perf.Success)
		require.Zero(t, perf.ResponseStatusCode)
		require.False(t, state.FailurePolicyRoutingChanged)
	})
}
