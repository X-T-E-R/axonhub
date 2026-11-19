package metrics

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"

	metric "go.opentelemetry.io/otel/metric"
	sdk "go.opentelemetry.io/otel/sdk/metric"
)

// _Metrics holds all the metrics for the server.
type _Metrics struct {
	// HTTP metrics
	HTTPRequestCount    metric.Int64Counter
	HTTPRequestDuration metric.Float64Histogram

	// GraphQL metrics
	GraphQLRequestCount    metric.Int64Counter
	GraphQLRequestDuration metric.Float64Histogram

	// Chat metrics
	ChatRequestCount    metric.Int64Counter
	ChatRequestDuration metric.Float64Histogram
	ChatTokenCount      metric.Int64Counter
	ChatSuccessCount    metric.Int64Counter
	ChatFailureCount    metric.Int64Counter

	ManagedObservabilityAdmissionSkipped metric.Int64Counter
	ManagedObservabilityEvicted          metric.Int64Counter
	ManagedObservabilityFailures         metric.Int64Counter
	ManagedObservabilityChargedBytes     metric.Int64Gauge
	StreamEvidenceAggregationIncomplete  metric.Int64Counter
	RetentionGCRuns                      metric.Int64Counter
	RetentionGCRecords                   metric.Int64Counter
}

var currentMetrics atomic.Pointer[_Metrics]

// Current returns the currently configured metrics registry safely across goroutines.
func Current() *_Metrics {
	return currentMetrics.Load()
}

// SetupMetrics creates a new ServerMetrics instance.
func SetupMetrics(provider *sdk.MeterProvider, name string) error {
	meter := provider.Meter(name)
	m := &_Metrics{}

	// HTTP metrics
	httpRequestCount, err := meter.Int64Counter(
		"http_request_count",
		metric.WithDescription("Number of HTTP requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create http_request_count counter: %w", err)
	}

	m.HTTPRequestCount = httpRequestCount

	httpRequestDuration, err := meter.Float64Histogram(
		"http_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("seconds"),
	)
	if err != nil {
		return fmt.Errorf("failed to create http_request_duration_seconds histogram: %w", err)
	}

	m.HTTPRequestDuration = httpRequestDuration

	// GraphQL metrics
	graphQLRequestCount, err := meter.Int64Counter(
		"graphql_request_count",
		metric.WithDescription("Number of GraphQL requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create graphql_request_count counter: %w", err)
	}

	m.GraphQLRequestCount = graphQLRequestCount

	graphQLRequestDuration, err := meter.Float64Histogram(
		"graphql_request_duration_seconds",
		metric.WithDescription("GraphQL request duration in seconds"),
		metric.WithUnit("seconds"),
	)
	if err != nil {
		return fmt.Errorf("failed to create graphql_request_duration_seconds histogram: %w", err)
	}

	m.GraphQLRequestDuration = graphQLRequestDuration

	// Chat metrics
	chatRequestCount, err := meter.Int64Counter(
		"chat_request_count",
		metric.WithDescription("Number of chat requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_request_count counter: %w", err)
	}

	m.ChatRequestCount = chatRequestCount

	chatRequestDuration, err := meter.Float64Histogram(
		"chat_request_duration_seconds",
		metric.WithDescription("Chat request duration in seconds"),
		metric.WithUnit("seconds"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_request_duration_seconds histogram: %w", err)
	}

	m.ChatRequestDuration = chatRequestDuration

	chatTokenCount, err := meter.Int64Counter(
		"chat_token_count",
		metric.WithDescription("Number of tokens in chat requests"),
		metric.WithUnit("tokens"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_token_count counter: %w", err)
	}

	m.ChatTokenCount = chatTokenCount

	chatSuccessCount, err := meter.Int64Counter(
		"chat_success_count",
		metric.WithDescription("Number of successful chat requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_success_count counter: %w", err)
	}

	m.ChatSuccessCount = chatSuccessCount

	chatFailureCount, err := meter.Int64Counter(
		"chat_failure_count",
		metric.WithDescription("Number of failed chat requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_failure_count counter: %w", err)
	}

	m.ChatFailureCount = chatFailureCount

	managedAdmissionSkipped, err := meter.Int64Counter(
		"managed_observability_admission_skipped_total",
		metric.WithDescription("Managed observability payload admissions skipped while degraded"),
		metric.WithUnit("payloads"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_admission_skipped_total: %w", err)
	}
	m.ManagedObservabilityAdmissionSkipped = managedAdmissionSkipped
	managedEvicted, err := meter.Int64Counter(
		"managed_observability_evicted_total",
		metric.WithDescription("Managed observability payloads evicted by capacity cleanup"),
		metric.WithUnit("payloads"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_evicted_total: %w", err)
	}
	m.ManagedObservabilityEvicted = managedEvicted
	managedFailures, err := meter.Int64Counter(
		"managed_observability_failures_total",
		metric.WithDescription("Managed observability owner, lock, admission, or cleanup failures"),
		metric.WithUnit("failures"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_failures_total: %w", err)
	}
	m.ManagedObservabilityFailures = managedFailures
	managedChargedBytes, err := meter.Int64Gauge(
		"managed_observability_charged_bytes",
		metric.WithDescription("Latest conservative live-byte charge for managed observability payloads"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_charged_bytes: %w", err)
	}
	m.ManagedObservabilityChargedBytes = managedChargedBytes

	streamAggregationIncomplete, err := meter.Int64Counter(
		"stream_evidence_aggregation_incomplete_total",
		metric.WithDescription("Successful provider streams whose diagnostic response aggregation was incomplete"),
		metric.WithUnit("streams"),
	)
	if err != nil {
		return fmt.Errorf("failed to create stream_evidence_aggregation_incomplete_total: %w", err)
	}
	m.StreamEvidenceAggregationIncomplete = streamAggregationIncomplete

	retentionRuns, err := meter.Int64Counter(
		"retention_gc_runs_total",
		metric.WithDescription("Retention GC runs, including successful zero-delete runs"),
		metric.WithUnit("runs"),
	)
	if err != nil {
		return fmt.Errorf("failed to create retention_gc_runs_total: %w", err)
	}
	m.RetentionGCRuns = retentionRuns
	retentionRecords, err := meter.Int64Counter(
		"retention_gc_records_total",
		metric.WithDescription("Retention GC record accounting by outcome"),
		metric.WithUnit("records"),
	)
	if err != nil {
		return fmt.Errorf("failed to create retention_gc_records_total: %w", err)
	}
	m.RetentionGCRecords = retentionRecords

	currentMetrics.Store(m)
	return nil
}

func RecordStreamEvidenceAggregationIncomplete(ctx context.Context, apiFormat string) {
	m := Current()
	if m == nil || m.StreamEvidenceAggregationIncomplete == nil {
		return
	}
	m.StreamEvidenceAggregationIncomplete.Add(ctx, 1, metric.WithAttributes(attribute.String("api_format", apiFormat)))
}

func RecordRetentionGC(ctx context.Context, outcome string, count int) {
	m := Current()
	if m == nil {
		return
	}
	if m.RetentionGCRecords != nil && count > 0 {
		m.RetentionGCRecords.Add(ctx, int64(count), metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

func RecordRetentionGCRun(ctx context.Context, succeeded bool, deleted int) {
	m := Current()
	if m == nil || m.RetentionGCRuns == nil {
		return
	}
	status := "error"
	if succeeded {
		status = "success"
	}
	deletionOutcome := "nonzero"
	if deleted == 0 {
		deletionOutcome = "zero"
	}
	m.RetentionGCRuns.Add(ctx, 1, metric.WithAttributes(
		attribute.String("status", status),
		attribute.String("deleted", deletionOutcome),
	))
}

func RecordManagedObservabilityAdmissionSkipped(ctx context.Context, reason string) {
	RecordManagedObservabilityAdmissionSkippedComponent(ctx, reason, "request_body")
}

func RecordManagedObservabilityAdmissionSkippedComponent(ctx context.Context, reason, component string) {
	m := Current()
	if m == nil || m.ManagedObservabilityAdmissionSkipped == nil {
		return
	}
	m.ManagedObservabilityAdmissionSkipped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
		attribute.String("component", component),
	))
}

func RecordManagedObservabilityFailure(ctx context.Context, component, reason string) {
	m := Current()
	if m == nil || m.ManagedObservabilityFailures == nil {
		return
	}
	m.ManagedObservabilityFailures.Add(ctx, 1, metric.WithAttributes(
		attribute.String("component", component),
		attribute.String("reason", reason),
	))
}

func RecordManagedObservabilityCapacity(ctx context.Context, chargedBytes int64, evicted int) {
	m := Current()
	if m == nil {
		return
	}
	if m.ManagedObservabilityChargedBytes != nil {
		m.ManagedObservabilityChargedBytes.Record(ctx, chargedBytes)
	}
	if evicted > 0 && m.ManagedObservabilityEvicted != nil {
		m.ManagedObservabilityEvicted.Add(ctx, int64(evicted))
	}
}

// RecordHTTPRequest records HTTP request metrics.
func (sm *_Metrics) RecordHTTPRequest(ctx context.Context, method, path string, statusCode int, duration float64) {
	if sm == nil {
		return
	}
	labels := []attribute.KeyValue{
		attribute.String("method", method),
		attribute.String("path", path),
		attribute.Int("status_code", statusCode),
	}

	sm.HTTPRequestCount.Add(ctx, 1, metric.WithAttributes(labels...))
	sm.HTTPRequestDuration.Record(ctx, duration, metric.WithAttributes(labels...))
}

// RecordGraphQLRequest records GraphQL request metrics.
func (sm *_Metrics) RecordGraphQLRequest(ctx context.Context, operation string, duration float64) {
	labels := []attribute.KeyValue{
		attribute.String("operation", operation),
	}

	sm.GraphQLRequestCount.Add(ctx, 1, metric.WithAttributes(labels...))
	sm.GraphQLRequestDuration.Record(ctx, duration, metric.WithAttributes(labels...))
}

// RecordChatRequest records chat request metrics.
func (sm *_Metrics) RecordChatRequest(ctx context.Context, channelID, channelName string, duration float64, success bool, tokenCount int64) {
	labels := []attribute.KeyValue{
		attribute.String("channel_id", channelID),
		attribute.String("channel_name", channelName),
	}

	sm.ChatRequestCount.Add(ctx, 1, metric.WithAttributes(labels...))
	sm.ChatRequestDuration.Record(ctx, duration, metric.WithAttributes(labels...))
	sm.ChatTokenCount.Add(ctx, tokenCount, metric.WithAttributes(labels...))

	if success {
		sm.ChatSuccessCount.Add(ctx, 1, metric.WithAttributes(labels...))
	} else {
		sm.ChatFailureCount.Add(ctx, 1, metric.WithAttributes(labels...))
	}
}
