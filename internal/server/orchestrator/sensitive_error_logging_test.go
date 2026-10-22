package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

type diagnosticCaptureKey struct{}

type diagnosticCapture struct {
	mu   sync.Mutex
	text strings.Builder
}

func captureErrorDiagnostics(ctx context.Context) (context.Context, func() string) {
	capture := &diagnosticCapture{}
	ctx = context.WithValue(log.ForceEnableLevel(ctx, log.DebugLevel), diagnosticCaptureKey{}, capture)
	log.GetGlobalLogger().AddHook(log.HookFunc(func(ctx context.Context, message string, fields ...log.Field) []log.Field {
		if ctx.Value(diagnosticCaptureKey{}) == capture {
			encoder := zapcore.NewMapObjectEncoder()
			for _, field := range fields {
				field.AddTo(encoder)
			}
			encoded, _ := json.Marshal(encoder.Fields)
			capture.mu.Lock()
			fmt.Fprintf(&capture.text, "%s %s\n", message, encoded)
			capture.mu.Unlock()
		}
		return fields
	}))
	return ctx, func() string {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		return capture.text.String()
	}
}

func TestSensitiveErrorDiagnosticClassification(t *testing.T) {
	const sentinel = "synthetic-error-body"
	ctx := shared.WithNativeResponsesWebSocket(t.Context())
	for _, test := range []struct {
		err  error
		want string
	}{
		{&llm.ResponseError{StatusCode: 503, Detail: llm.ErrorDetail{Code: sentinel, Type: sentinel, Message: sentinel}}, "upstream_response_error (status 503)"},
		{&httpclient.Error{StatusCode: 502, URL: sentinel, Status: sentinel, Body: []byte(sentinel)}, "upstream_http_error (status 502)"},
		{fmt.Errorf("%s: %w", sentinel, context.Canceled), "context_canceled"},
		{fmt.Errorf("%s: %w", sentinel, context.DeadlineExceeded), "context_deadline_exceeded"},
		{errors.New(sentinel), "error"},
	} {
		require.Equal(t, test.want, diagnosticErrorString(ctx, test.err))
		require.Equal(t, test.err.Error(), diagnosticErrorString(t.Context(), test.err))
	}
	require.Empty(t, diagnosticErrorString(ctx, nil))
}

func TestSensitiveRequestExecutionErrorDiagnosticsAndHTTPControl(t *testing.T) {
	const sentinel = "synthetic-provider-error-payload"
	for _, sensitive := range []bool{true, false} {
		for _, providerErr := range []error{
			&llm.ResponseError{StatusCode: 503, Detail: llm.ErrorDetail{Message: sentinel, Code: sentinel, Type: sentinel}},
			&httpclient.Error{StatusCode: 502, Status: sentinel, Body: []byte(`{"error":{"message":"` + sentinel + `"}}`)},
		} {
			t.Run(fmt.Sprintf("sensitive=%t/%T", sensitive, providerErr), func(t *testing.T) {
				ctx, client, _, middleware, execution := setupRequestExecutionTerminalMiddleware(t)
				defer client.Close()
				if sensitive {
					ctx = shared.WithNativeResponsesWebSocket(ctx)
				}
				ctx, captured := captureErrorDiagnostics(ctx)
				middleware.OnOutboundRawError(ctx, fmt.Errorf("provider failed: %w", providerErr))
				require.Contains(t, captured(), "request process failed")
				if sensitive {
					require.NotContains(t, captured(), sentinel)
					require.Contains(t, captured(), "upstream_")
				} else {
					require.Contains(t, captured(), sentinel)
				}
				stored := client.RequestExecution.GetX(ctx, execution.ID)
				require.Equal(t, requestexecution.StatusFailed, stored.Status)
				require.Contains(t, stored.ErrorMessage, sentinel, "diagnostic suppression must not change business error persistence")
			})
		}
	}
}

type diagnosticPanicTransformer struct{ mockTransformer }

func (*diagnosticPanicTransformer) AggregateStreamChunks(context.Context, *httpclient.Request, []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	panic("synthetic-aggregation-panic-payload")
}

func TestSensitiveOutboundFinalizationDiagnosticsAndHTTPControl(t *testing.T) {
	for _, sensitive := range []bool{true, false} {
		t.Run(fmt.Sprintf("sensitive=%t", sensitive), func(t *testing.T) {
			ctx, client, requests, middleware, execution := setupRequestExecutionTerminalMiddleware(t)
			defer client.Close()
			if sensitive {
				ctx = shared.WithNativeResponsesWebSocket(ctx)
			}
			ctx, captured := captureErrorDiagnostics(ctx)
			stream := &OutboundPersistentStream{
				transformer: &mockTransformer{aggregatedErr: errors.New("synthetic-aggregate-payload")},
				requestExec: execution, RequestService: requests, state: middleware.outbound.state,
			}
			stream.logFinalizationDecision(ctx, "explicit_stream_error",
				&llm.ResponseError{Detail: llm.ErrorDetail{Message: "synthetic-stream-payload"}},
				errors.New("synthetic-cancel-cause-payload"), false, errors.New("synthetic-aggregate-payload"))
			stream.persistResponseChunks(ctx)
			stream.transformer = &diagnosticPanicTransformer{}
			require.NotPanics(t, func() { stream.persistResponseChunks(ctx) })
			text := captured()
			for _, sentinel := range []string{"synthetic-stream-payload", "synthetic-cancel-cause-payload", "synthetic-aggregate-payload", "synthetic-aggregation-panic-payload"} {
				if sensitive {
					require.NotContains(t, text, sentinel)
				} else {
					require.Contains(t, text, sentinel)
				}
			}
			require.Contains(t, text, "Outbound stream finalization decision")
			require.Contains(t, text, "Failed to aggregate chunks using transformer")
			require.Contains(t, text, "Failed to persist outbound response chunks")
			require.Contains(t, text, "Stream completed but response aggregation evidence is incomplete")
			require.Equal(t, requestexecution.StatusCompleted, client.RequestExecution.GetX(ctx, execution.ID).Status)
		})
	}
}

type diagnosticPanicStream struct{ errorStream }

func (*diagnosticPanicStream) Next() bool { panic("synthetic-drain-panic-payload") }

func TestSensitivePassThroughDrainDiagnosticsAndHTTPControl(t *testing.T) {
	for _, sensitive := range []bool{true, false} {
		for _, panicStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("sensitive=%t/panic=%t", sensitive, panicStream), func(t *testing.T) {
				ctx := t.Context()
				if sensitive {
					ctx = shared.WithNativeResponsesWebSocket(ctx)
				}
				ctx, captured := captureErrorDiagnostics(ctx)
				raw := make(chan *httpclient.StreamEvent)
				close(raw)
				attempt := newPassThroughAttempt(nil)
				attempt.producerDone <- nil
				close(attempt.producerDone)
				state := &PersistenceState{
					CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "offline", Settings: &objects.ChannelSettings{PassThroughBody: lo.ToPtr(true)}}}},
					RawStreamCh:      raw, RawStreamAttempt: attempt, OriginalRequestStream: lo.ToPtr(true),
					LlmRequest:         &llm.Request{APIFormat: llm.APIFormatOpenAIResponse, Stream: lo.ToPtr(true)},
					RawProviderRequest: &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse)},
				}
				var source streams.Stream[*httpclient.StreamEvent] = &errorStream{err: errors.New("synthetic-drain-error-payload")}
				if panicStream {
					source = &diagnosticPanicStream{}
				}
				result, err := applyPassThroughStream(&PersistentOutboundTransformer{state: state}, nil).OnInboundRawStream(ctx, source)
				require.NoError(t, err)
				if sensitive {
					err = result.Close()
				} else {
					err = attempt.stopAndWait()
					require.NoError(t, result.Close())
				}
				require.Error(t, err)
				require.Contains(t, err.Error(), "synthetic-drain-", "error propagation is unchanged")
				text := captured()
				require.Contains(t, text, "pass_through_drain_end")
				if sensitive {
					require.NotContains(t, text, "synthetic-drain-")
				} else {
					require.Contains(t, text, "synthetic-drain-")
				}
			})
		}
	}
}
