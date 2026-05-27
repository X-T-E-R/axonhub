package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelkeymonitoringevent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestMonitoringSettingsDefaultsAndValidation(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	defaults, err := svc.SystemService.MonitoringSettings(ctx)
	require.NoError(t, err)
	require.False(t, defaults.Enabled)
	require.Equal(t, 30, defaults.HistoryRetentionDays)
	require.Empty(t, defaults.Rules)

	err = svc.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{
		Enabled:              true,
		HistoryRetentionDays: -1,
		Rules: []MonitoringRule{{
			ID:      "recover-disabled",
			Name:    " Recover disabled ",
			Enabled: nil,
			Schedule: MonitoringRuleSchedule{
				IntervalMinutes:   -10,
				MaxChannels:       0,
				MaxKeysPerChannel: 0,
			},
			Targets: MonitoringRuleTargets{
				ChannelIDs:      []int{0, 7, 7},
				ChannelStatuses: []string{"enabled", "invalid"},
				KeyStatuses:     []objects.ChannelKeyStatus{objects.ChannelKeyStatusDisabled, "invalid"},
			},
		}},
	})
	require.NoError(t, err)

	got, err := svc.SystemService.MonitoringSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, 30, got.HistoryRetentionDays)
	require.Len(t, got.Rules, 1)
	require.True(t, lo.FromPtr(got.Rules[0].Enabled))
	require.Equal(t, "Recover disabled", got.Rules[0].Name)
	require.Equal(t, 60, got.Rules[0].Schedule.IntervalMinutes)
	require.Equal(t, []int{7}, got.Rules[0].Targets.ChannelIDs)
	require.Equal(t, []string{"enabled"}, got.Rules[0].Targets.ChannelStatuses)
	require.Equal(t, []objects.ChannelKeyStatus{objects.ChannelKeyStatusDisabled}, got.Rules[0].Targets.KeyStatuses)

	err = svc.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{
		Rules: []MonitoringRule{{ID: "dup", Name: "one"}, {ID: "dup", Name: "two"}},
	})
	require.Error(t, err)
}

func TestMonitoringSettingsBackfillsLegacyBlankRuleFields(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	raw, err := json.Marshal(map[string]any{
		"enabled":              true,
		"historyRetentionDays": 0,
		"rules": []map[string]any{{
			"id":       " ",
			"name":     "",
			"schedule": map[string]any{},
			"targets":  map[string]any{},
		}},
	})
	require.NoError(t, err)

	_, err = client.System.Create().
		SetKey(SystemKeyMonitoringSettings).
		SetValue(string(raw)).
		Save(ctx)
	require.NoError(t, err)

	got, err := svc.SystemService.MonitoringSettings(ctx)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, 30, got.HistoryRetentionDays)
	require.Len(t, got.Rules, 1)
	require.Equal(t, "monitor-rule-1", got.Rules[0].ID)
	require.Equal(t, "Monitoring rule 1", got.Rules[0].Name)
	require.Equal(t, 60, got.Rules[0].Schedule.IntervalMinutes)
	require.Equal(t, 100, got.Rules[0].Schedule.HistoryLimit)
	require.Equal(t, []string{"enabled"}, got.Rules[0].Targets.ChannelStatuses)
	require.Equal(t, []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive}, got.Rules[0].Targets.KeyStatuses)
}

func TestMonitoringRuleRecoversDisabledKeyAndWritesEvent(t *testing.T) {
	disableChannelKeyHealthCheckDelays(t)

	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc.SetChannelKeyHealthCheckTester(channelKeyHealthCheckTesterFunc(func(ctx context.Context, channelID objects.GUID, key string, modelID *string, proxy *httpclient.ProxyConfig) ChannelKeyHealthCheckBuiltinResult {
		require.Equal(t, "disabled-key", key)
		return ChannelKeyHealthCheckBuiltinResult{Success: true, Reason: "ok"}
	}))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Monitoring Disabled Recovery").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"active-key", "disabled-key"}}).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{{
			Key:        "disabled-key",
			DisabledAt: time.Now().Add(-time.Hour),
			Reason:     "previous failure",
		}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{KeyHealthCheck: &objects.ChannelKeyHealthCheck{}}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{
		Enabled: true,
		Rules: []MonitoringRule{{
			ID:   "recover-disabled",
			Name: "Recover disabled keys",
			Targets: MonitoringRuleTargets{
				ChannelIDs:  []int{ch.ID},
				KeyStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusDisabled},
			},
			KeyProfiles: []objects.FailurePolicyProfile{{
				ID:   "success-enables-disabled",
				Name: "Success enables disabled key",
				Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
					Success:     lo.ToPtr(true),
					KeyStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusDisabled},
				},
				Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionEnableKey}},
			}},
		}},
	})
	require.NoError(t, err)

	err = svc.RunDueChannelKeyHealthChecks(ctx, time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, updated.DisabledAPIKeys)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.Equal(t, objects.ChannelAPIKeyFingerprint("disabled-key"), updated.Settings.KeyHealthCheck.KeyMetadata[0].ID)
	require.Equal(t, "enable_key", updated.Settings.KeyHealthCheck.KeyMetadata[0].Action)

	events, err := client.ChannelKeyMonitoringEvent.Query().
		Where(channelkeymonitoringevent.RuleIDEQ("recover-disabled")).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, objects.ChannelAPIKeyFingerprint("disabled-key"), events[0].KeyID)
	require.Equal(t, objects.MaskChannelAPIKey("disabled-key"), events[0].MaskedKey)
	require.True(t, events[0].Success)
	require.Equal(t, "enable_key", events[0].Action)
	require.NotContains(t, events[0].Reason, "disabled-key")
}

func TestMonitoringRuleRestoresArchivedKey(t *testing.T) {
	disableChannelKeyHealthCheckDelays(t)

	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc.SetChannelKeyHealthCheckTester(channelKeyHealthCheckTesterFunc(func(ctx context.Context, channelID objects.GUID, key string, modelID *string, proxy *httpclient.ProxyConfig) ChannelKeyHealthCheckBuiltinResult {
		require.Equal(t, "archived-key", key)
		return ChannelKeyHealthCheckBuiltinResult{Success: true, Reason: "ok"}
	}))

	archivedID := objects.ChannelAPIKeyFingerprint("archived-key")
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Monitoring Archived Recovery").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"active-key", "archived-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				ArchivedKeys: []objects.ChannelArchivedAPIKey{{
					ID:        archivedID,
					MaskedKey: objects.MaskChannelAPIKey("archived-key"),
					Reason:    "previous failure",
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{
		Enabled: true,
		Rules: []MonitoringRule{{
			ID:   "recover-archived",
			Name: "Recover archived keys",
			Targets: MonitoringRuleTargets{
				ChannelIDs:  []int{ch.ID},
				KeyStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusArchived},
			},
			KeyProfiles: []objects.FailurePolicyProfile{{
				ID:   "success-restores-archived",
				Name: "Success restores archived key",
				Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
					Success:     lo.ToPtr(true),
					KeyStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusArchived},
				},
				Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionRestoreKey}},
			}},
		}},
	})
	require.NoError(t, err)

	err = svc.RunDueChannelKeyHealthChecks(ctx, time.Date(2026, 5, 26, 12, 30, 0, 0, time.UTC))
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, updated.Settings.KeyHealthCheck.ArchivedKeys)

	events, err := client.ChannelKeyMonitoringEvent.Query().
		Where(channelkeymonitoringevent.RuleIDEQ("recover-archived")).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, archivedID, events[0].KeyID)
	require.Equal(t, "restore_key", events[0].Action)
}

func TestMonitoringBalanceProbeUsesRuleKeyStatusesNotProbeIncludeStatuses(t *testing.T) {
	disableChannelKeyHealthCheckDelays(t)

	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"balance":"5.5","currency":"USD","available":true}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer client.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Monitoring Balance Targets").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"active-key", "disabled-key"}}).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{{
			Key:        "disabled-key",
			DisabledAt: time.Now().Add(-time.Hour),
			Reason:     "previous failure",
		}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{},
			BalanceProbe: &objects.ChannelBalanceProbe{
				Preset:          objects.ChannelBalanceProbePresetCustom,
				IncludeStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusDisabled},
				HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
					Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
					URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
					Path:             "/balance",
					ExpectedStatuses: []int{http.StatusOK},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{
		Enabled: true,
		Rules: []MonitoringRule{{
			ID:        "balance-active-only",
			Name:      "Balance active only",
			ProbeType: MonitoringProbeTypeChannelBalanceProbe,
			Targets: MonitoringRuleTargets{
				ChannelIDs:  []int{ch.ID},
				KeyStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive},
			},
			Schedule: MonitoringRuleSchedule{MaxKeysPerChannel: 10},
		}},
	})
	require.NoError(t, err)

	err = svc.RunDueChannelKeyHealthChecks(ctx, time.Date(2026, 5, 26, 13, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "Bearer active-key", gotAuthorization)

	events, err := client.ChannelKeyMonitoringEvent.Query().
		Where(channelkeymonitoringevent.RuleIDEQ("balance-active-only")).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, objects.ChannelAPIKeyFingerprint("active-key"), events[0].KeyID)
}

func TestOrderedKeyHealthCheckActionNamesRestoresBeforeEnable(t *testing.T) {
	require.Equal(t,
		[]string{"restore_key", "enable_key", "disable_key"},
		orderedKeyHealthCheckActionNames("enable_key,restore_key,disable_key"),
	)
	require.Equal(t,
		[]string{"restore_key", "enable_key"},
		orderedKeyHealthCheckActionNames("enable_key, restore_key"),
	)
}
