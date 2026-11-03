package biz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

func TestRawHTTPExplicitHealthProbesRemainUsable(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	var calls, responseStatus atomic.Int32
	responseStatus.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/balance", r.URL.Path)
		require.Equal(t, "Bearer synthetic-health-key", r.Header.Get("Authorization"))
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(responseStatus.Load()))
		_, _ = w.Write([]byte(`{"balance":12,"currency":"USD","available":true}`))
	}))
	defer upstream.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(upstream.Client())
	rule := objects.ChannelKeyHealthCheckHTTPRule{
		Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
		URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
		Path:             "/balance",
		ExpectedStatuses: []int{http.StatusOK},
	}
	probes := []objects.ChannelKeyHealthCheckRule{
		{ID: "unsupported-builtin", Type: objects.ChannelKeyHealthCheckRuleTypeBuiltinTest},
		{ID: "explicit-http", Type: objects.ChannelKeyHealthCheckRuleTypeHTTP, HTTP: &rule},
	}
	ch := client.Channel.Create().SetType(channel.TypeOpenai).SetName("Raw explicit health").SetStatus(channel.StatusEnabled).
		SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKeys: []string{"synthetic-health-key"}}).
		SetSupportedModels([]string{"raw-health"}).SetDefaultTestModel("raw-health").
		SetSettings(&objects.ChannelSettings{
			RawHTTP:        &objects.ChannelRawHTTP{Enabled: true, Resource: "raw-health", AuthType: "bearer"},
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{Enabled: true, IntervalMinutes: 60, FailureThreshold: 1, FailureAction: objects.ChannelKeyHealthCheckFailureActionDisable, Rules: probes},
		}).SaveX(ctx)
	items, err := svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, nil)
	require.NoError(t, err)
	require.True(t, *items[0].Success)
	require.EqualValues(t, 1, calls.Load(), "only the configured HTTP rule runs")
	_, err = svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, nil, objects.ChannelAPIKeyHealthCheckModeRealRequest)
	require.ErrorIs(t, err, ErrRawHTTPBuiltinTestUnsupported)
	require.EqualValues(t, 1, calls.Load())
	now := time.Now().Add(2 * time.Hour)
	require.NoError(t, svc.RunDueChannelKeyHealthChecks(ctx, now))
	require.EqualValues(t, 2, calls.Load(), "legacy scheduled HTTP rules remain enabled")

	updated := client.Channel.GetX(ctx, ch.ID)
	settings := cloneChannelSettings(updated.Settings)
	settings.BalanceProbe = &objects.ChannelBalanceProbe{Preset: objects.ChannelBalanceProbePresetCustom, HTTP: &rule}
	client.Channel.UpdateOneID(ch.ID).SetSettings(settings).SaveX(ctx)
	items, err = svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, nil, objects.ChannelAPIKeyHealthCheckModeBalanceProbe)
	require.NoError(t, err)
	require.True(t, *items[0].Success)
	require.EqualValues(t, 3, calls.Load(), "explicit custom balance probe remains enabled")

	// The global monitoring consumer must skip builtin but still consume the
	// supported HTTP result and its real success/failure policy semantics.
	require.NoError(t, svc.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{Enabled: true, Rules: []MonitoringRule{{
		ID: "raw-http-rule", Name: "Explicit raw HTTP", Targets: MonitoringRuleTargets{ChannelIDs: []int{ch.ID}}, Probes: probes,
	}}}))
	// Only exercise this monitoring rule here; disable the independent legacy schedule.
	settings.KeyHealthCheck.Enabled = false
	client.Channel.UpdateOneID(ch.ID).SetSettings(settings).SaveX(ctx)
	require.NoError(t, svc.RunDueChannelKeyHealthChecks(ctx, now.Add(time.Minute)))
	require.EqualValues(t, 4, calls.Load())
	after := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusEnabled, after.Status)
	require.Empty(t, after.DisabledAPIKeys)
	require.Zero(t, after.Settings.KeyHealthCheck.KeyMetadata[0].FailureCount)

	// A real failure from the explicitly configured HTTP check is still failure
	// evidence: the unsupported builtin must not suppress it or its disable policy.
	responseStatus.Store(http.StatusUnauthorized)
	settings = cloneChannelSettings(after.Settings)
	settings.BalanceProbe = nil
	client.Channel.UpdateOneID(ch.ID).SetSettings(settings).SaveX(ctx)
	items, err = svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, nil)
	require.NoError(t, err)
	require.False(t, *items[0].Success)
	require.Equal(t, 1, items[0].FailureCount)
	require.EqualValues(t, 5, calls.Load())
	after = client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusDisabled, after.Status)
	require.Len(t, after.DisabledAPIKeys, 1)
}
