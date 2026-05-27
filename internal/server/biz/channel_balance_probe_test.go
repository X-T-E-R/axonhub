package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestBalanceProbeDeepSeekParserKeepsComponentsAndSelectsPrimary(t *testing.T) {
	snapshot := parseDeepSeekBalanceSnapshot([]byte(`{
		"is_available": true,
		"balance_infos": [
			{"currency":"CNY","total_balance":"10.00","granted_balance":"4.00","topped_up_balance":"6.00"},
			{"currency":"USD","total_balance":"20.00","granted_balance":"1.00","topped_up_balance":"19.00"}
		]
	}`), http.StatusOK)

	selectChannelBalancePrimary(&snapshot, "")
	require.True(t, snapshot.Success)
	require.NotNil(t, snapshot.Available)
	require.True(t, *snapshot.Available)
	require.Len(t, snapshot.Components, 6)
	require.NotNil(t, snapshot.PrimaryBalance)
	require.Equal(t, 20.0, snapshot.PrimaryBalance.Amount)
	require.Equal(t, "USD", snapshot.PrimaryBalance.Currency)

	selectChannelBalancePrimary(&snapshot, "CNY")
	require.Equal(t, 10.0, snapshot.PrimaryBalance.Amount)
	require.Equal(t, "CNY", snapshot.PrimaryBalance.Currency)
}

func TestBalanceProbeProviderParsersNormalizeKnownShapes(t *testing.T) {
	tests := []struct {
		name     string
		parse    func([]byte, int) objects.ChannelKeyBalanceSnapshot
		body     string
		currency string
		amount   float64
	}{
		{
			name:     "siliconflow",
			parse:    parseSiliconFlowBalanceSnapshot,
			body:     `{"code":20000,"status":true,"data":{"id":"user_1","balance":"0.88","status":"normal","chargeBalance":"88.00","totalBalance":"88.88"}}`,
			currency: "CNY",
			amount:   88.88,
		},
		{
			name:     "moonshot",
			parse:    parseMoonshotBalanceSnapshot,
			body:     `{"available_balance":"12.50","voucher_balance":"2.00","cash_balance":"10.50","currency":"CNY"}`,
			currency: "CNY",
			amount:   12.50,
		},
		{
			name:     "openrouter",
			parse:    parseOpenRouterBalanceSnapshot,
			body:     `{"data":{"total_credits":100.5,"total_usage":25.75}}`,
			currency: "USD",
			amount:   100.5,
		},
		{
			name:     "nanogpt",
			parse:    parseNanoGPTBalanceSnapshot,
			body:     `{"usd_balance":"129.46956147","nano_balance":"26.71801147","nanoDepositAddress":"nano_secret"}`,
			currency: "USD",
			amount:   129.46956147,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := tt.parse([]byte(tt.body), http.StatusOK)
			selectChannelBalancePrimary(&snapshot, "")
			require.True(t, snapshot.Success)
			require.NotNil(t, snapshot.PrimaryBalance)
			require.Equal(t, tt.currency, snapshot.PrimaryBalance.Currency)
			require.InDelta(t, tt.amount, snapshot.PrimaryBalance.Amount, 0.00000001)
			require.NotEmpty(t, snapshot.Components)
		})
	}
}

func TestBalanceProbeCustomPresetUsesHTTPOverrideAndGenericParser(t *testing.T) {
	spec, probe, ok := channelBalanceProbeSpecForChannel(&ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.example.com/v1",
		Settings: &objects.ChannelSettings{BalanceProbe: &objects.ChannelBalanceProbe{
			Preset: objects.ChannelBalanceProbePresetCustom,
			HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
				Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
				URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
				Path:             "/billing/balance",
				ExpectedStatuses: []int{http.StatusOK},
			},
		}},
	})
	require.True(t, ok)
	require.NotNil(t, probe)
	require.Equal(t, objects.ChannelBalanceProbePresetCustom, spec.ID)
	require.Equal(t, "/billing/balance", spec.HTTP.Path)

	snapshot := spec.Parse([]byte(`{"data":{"balance":"12.75","currency":"USD","available":true}}`), http.StatusOK)
	selectChannelBalancePrimary(&snapshot, "")
	require.True(t, snapshot.Success)
	require.NotNil(t, snapshot.Available)
	require.True(t, *snapshot.Available)
	require.NotNil(t, snapshot.PrimaryBalance)
	require.Equal(t, 12.75, snapshot.PrimaryBalance.Amount)
	require.Equal(t, "USD", snapshot.PrimaryBalance.Currency)
	require.NoError(t, ValidateChannelBalanceProbe(probe))
}

func TestBalanceProbePresetURLStrategiesUseProviderBaseWhenEndpointIsUnderBasePath(t *testing.T) {
	tests := []struct {
		name     string
		preset   objects.ChannelBalanceProbePreset
		baseURL  string
		expected string
	}{
		{
			name:     "moonshot ai regional base",
			preset:   objects.ChannelBalanceProbePresetMoonshot,
			baseURL:  "https://api.moonshot.ai/v1",
			expected: "https://api.moonshot.ai/v1/users/me/balance",
		},
		{
			name:     "moonshot cn regional base",
			preset:   objects.ChannelBalanceProbePresetMoonshot,
			baseURL:  "https://api.moonshot.cn/v1",
			expected: "https://api.moonshot.cn/v1/users/me/balance",
		},
		{
			name:     "siliconflow standard v1 base",
			preset:   objects.ChannelBalanceProbePresetSiliconFlow,
			baseURL:  "https://api.siliconflow.cn/v1",
			expected: "https://api.siliconflow.cn/v1/user/info",
		},
		{
			name:     "openrouter standard api v1 base",
			preset:   objects.ChannelBalanceProbePresetOpenRouter,
			baseURL:  "https://openrouter.ai/api/v1",
			expected: "https://openrouter.ai/api/v1/credits",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := channelBalanceProbePresetByID(tt.preset)
			require.True(t, ok)
			require.Equal(t, objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL, spec.HTTP.URLMode)

			got, err := buildChannelKeyHealthCheckHTTPURL(context.Background(), tt.baseURL, spec.HTTP)
			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestBalanceProbePresetsKeepRootPathEndpointsAbsolute(t *testing.T) {
	tests := []struct {
		name     string
		preset   objects.ChannelBalanceProbePreset
		expected string
	}{
		{
			name:     "deepseek balance is outside normal v1 base",
			preset:   objects.ChannelBalanceProbePresetDeepSeek,
			expected: "https://api.deepseek.com/user/balance",
		},
		{
			name:     "nanogpt check balance is outside normal v1 base",
			preset:   objects.ChannelBalanceProbePresetNanoGPT,
			expected: "https://nano-gpt.com/api/check-balance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := channelBalanceProbePresetByID(tt.preset)
			require.True(t, ok)
			require.Equal(t, objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL, spec.HTTP.URLMode)
			require.Equal(t, tt.expected, spec.HTTP.URL)
		})
	}
}

func TestBalanceProbeAbsoluteURLAllowsProviderOwnedExternalBase(t *testing.T) {
	rule := objects.ChannelKeyHealthCheckHTTPRule{
		URLMode: objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
		URL:     "https://93.184.216.34/custom/balance",
	}

	got, err := buildBalanceProbeHTTPURL(context.Background(), "https://api.example.com/v1", rule)
	require.NoError(t, err)
	require.Equal(t, "https://93.184.216.34/custom/balance", got)
}

func TestRunChannelAPIKeyHealthCheckPrefersBalanceProbeAndStoresSnapshot(t *testing.T) {
	disableChannelKeyHealthCheckDelays(t)

	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"42.00"}]}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer client.Close()

	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())
	builtinCalled := false
	svc.SetChannelKeyHealthCheckTester(channelKeyHealthCheckTesterFunc(func(ctx context.Context, channelID objects.GUID, key string, modelID *string, proxy *httpclient.ProxyConfig) ChannelKeyHealthCheckBuiltinResult {
		builtinCalled = true
		return ChannelKeyHealthCheckBuiltinResult{Success: true, Reason: "builtin"}
	}))

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeDeepseek).
		SetName("Balance Probe Manual").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-balance-manual"}}).
		SetSupportedModels([]string{"deepseek-chat"}).
		SetDefaultTestModel("deepseek-chat").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{},
			BalanceProbe: &objects.ChannelBalanceProbe{
				Preset: objects.ChannelBalanceProbePresetDeepSeek,
				HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
					Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
					URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
					Path:             "/user/balance",
					ExpectedStatuses: []int{http.StatusOK},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	items, err := svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, nil)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.False(t, builtinCalled)
	require.Equal(t, "Bearer sk-balance-manual", gotAuthorization)
	require.NotNil(t, items[0].BalanceSnapshot)
	require.NotNil(t, items[0].BalanceSnapshot.PrimaryBalance)
	require.Equal(t, 42.0, items[0].BalanceSnapshot.PrimaryBalance.Amount)
	require.Equal(t, "USD", items[0].BalanceSnapshot.PrimaryBalance.Currency)
	require.Equal(t, 42.0, items[0].Balance)
	require.Equal(t, "USD", items[0].Currency)
}

func TestRunChannelAPIKeyHealthCheckRealRequestModeBypassesBalanceProbe(t *testing.T) {
	disableChannelKeyHealthCheckDelays(t)

	balanceProbeCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		balanceProbeCalled = true
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"42.00"}]}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer client.Close()

	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())
	builtinCalled := false
	svc.SetChannelKeyHealthCheckTester(channelKeyHealthCheckTesterFunc(func(ctx context.Context, channelID objects.GUID, key string, modelID *string, proxy *httpclient.ProxyConfig) ChannelKeyHealthCheckBuiltinResult {
		builtinCalled = true
		require.Equal(t, "sk-real-request-manual", key)
		return ChannelKeyHealthCheckBuiltinResult{Success: true, Reason: "builtin ok"}
	}))

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeDeepseek).
		SetName("Real Request Manual").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-real-request-manual"}}).
		SetSupportedModels([]string{"deepseek-chat"}).
		SetDefaultTestModel("deepseek-chat").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{},
			BalanceProbe: &objects.ChannelBalanceProbe{
				Preset: objects.ChannelBalanceProbePresetDeepSeek,
				HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
					Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
					URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
					Path:             "/user/balance",
					ExpectedStatuses: []int{http.StatusOK},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	items, err := svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, nil, objects.ChannelAPIKeyHealthCheckModeRealRequest)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, builtinCalled)
	require.False(t, balanceProbeCalled)
	require.NotNil(t, items[0].Success)
	require.True(t, *items[0].Success)
	require.Equal(t, "builtin ok", items[0].Reason)
	require.Nil(t, items[0].BalanceSnapshot)
}

func TestSelectedChannelKeyHealthCheckTargetsIncludeSelectedDisabledAndArchivedKeys(t *testing.T) {
	activeKey := "sk-active"
	disabledKey := "sk-disabled"
	archivedKey := "sk-archived"
	ch := &ent.Channel{
		Credentials: objects.ChannelCredentials{APIKeys: []string{activeKey, disabledKey, archivedKey}},
		DisabledAPIKeys: []objects.DisabledAPIKey{{
			Key: disabledKey,
		}},
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				ArchivedKeys: []objects.ChannelArchivedAPIKey{{
					ID: objects.ChannelAPIKeyFingerprint(archivedKey),
				}},
			},
		},
	}

	require.Equal(t, []string{activeKey}, selectedChannelKeyHealthCheckTargets(ch, nil))
	require.ElementsMatch(
		t,
		[]string{disabledKey, archivedKey},
		selectedChannelKeyHealthCheckTargets(ch, []string{
			objects.ChannelAPIKeyFingerprint(disabledKey),
			objects.ChannelAPIKeyFingerprint(archivedKey),
		}),
	)
}

func TestUpdateChannelAllowsWriteOnlyAdminToSaveCustomBalanceProbe(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Custom Balance Save").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-custom-save"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{KeyHealthCheck: &objects.ChannelKeyHealthCheck{}}).
		Save(setupCtx)
	require.NoError(t, err)

	writeOnlyUser := &ent.User{
		ID:     45678,
		Scopes: []string{string(scopes.ScopeWriteChannels)},
	}
	writeCtx := ent.NewContext(context.Background(), client)
	writeCtx = contexts.WithUser(writeCtx, writeOnlyUser)
	writeCtx = authz.NewUserContext(writeCtx, writeOnlyUser.ID)

	updated, err := svc.UpdateChannel(writeCtx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: true,
				Preset:  objects.ChannelBalanceProbePresetCustom,
				HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
					Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
					URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
					Path:             "/balance",
					ExpectedStatuses: []int{http.StatusOK},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updated.Settings)
	require.NotNil(t, updated.Settings.BalanceProbe)
	require.True(t, updated.Settings.BalanceProbe.Enabled)
	require.Equal(t, objects.ChannelBalanceProbePresetCustom, updated.Settings.BalanceProbe.Preset)
	require.Equal(t, "/balance", updated.Settings.BalanceProbe.HTTP.Path)
}

func TestFailurePolicyBalanceProbeSourcesAndThresholds(t *testing.T) {
	matches := evaluateFailurePolicyProfiles([]objects.FailurePolicyProfile{{
		ID:      "restore-funded-key",
		Name:    "Restore funded key",
		Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceManualBalanceProbe},
		Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
			Success:    lo.ToPtr(true),
			BalanceGTE: lo.ToPtr(10.0),
		},
		Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionRestoreKey}},
	}}, failurePolicyEvent{
		Source:       objects.FailurePolicyEventSourceManualBalanceProbe,
		Target:       objects.FailurePolicyTargetKey,
		Success:      true,
		Balance:      12.5,
		Currency:     "USD",
		FailureCount: 0,
		CheckedAt:    time.Now(),
	})

	require.Len(t, matches, 1)
	require.Equal(t, objects.FailurePolicyActionRestoreKey, matches[0].Actions[0].Type)
}
