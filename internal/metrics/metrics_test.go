package metrics

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRecordRetentionGCRunMakesSuccessfulZeroDeleteVisible(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	previous := Metrics
	t.Cleanup(func() { Metrics = previous })
	require.NoError(t, SetupMetrics(provider, "retention-gc-test"))

	RecordRetentionGCRun(t.Context(), true, 0)

	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &collected))
	runs := findInt64Counter(t, collected, "retention_gc_runs_total")
	require.Len(t, runs.DataPoints, 1)
	require.Equal(t, int64(1), runs.DataPoints[0].Value)
	status, ok := runs.DataPoints[0].Attributes.Value(attribute.Key("status"))
	require.True(t, ok)
	require.Equal(t, "success", status.AsString())
	deleted, ok := runs.DataPoints[0].Attributes.Value(attribute.Key("deleted"))
	require.True(t, ok)
	require.Equal(t, "zero", deleted.AsString())
}

func findInt64Counter(t *testing.T, collected metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	t.Helper()
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name == name {
				counter, ok := instrument.Data.(metricdata.Sum[int64])
				require.True(t, ok, "metric %q is not an int64 counter", name)
				return counter
			}
		}
	}
	t.Fatalf("counter %q not present in collected metrics", name)
	return metricdata.Sum[int64]{}
}
