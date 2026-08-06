package metrics

import (
	"context"
	"fmt"

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
}

var Metrics *_Metrics

// SetupMetrics creates a new ServerMetrics instance.
func SetupMetrics(provider *sdk.MeterProvider, name string) error {
	meter := provider.Meter(name)
	Metrics = &_Metrics{}

	// HTTP metrics
	httpRequestCount, err := meter.Int64Counter(
		"http_request_count",
		metric.WithDescription("Number of HTTP requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create http_request_count counter: %w", err)
	}

	Metrics.HTTPRequestCount = httpRequestCount

	httpRequestDuration, err := meter.Float64Histogram(
		"http_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("seconds"),
	)
	if err != nil {
		return fmt.Errorf("failed to create http_request_duration_seconds histogram: %w", err)
	}

	Metrics.HTTPRequestDuration = httpRequestDuration

	// GraphQL metrics
	graphQLRequestCount, err := meter.Int64Counter(
		"graphql_request_count",
		metric.WithDescription("Number of GraphQL requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create graphql_request_count counter: %w", err)
	}

	Metrics.GraphQLRequestCount = graphQLRequestCount

	graphQLRequestDuration, err := meter.Float64Histogram(
		"graphql_request_duration_seconds",
		metric.WithDescription("GraphQL request duration in seconds"),
		metric.WithUnit("seconds"),
	)
	if err != nil {
		return fmt.Errorf("failed to create graphql_request_duration_seconds histogram: %w", err)
	}

	Metrics.GraphQLRequestDuration = graphQLRequestDuration

	// Chat metrics
	chatRequestCount, err := meter.Int64Counter(
		"chat_request_count",
		metric.WithDescription("Number of chat requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_request_count counter: %w", err)
	}

	Metrics.ChatRequestCount = chatRequestCount

	chatRequestDuration, err := meter.Float64Histogram(
		"chat_request_duration_seconds",
		metric.WithDescription("Chat request duration in seconds"),
		metric.WithUnit("seconds"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_request_duration_seconds histogram: %w", err)
	}

	Metrics.ChatRequestDuration = chatRequestDuration

	chatTokenCount, err := meter.Int64Counter(
		"chat_token_count",
		metric.WithDescription("Number of tokens in chat requests"),
		metric.WithUnit("tokens"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_token_count counter: %w", err)
	}

	Metrics.ChatTokenCount = chatTokenCount

	chatSuccessCount, err := meter.Int64Counter(
		"chat_success_count",
		metric.WithDescription("Number of successful chat requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_success_count counter: %w", err)
	}

	Metrics.ChatSuccessCount = chatSuccessCount

	chatFailureCount, err := meter.Int64Counter(
		"chat_failure_count",
		metric.WithDescription("Number of failed chat requests"),
		metric.WithUnit("requests"),
	)
	if err != nil {
		return fmt.Errorf("failed to create chat_failure_count counter: %w", err)
	}

	Metrics.ChatFailureCount = chatFailureCount

	managedAdmissionSkipped, err := meter.Int64Counter(
		"managed_observability_admission_skipped_total",
		metric.WithDescription("Managed observability payload admissions skipped while degraded"),
		metric.WithUnit("payloads"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_admission_skipped_total: %w", err)
	}
	Metrics.ManagedObservabilityAdmissionSkipped = managedAdmissionSkipped
	managedEvicted, err := meter.Int64Counter(
		"managed_observability_evicted_total",
		metric.WithDescription("Managed observability payloads evicted by capacity cleanup"),
		metric.WithUnit("payloads"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_evicted_total: %w", err)
	}
	Metrics.ManagedObservabilityEvicted = managedEvicted
	managedFailures, err := meter.Int64Counter(
		"managed_observability_failures_total",
		metric.WithDescription("Managed observability owner, lock, admission, or cleanup failures"),
		metric.WithUnit("failures"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_failures_total: %w", err)
	}
	Metrics.ManagedObservabilityFailures = managedFailures
	managedChargedBytes, err := meter.Int64Gauge(
		"managed_observability_charged_bytes",
		metric.WithDescription("Latest conservative live-byte charge for managed observability payloads"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return fmt.Errorf("failed to create managed_observability_charged_bytes: %w", err)
	}
	Metrics.ManagedObservabilityChargedBytes = managedChargedBytes

	return nil
}

func RecordManagedObservabilityAdmissionSkipped(ctx context.Context, reason string) {
	RecordManagedObservabilityAdmissionSkippedComponent(ctx, reason, "request_body")
}

func RecordManagedObservabilityAdmissionSkippedComponent(ctx context.Context, reason, component string) {
	if Metrics == nil || Metrics.ManagedObservabilityAdmissionSkipped == nil {
		return
	}
	Metrics.ManagedObservabilityAdmissionSkipped.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
		attribute.String("component", component),
	))
}

func RecordManagedObservabilityFailure(ctx context.Context, component, reason string) {
	if Metrics == nil || Metrics.ManagedObservabilityFailures == nil {
		return
	}
	Metrics.ManagedObservabilityFailures.Add(ctx, 1, metric.WithAttributes(
		attribute.String("component", component),
		attribute.String("reason", reason),
	))
}

func RecordManagedObservabilityCapacity(ctx context.Context, chargedBytes int64, evicted int) {
	if Metrics == nil {
		return
	}
	if Metrics.ManagedObservabilityChargedBytes != nil {
		Metrics.ManagedObservabilityChargedBytes.Record(ctx, chargedBytes)
	}
	if evicted > 0 && Metrics.ManagedObservabilityEvicted != nil {
		Metrics.ManagedObservabilityEvicted.Add(ctx, int64(evicted))
	}
}

// RecordHTTPRequest records HTTP request metrics.
func (sm *_Metrics) RecordHTTPRequest(ctx context.Context, method, path string, statusCode int, duration float64) {
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
