package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/stretchr/testify/require"
)

func TestRawHTTPCORSAndOptions(t *testing.T) {
	var corsFixture *nativeDecisionFixture
	require.NotPanics(t, func() {
		corsFixture = newNativeDecisionFixture(t, CORS{Enabled: true, AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET", "POST", "OPTIONS"}, AllowedHeaders: []string{"Authorization", "Content-Type"}})
	})
	for _, path := range []string{"/raw/42/search", "/v1/chat/completions", "/admin/graphql"} {
		req, err := http.NewRequest(http.MethodOptions, corsFixture.server.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "https://client.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type")
		res, err := corsFixture.server.Client().Do(req)
		require.NoError(t, err)
		res.Body.Close()
		require.Equal(t, http.StatusNoContent, res.StatusCode, path)
		require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))
		require.Contains(t, res.Header.Get("Access-Control-Allow-Methods"), "POST")
	}

	// With CORS disabled the same method is a real authenticated raw operation.
	f := newNativeDecisionFixture(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, http.MethodOptions, r.Method)
		require.Equal(t, "/capability?opaque=%2F", r.RequestURI)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "opaque-options", string(body))
		w.Header().Set("Content-Type", "application/custom")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte("provider-options"))
	}))
	defer upstream.Close()
	id := createRawChannel(t, f, upstream.URL, "raw-options", "bearer", "")
	for _, authenticated := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodOptions, fmt.Sprintf("%s/raw/%d/capability?opaque=%%2F", f.server.URL, id), strings.NewReader("opaque-options"))
		require.NoError(t, err)
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+f.key.Key)
		}
		res, err := f.server.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		res.Body.Close()
		if authenticated {
			require.Equal(t, http.StatusMultiStatus, res.StatusCode)
			require.Equal(t, "provider-options", string(body))
			require.Equal(t, "application/custom", res.Header.Get("Content-Type"))
		} else {
			require.Equal(t, http.StatusUnauthorized, res.StatusCode)
		}
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestRawHTTPPersistentKeySelectionAndRevocation(t *testing.T) {
	f := newNativeDecisionFixture(t)
	received := make(chan string, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	id := createRawChannel(t, f, upstream.URL, "raw-key-routing", "bearer", "")
	for _, strategy := range []objects.ChannelKeySelectionStrategy{objects.ChannelKeySelectionStrategyRoundRobin, objects.ChannelKeySelectionStrategyCacheAffinity} {
		row := f.db.Channel.GetX(f.ctx, id)
		settings := *row.Settings
		settings.KeySelection = &objects.ChannelKeySelection{Strategy: strategy}
		f.db.Channel.UpdateOneID(id).SetSettings(&settings).SetDisabledAPIKeys(nil).SetCredentials(objects.ChannelCredentials{APIKeys: []string{"synthetic-first", "synthetic-second"}}).SaveX(f.ctx)
		got := []string{}
		for i := 0; i < 4; i++ {
			status, _, _ := f.post(t, fmt.Sprintf("/raw/%d/search", id), "opaque", f.key.Key, nil)
			require.Equal(t, 200, status)
			got = append(got, <-received)
		}
		require.Equal(t, []string{"Bearer synthetic-first", "Bearer synthetic-second", "Bearer synthetic-first", "Bearer synthetic-second"}, got, strategy)
		f.db.Channel.UpdateOneID(id).SetDisabledAPIKeys([]objects.DisabledAPIKey{{Key: "synthetic-first", DisabledAt: time.Now(), Reason: "revoked"}}).SaveX(f.ctx)
		for i := 0; i < 2; i++ {
			status, _, _ := f.post(t, fmt.Sprintf("/raw/%d/search", id), "opaque", f.key.Key, nil)
			require.Equal(t, 200, status)
			require.Equal(t, "Bearer synthetic-second", <-received, strategy)
		}
	}
	// Revoking the remaining credentials cannot use a cached provider.
	f.db.Channel.UpdateOneID(id).SetCredentials(objects.ChannelCredentials{}).SaveX(f.ctx)
	status, _, _ := f.post(t, fmt.Sprintf("/raw/%d/search", id), "opaque", f.key.Key, nil)
	require.Equal(t, http.StatusForbidden, status)
	require.Empty(t, received)
}

func TestRawHTTPUnsupportedHealthChecksPreserveState(t *testing.T) {
	f := newNativeDecisionFixture(t)
	f.channelService.SetChannelKeyHealthCheckTester(f.tester)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("allowed"))
	}))
	defer upstream.Close()
	id := createRawChannel(t, f, upstream.URL, "raw-health", "bearer", "")
	path := fmt.Sprintf("/raw/%d/search", id)
	status, _, _ := f.post(t, path, "opaque", f.key.Key, nil)
	require.Equal(t, 200, status)
	row := f.db.Channel.GetX(f.ctx, id)
	settings := *row.Settings
	settings.KeyHealthCheck = &objects.ChannelKeyHealthCheck{Enabled: true, FailureThreshold: 1, FailureAction: objects.ChannelKeyHealthCheckFailureActionDisable}
	f.db.Channel.UpdateOneID(id).SetSettings(&settings).SaveX(f.ctx)
	for _, mode := range []objects.ChannelAPIKeyHealthCheckMode{objects.ChannelAPIKeyHealthCheckModeRealRequest, objects.ChannelAPIKeyHealthCheckModeAuto} {
		_, err := f.channelService.RunChannelAPIKeyHealthCheck(f.ctx, id, nil, mode)
		require.ErrorIs(t, err, biz.ErrRawHTTPBuiltinTestUnsupported)
	}
	require.NoError(t, f.channelService.RunDueChannelKeyHealthChecks(f.ctx, time.Now()))
	// Global monitoring rules consume the same unsupported result as skipped.
	require.NoError(t, f.system.SetMonitoringSettings(f.ctx, biz.MonitoringSettings{Enabled: true, Rules: []biz.MonitoringRule{{ID: "raw-builtin", Name: "Raw builtin", Targets: biz.MonitoringRuleTargets{ChannelIDs: []int{id}}, Probes: []objects.ChannelKeyHealthCheckRule{{Type: objects.ChannelKeyHealthCheckRuleTypeBuiltinTest}}}}}))
	require.NoError(t, f.channelService.RunDueChannelKeyHealthChecks(f.ctx, time.Now()))
	after := f.db.Channel.GetX(f.ctx, id)
	require.Equal(t, channel.StatusEnabled, after.Status)
	require.Empty(t, after.DisabledAPIKeys)
	require.Empty(t, after.Settings.KeyHealthCheck.KeyMetadata)
	events := f.db.ChannelKeyMonitoringEvent.Query().AllX(f.ctx)
	require.Len(t, events, 1)
	require.True(t, events[0].Skipped)
	require.False(t, events[0].Success)
	require.Empty(t, events[0].Action)
	inventory, err := f.channelService.ChannelAPIKeyInventory(f.ctx, id)
	require.NoError(t, err)
	require.Nil(t, inventory[0].Success)
	require.Zero(t, inventory[0].FailureCount)
	status, _, _ = f.post(t, path, "opaque", f.key.Key, nil)
	require.Equal(t, 200, status)
	require.EqualValues(t, 2, calls.Load(), "unsupported checks must not probe the provider")
}
