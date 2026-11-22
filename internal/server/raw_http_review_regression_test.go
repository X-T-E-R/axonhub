package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
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
	// Revoking the remaining credentials cannot use a cached provider and must
	// surface 503 to authorized callers rather than 403 auth failure.
	f.db.Channel.UpdateOneID(id).SetCredentials(objects.ChannelCredentials{}).SaveX(f.ctx)
	status, _, _ := f.post(t, fmt.Sprintf("/raw/%d/search", id), "opaque", f.key.Key, nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	status, _, _ = f.post(t, fmt.Sprintf("/raw/%d/search", id), "opaque", f.blockedKey.Key, nil)
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

func TestRawHTTPUpstream403PreservesKeysWhile401RetriesAndAutoDisables(t *testing.T) {
	f := newNativeDecisionFixture(t)
	require.NoError(t, f.system.SetRetryPolicy(f.ctx, &biz.RetryPolicy{
		Enabled: true,
		AutoDisableChannel: biz.AutoDisableChannel{
			Enabled: true,
			Statuses: []biz.AutoDisableChannelStatus{
				{Status: http.StatusUnauthorized, Times: 1},
				{Status: http.StatusPaymentRequired, Times: 1},
				{Status: http.StatusForbidden, Times: 1},
			},
		},
	}))

	var calls atomic.Int32
	var seenMu sync.Mutex
	var seenKeys []string
	var mode atomic.Int32 // 0: always 403, 1: first attempt 401 then 200, 2: always 401
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		authHeader := r.Header.Get("X-Api-Key")
		seenMu.Lock()
		seenKeys = append(seenKeys, authHeader)
		seenMu.Unlock()
		switch mode.Load() {
		case 0:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Upstream-Block", "robots-forbidden")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"blocked target url"}`))
		case 1:
			if call == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"revoked key"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"all revoked"}`))
		}
	}))
	defer upstream.Close()

	id := createRawChannel(t, f, upstream.URL, "exa-raw-regression", "header", "x-api-key")
	row := f.db.Channel.GetX(f.ctx, id)
	settings := *row.Settings
	settings.KeySelection = &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin}
	f.db.Channel.UpdateOneID(id).
		SetSettings(&settings).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"exa-key-1", "exa-key-2", "exa-key-3"}}).
		SaveX(f.ctx)

	path := fmt.Sprintf("/raw/%d/contents", id)

	// 1. Target-level 403: single upstream call, exact 403/body/headers returned,
	// 0 keys disabled, channel stays enabled, audit row records failed status.
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(`{"urls":["https://blocked.example"]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.key.Key)
	res, err := f.server.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Equal(t, "robots-forbidden", res.Header.Get("X-Upstream-Block"))
	require.Equal(t, `{"error":"blocked target url"}`, string(body))
	require.EqualValues(t, 1, calls.Load())

	after403 := f.db.Channel.GetX(f.ctx, id)
	require.Equal(t, channel.StatusEnabled, after403.Status)
	require.Empty(t, after403.DisabledAPIKeys)

	require.Eventually(t, func() bool {
		return f.db.Request.Query().Where(
			request.FormatEQ("http/raw"),
			request.ChannelIDEQ(id),
			request.StatusEQ(request.StatusFailed),
		).CountX(f.ctx) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// 2. Credential 401 on the first attempted key: retries to another key,
	// succeeds with 200, and disables only that failing key per policy while
	// leaving the remaining keys and channel enabled.
	mode.Store(1)
	seenMu.Lock()
	seenKeys = nil
	seenMu.Unlock()
	calls.Store(0)

	status, _, respBody := f.post(t, path, `{"urls":["https://ok.example"]}`, f.key.Key, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, `{"ok":true}`, string(respBody))
	require.EqualValues(t, 2, calls.Load())
	seenMu.Lock()
	require.Len(t, seenKeys, 2)
	require.NotEqual(t, seenKeys[0], seenKeys[1])
	failedKey := seenKeys[0]
	seenMu.Unlock()

	afterPartial401 := f.db.Channel.GetX(f.ctx, id)
	require.Equal(t, channel.StatusEnabled, afterPartial401.Status)
	require.Len(t, afterPartial401.DisabledAPIKeys, 1)
	require.Equal(t, failedKey, afterPartial401.DisabledAPIKeys[0].Key)

	// 3. Remaining keys return 401: retries across remaining keys, disables them,
	// and subsequent requests from an authorized caller receive 503 (never 403),
	// while an unauthorized caller still receives 403.
	mode.Store(2)
	calls.Store(0)
	status, _, _ = f.post(t, path, `{"urls":["https://fail.example"]}`, f.key.Key, nil)
	require.Equal(t, http.StatusUnauthorized, status)
	require.EqualValues(t, 2, calls.Load())

	afterAll401 := f.db.Channel.GetX(f.ctx, id)
	require.Len(t, afterAll401.DisabledAPIKeys, 3)

	status, _, _ = f.post(t, path, `{"urls":["https://after.example"]}`, f.key.Key, nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	status, _, _ = f.post(t, path, `{"urls":["https://after.example"]}`, f.blockedKey.Key, nil)
	require.Equal(t, http.StatusForbidden, status)
}
