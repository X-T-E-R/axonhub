package biz

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func TestOfficialBalanceProbePresetSpecs(t *testing.T) {
	tests := []struct {
		name       string
		preset     objects.ChannelBalanceProbePreset
		url        string
		keyHeader  string
		channel    *ent.Channel
		wantAutoID objects.ChannelBalanceProbePreset
	}{
		{
			name:   "tavily",
			preset: objects.ChannelBalanceProbePresetTavily,
			url:    "https://api.tavily.com/usage",
			channel: &ent.Channel{
				Type:    channel.TypeOpenai,
				BaseURL: "https://gateway.example/v1",
				Settings: &objects.ChannelSettings{RawHTTP: &objects.ChannelRawHTTP{
					Resource: "tavily-search",
				}},
			},
			wantAutoID: objects.ChannelBalanceProbePresetTavily,
		},
		{
			name:       "kimi code",
			preset:     objects.ChannelBalanceProbePresetKimiCode,
			url:        "https://api.kimi.com/coding/v1/usages",
			channel:    &ent.Channel{Type: channel.TypeMoonshotCoding},
			wantAutoID: objects.ChannelBalanceProbePresetKimiCode,
		},
		{
			name:      "exa team credits",
			preset:    objects.ChannelBalanceProbePresetExa,
			url:       "https://admin-api.exa.ai/team-management/credits",
			keyHeader: "x-api-key",
		},
		{
			name:   "firecrawl",
			preset: objects.ChannelBalanceProbePresetFirecrawl,
			url:    "https://api.firecrawl.dev/v2/team/credit-usage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := channelBalanceProbePresetByID(tt.preset)
			require.True(t, ok)
			require.Equal(t, objects.ChannelKeyHealthCheckHTTPMethodGet, spec.HTTP.Method)
			require.Equal(t, objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL, spec.HTTP.URLMode)
			require.Equal(t, tt.url, spec.HTTP.URL)
			if tt.keyHeader == "" {
				require.Nil(t, spec.HTTP.KeyInjection)
			} else {
				require.NotNil(t, spec.HTTP.KeyInjection)
				require.Equal(t, tt.keyHeader, spec.HTTP.KeyInjection.HeaderName)
			}
			if tt.channel != nil {
				selected, _, found := channelBalanceProbeSpecForChannel(tt.channel)
				require.True(t, found)
				require.Equal(t, tt.wantAutoID, selected.ID)
			}
		})
	}

	spec, _, ok := channelBalanceProbeSpecForChannel(&ent.Channel{
		Type:    channel.TypeMoonshotCoding,
		BaseURL: "https://api.example.test/kimi",
	})
	require.True(t, ok)
	require.Equal(t, objects.ChannelBalanceProbePresetKimiCode, spec.ID)
	require.Equal(t, "https://api.example.test/kimi/v1/usages", spec.HTTP.URL)

	spec, _, ok = channelBalanceProbeSpecForChannel(&ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.tavily.com/v1",
	})
	require.True(t, ok)
	require.Equal(t, objects.ChannelBalanceProbePresetTavily, spec.ID)

	gotURL, err := buildBalanceProbeHTTPURL(context.Background(), "", spec.HTTP)
	require.NoError(t, err)
	require.Equal(t, "https://api.tavily.com/usage", gotURL)
}

func TestTavilyUsageBalanceParserCapacityAndFallback(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		used      float64
		limit     float64
		remaining float64
		available bool
	}{
		{
			name: "key values take precedence",
			body: `{"key":{"usage":"20","limit":100},"account":{"plan_usage":3,"plan_limit":20,"paygo_usage":7,"paygo_limit":80,"current_plan":"Team"}}`,
			used: 20, limit: 100, remaining: 80, available: true,
		},
		{
			name: "account windows are summed",
			body: `{"key":{"usage":null,"limit":null},"account":{"plan_usage":3,"plan_limit":20,"paygo_usage":7,"paygo_limit":80}}`,
			used: 10, limit: 100, remaining: 90, available: true,
		},
		{
			name: "exhaustion clamps remaining",
			body: `{"key":{"usage":150,"limit":100},"account":{}}`,
			used: 150, limit: 100, remaining: 0, available: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := parseTavilyUsageBalanceSnapshot([]byte(tt.body), http.StatusOK)
			normalizeChannelBalanceCapacity(&snapshot, nil)
			require.True(t, snapshot.Success)
			require.NotNil(t, snapshot.Used)
			require.NotNil(t, snapshot.Limit)
			require.NotNil(t, snapshot.Remaining)
			require.InDelta(t, tt.used, *snapshot.Used, 1e-9)
			require.InDelta(t, tt.limit, *snapshot.Limit, 1e-9)
			require.InDelta(t, tt.remaining, *snapshot.Remaining, 1e-9)
			require.NotNil(t, snapshot.UsageRatio)
			require.NotNil(t, snapshot.Available)
			require.Equal(t, tt.available, *snapshot.Available)
			require.NotNil(t, snapshot.PrimaryBalance)
			require.Equal(t, objects.BalanceAmountKindRemaining, snapshot.PrimaryBalance.Kind)
			require.Equal(t, "CREDITS", snapshot.PrimaryBalance.Currency)
			require.Len(t, snapshot.Components, 3)
		})
	}
}

func TestKimiCodeUsageBalanceParserIncludesAllWindowsAndResetTimes(t *testing.T) {
	body := []byte(`{
		"usage":{"name":"Weekly limit","used":40,"limit":100,"resetAt":"2026-12-01T00:00:00.123456Z"},
		"limits":[
			{"detail":{"used":"15","limit":"20","resetAt":"2026-11-20T00:00:00Z"},"name":"Daily limit"},
			{"detail":{"remaining":5,"limit":50,"resetAt":"2026-11-25T00:00:00Z"},"name":"Five hour limit"}
		]
	}`)
	snapshot := parseKimiCodeUsageBalanceSnapshot(body, http.StatusOK)
	normalizeChannelBalanceCapacity(&snapshot, nil)

	require.True(t, snapshot.Success)
	require.NotNil(t, snapshot.Available)
	require.True(t, *snapshot.Available)
	require.Equal(t, 40.0, *snapshot.Used)
	require.Equal(t, 100.0, *snapshot.Limit)
	require.Equal(t, 60.0, *snapshot.Remaining)
	require.InDelta(t, 0.4, *snapshot.UsageRatio, 1e-9)
	require.Equal(t, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), *snapshot.ResetAt)
	require.Len(t, snapshot.Components, 9)
	require.NotNil(t, snapshot.PrimaryBalance)
	require.Equal(t, "Weekly limit remaining", snapshot.PrimaryBalance.Label)
	require.Equal(t, time.Date(2026, 12, 1, 0, 0, 0, 123456000, time.UTC), *snapshot.Components[2].ExpiresAt)

	exhausted := parseKimiCodeUsageBalanceSnapshot([]byte(`{"usage":{"used":100,"limit":100},"limits":[{"detail":{"used":1,"limit":5}}]}`), http.StatusOK)
	require.NotNil(t, exhausted.Available)
	require.False(t, *exhausted.Available)
}

func TestExaAndFirecrawlBalanceParsers(t *testing.T) {
	t.Run("exa reports USD remaining and expiration", func(t *testing.T) {
		snapshot := parseExaTeamCreditsBalanceSnapshot([]byte(`{"credits_usd":"12.50","expiring_credits":[{"expires_at":"2026-12-31T23:59:59.123Z"}]}`), http.StatusOK)
		manualLimit := 20.0
		normalizeChannelBalanceCapacity(&snapshot, &manualLimit)
		require.True(t, snapshot.Success)
		require.Equal(t, 12.5, *snapshot.Remaining)
		require.Equal(t, 20.0, *snapshot.Limit)
		require.Equal(t, 7.5, *snapshot.Used)
		require.Equal(t, 0.375, *snapshot.UsageRatio)
		require.Equal(t, "USD", snapshot.PrimaryBalance.Currency)
		expiry := time.Date(2026, 12, 31, 23, 59, 59, 123000000, time.UTC)
		require.Equal(t, expiry, *snapshot.ResetAt)
		require.Equal(t, expiry, *snapshot.Components[0].ExpiresAt)
	})

	t.Run("firecrawl reports exact capacity", func(t *testing.T) {
		snapshot := parseFirecrawlCreditsBalanceSnapshot([]byte(`{"success":true,"data":{"remainingCredits":"250","planCredits":1000}}`), http.StatusOK)
		normalizeChannelBalanceCapacity(&snapshot, nil)
		require.True(t, snapshot.Success)
		require.Equal(t, 750.0, *snapshot.Used)
		require.Equal(t, 1000.0, *snapshot.Limit)
		require.Equal(t, 250.0, *snapshot.Remaining)
		require.Equal(t, 0.75, *snapshot.UsageRatio)
		require.True(t, *snapshot.Available)
		require.Equal(t, "CREDITS", snapshot.PrimaryBalance.Currency)
	})
}

func TestNormalizeChannelBalanceCapacityForLegacyAndManualLimit(t *testing.T) {
	legacy := parseOpenRouterBalanceSnapshot([]byte(`{"data":{"total_credits":100.5,"total_usage":25.75}}`), http.StatusOK)
	selectChannelBalancePrimary(&legacy, "")
	normalizeChannelBalanceCapacity(&legacy, nil)
	require.Equal(t, 25.75, *legacy.Used)
	require.Equal(t, 100.5, *legacy.Limit)
	require.Equal(t, 74.75, *legacy.Remaining)
	require.InDelta(t, 25.75/100.5, *legacy.UsageRatio, 1e-9)
	require.Len(t, legacy.Components, 3)
	require.Equal(t, objects.BalanceAmountKindTotal, legacy.PrimaryBalance.Kind)

	manualLimit := 100.0
	manual := objects.ChannelKeyBalanceSnapshot{
		Success: true,
		PrimaryBalance: &objects.BalanceAmount{
			Amount:   72,
			Currency: "CREDITS",
			Kind:     objects.BalanceAmountKindAvailable,
			Label:    "balance",
		},
		Components: []objects.BalanceAmount{{
			Amount:   72,
			Currency: "CREDITS",
			Kind:     objects.BalanceAmountKindAvailable,
			Label:    "balance",
		}},
	}
	originalPrimary := *manual.PrimaryBalance
	originalComponent := manual.Components[0]
	normalizeChannelBalanceCapacity(&manual, &manualLimit)
	require.Equal(t, 28.0, *manual.Used)
	require.Equal(t, 100.0, *manual.Limit)
	require.Equal(t, 72.0, *manual.Remaining)
	require.Equal(t, 0.28, *manual.UsageRatio)
	require.Equal(t, originalPrimary, *manual.PrimaryBalance)
	require.Equal(t, originalComponent, manual.Components[0])
}
