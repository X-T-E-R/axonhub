package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
)

func TestResolveEffectiveFailurePolicyModes(t *testing.T) {
	global := &RetryPolicy{FailurePolicy: objects.FailurePolicy{
		KeyProfiles: []objects.FailurePolicyProfile{{ID: "global-key", Name: "Global key", Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionReportOnly}}}},
	}}
	channelProfile := objects.FailurePolicyProfile{ID: "channel-key", Name: "Channel key", Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionReportOnly}}}

	tests := []struct {
		name string
		mode objects.ChannelFailurePolicyMode
		want []string
	}{
		{name: "inherit", mode: objects.ChannelFailurePolicyModeInherit, want: []string{"global-key"}},
		{name: "override", mode: objects.ChannelFailurePolicyModeOverride, want: []string{"channel-key"}},
		{name: "merge", mode: objects.ChannelFailurePolicyModeMerge, want: []string{"channel-key", "global-key"}},
		{name: "disabled", mode: objects.ChannelFailurePolicyModeDisabled, want: []string{}},
		{name: "empty mode with local profiles merges", mode: "", want: []string{"channel-key", "global-key"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			effective := resolveEffectiveFailurePolicy(global, &objects.ChannelSettings{
				FailurePolicy: &objects.ChannelFailurePolicy{
					Mode:        tt.mode,
					KeyProfiles: []objects.FailurePolicyProfile{channelProfile},
				},
			}, true)

			got := make([]string, 0, len(effective.KeyProfiles))
			for _, profile := range effective.KeyProfiles {
				got = append(got, profile.ID)
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestLegacyPoliciesSynthesizeProfiles(t *testing.T) {
	retry := &RetryPolicy{AutoDisableChannel: AutoDisableChannel{
		Enabled: true,
		Statuses: []AutoDisableChannelStatus{{
			Status: http.StatusTooManyRequests,
			Times:  2,
		}},
	}}
	health := &objects.ChannelKeyHealthCheck{
		FailureThreshold: 3,
		FailureAction:    objects.ChannelKeyHealthCheckFailureActionArchive,
	}

	effective := resolveEffectiveFailurePolicy(retry, &objects.ChannelSettings{KeyHealthCheck: health}, false)
	require.Len(t, effective.KeyProfiles, 2)
	require.Equal(t, "legacy-health-check-threshold", effective.KeyProfiles[0].ID)
	require.Equal(t, objects.FailurePolicyActionArchiveKey, effective.KeyProfiles[0].Actions[0].Type)
	require.Equal(t, "legacy-auto-disable-key-429", effective.KeyProfiles[1].ID)
	require.Empty(t, effective.ChannelProfiles)

	keyMatches := evaluateFailurePolicyProfiles(effective.KeyProfiles, failurePolicyEvent{
		Source:       objects.FailurePolicyEventSourceRequestFailure,
		Target:       objects.FailurePolicyTargetKey,
		StatusCode:   http.StatusTooManyRequests,
		FailureCount: 2,
	})
	require.Len(t, keyMatches, 1)
	require.Equal(t, objects.FailurePolicyActionDisableKey, keyMatches[0].Actions[0].Type)

	manualMatches := evaluateFailurePolicyProfiles(effective.KeyProfiles, failurePolicyEvent{
		Source:       objects.FailurePolicyEventSourceManualHealthCheckFailure,
		Target:       objects.FailurePolicyTargetKey,
		FailureCount: 3,
	})
	require.Len(t, manualMatches, 1)
	require.Equal(t, objects.FailurePolicyActionArchiveKey, manualMatches[0].Actions[0].Type)
}

func TestFailurePolicyEmptySourcesDoNotMatchManualHealthChecks(t *testing.T) {
	profiles := []objects.FailurePolicyProfile{{
		ID:      "empty-source-profile",
		Name:    "Empty source profile",
		Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDeleteKey}},
	}}

	manualMatches := evaluateFailurePolicyProfiles(profiles, failurePolicyEvent{
		Source:       objects.FailurePolicyEventSourceManualHealthCheckFailure,
		Target:       objects.FailurePolicyTargetKey,
		FailureCount: 1,
	})
	require.Empty(t, manualMatches)

	requestMatches := evaluateFailurePolicyProfiles(profiles, failurePolicyEvent{
		Source:       objects.FailurePolicyEventSourceRequestFailure,
		Target:       objects.FailurePolicyTargetKey,
		FailureCount: 1,
	})
	require.Len(t, requestMatches, 1)
}

func TestFailurePolicyConditionMatchesDefaultsMissingCombinerToAnd(t *testing.T) {
	minFailures := 3
	result := ChannelKeyHealthCheckResult{
		Success:    false,
		StatusCode: http.StatusUnauthorized,
	}
	condition := objects.ChannelKeyHealthCheckPolicyCondition{
		MinFailureCount: &minFailures,
		StatusCodes:     []int{http.StatusTooManyRequests},
	}

	require.False(t, failurePolicyConditionMatches("", condition, result, minFailures, objects.ChannelKeyHealthCheckTriggerRequest, false, ""))
	require.True(
		t,
		failurePolicyConditionMatches(
			objects.FailurePolicyConditionCombinerOr,
			condition,
			result,
			minFailures,
			objects.ChannelKeyHealthCheckTriggerRequest,
			false,
			"",
		),
	)
}

func TestCloneChannelSettingsNormalizesLegacyFailurePolicyCombiner(t *testing.T) {
	settings := &objects.ChannelSettings{
		FailurePolicy: &objects.ChannelFailurePolicy{
			KeyProfiles: []objects.FailurePolicyProfile{{
				ID:      "legacy-profile",
				Name:    "Legacy profile",
				Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
				Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
					StatusCodes: []int{http.StatusUnauthorized},
				},
				Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
			}},
		},
	}

	cloned := cloneChannelSettings(settings)

	require.NotNil(t, cloned)
	require.NotNil(t, cloned.FailurePolicy)
	require.Len(t, cloned.FailurePolicy.KeyProfiles, 1)
	require.Equal(t, objects.FailurePolicyConditionCombinerAnd, cloned.FailurePolicy.KeyProfiles[0].ConditionCombiner)
}

func TestRequestFailurePolicyReportOnlyRecordsKeyHistory(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	apiKey := "sk-report-only-secret"
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Request Report Policy").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{apiKey}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			FailurePolicy: &objects.ChannelFailurePolicy{
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:      "request-report",
					Name:    "Request report",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusTooManyRequests},
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionReportOnly}},
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	changed := svc.handleRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		APIKey:             apiKey,
		ResponseStatusCode: http.StatusTooManyRequests,
		Success:            false,
	}, &RetryPolicy{})
	require.False(t, changed)

	updated := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, channel.StatusEnabled, updated.Status)
	require.Empty(t, updated.DisabledAPIKeys)
	require.NotNil(t, updated.Settings)
	require.NotNil(t, updated.Settings.KeyHealthCheck)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 1)
	meta := updated.Settings.KeyHealthCheck.KeyMetadata[0]
	require.Equal(t, objects.ChannelAPIKeyFingerprint(apiKey), meta.ID)
	require.Equal(t, objects.MaskChannelAPIKey(apiKey), meta.MaskedKey)
	require.False(t, *meta.Success)
	require.Equal(t, 1, meta.FailureCount)
	require.Equal(t, "Request report", meta.MatchedPolicy)
	require.Equal(t, string(objects.FailurePolicyActionReportOnly), meta.Action)
	require.Nil(t, meta.NextCheckAt)
	require.Len(t, meta.History, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerRequest, meta.History[0].Trigger)
	require.Equal(t, "Request report", meta.History[0].MatchedPolicy)
	require.Equal(t, string(objects.FailurePolicyActionReportOnly), meta.History[0].Action)
	require.Nil(t, meta.History[0].NextCheckAt)
	require.Len(t, updated.Settings.KeyHealthCheck.History, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerRequest, updated.Settings.KeyHealthCheck.History[0].Trigger)

	rawHistory, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NotContains(t, string(rawHistory), apiKey)
}

func TestRequestFailurePolicyDisableKeyUpdatesInventoryState(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	apiKey := "sk-disable-secret"
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Request Disable Policy").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{apiKey, "sk-spare-secret"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{HistoryLimit: 10},
			FailurePolicy: &objects.ChannelFailurePolicy{
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:      "request-disable",
					Name:    "Request disable",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusUnauthorized},
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	changed := svc.handleRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		APIKey:             apiKey,
		ResponseStatusCode: http.StatusUnauthorized,
		Success:            false,
	}, &RetryPolicy{})
	require.True(t, changed)

	updated := client.Channel.GetX(ctx, ch.ID)
	require.Len(t, updated.DisabledAPIKeys, 1)
	require.Equal(t, apiKey, updated.DisabledAPIKeys[0].Key)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.Equal(t, string(objects.FailurePolicyActionDisableKey), updated.Settings.KeyHealthCheck.KeyMetadata[0].Action)

	items, err := svc.ChannelAPIKeyInventory(ctx, ch.ID)
	require.NoError(t, err)
	var disabled *ChannelAPIKeyInventoryItem
	for _, item := range items {
		if item.ID == objects.ChannelAPIKeyFingerprint(apiKey) {
			disabled = item
			break
		}
	}
	require.NotNil(t, disabled)
	require.Equal(t, objects.ChannelKeyStatusDisabled, disabled.Status)
	require.Equal(t, "Request disable", disabled.MatchedPolicy)
	require.Equal(t, string(objects.FailurePolicyActionDisableKey), disabled.Action)
}

func TestRequestFailurePolicyBackoffRecordsHistoryAndNextCheckAt(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	apiKey := "sk-backoff-secret"
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Request Backoff Policy").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{apiKey}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{HistoryLimit: 10},
			FailurePolicy: &objects.ChannelFailurePolicy{
				Mode: objects.ChannelFailurePolicyModeMerge,
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:      "request-backoff",
					Name:    "Request backoff",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusTooManyRequests},
					},
					Actions: []objects.FailurePolicyAction{{
						Type: objects.FailurePolicyActionBackoffKey,
						Backoff: &objects.ChannelKeyHealthCheckBackoff{
							Mode:            objects.ChannelKeyHealthCheckBackoffModeFixed,
							IntervalMinutes: 7,
						},
					}},
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	start := time.Now()
	changed := svc.handleRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		APIKey:             apiKey,
		ResponseStatusCode: http.StatusTooManyRequests,
		Success:            false,
	}, &RetryPolicy{})
	require.True(t, changed)

	updated := client.Channel.GetX(ctx, ch.ID)
	require.NotNil(t, updated.Settings)
	require.NotNil(t, updated.Settings.KeyHealthCheck)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 1)
	meta := updated.Settings.KeyHealthCheck.KeyMetadata[0]
	require.NotNil(t, meta.NextCheckAt)
	require.GreaterOrEqual(t, meta.NextCheckAt.Sub(start), 6*time.Minute)
	require.LessOrEqual(t, meta.NextCheckAt.Sub(start), 8*time.Minute)
	require.Equal(t, 1, meta.BackoffAttempt)
	require.Equal(t, "Request backoff", meta.MatchedPolicy)
	require.Equal(t, string(objects.FailurePolicyActionBackoffKey), meta.Action)
	require.Len(t, meta.History, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerRequest, meta.History[0].Trigger)
	require.Equal(t, string(objects.FailurePolicyActionBackoffKey), meta.History[0].Action)
	require.NotNil(t, meta.History[0].NextCheckAt)
	require.Equal(t, meta.NextCheckAt, meta.History[0].NextCheckAt)
}

func TestRequestFailurePolicyDeleteKeyKeepsRequestHistory(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	apiKey := "sk-delete-secret"
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Request Delete Policy").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{apiKey, "sk-spare-secret"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{HistoryLimit: 10},
			FailurePolicy: &objects.ChannelFailurePolicy{
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:      "request-delete",
					Name:    "Request delete",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusUnauthorized},
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDeleteKey}},
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	changed := svc.handleRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		APIKey:             apiKey,
		ResponseStatusCode: http.StatusUnauthorized,
		Success:            false,
	}, &RetryPolicy{})
	require.True(t, changed)

	updated := client.Channel.GetX(ctx, ch.ID)
	require.NotContains(t, updated.Credentials.APIKeys, apiKey)
	require.Len(t, updated.Settings.KeyHealthCheck.History, 1)
	entry := updated.Settings.KeyHealthCheck.History[0]
	require.Contains(t, entry.ID, objects.ChannelAPIKeyFingerprint(apiKey))
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerRequest, entry.Trigger)
	require.Equal(t, "Request delete", entry.MatchedPolicy)
	require.Equal(t, string(objects.FailurePolicyActionDeleteKey), entry.Action)

	rawHistory, err := json.Marshal(updated.Settings.KeyHealthCheck.History)
	require.NoError(t, err)
	require.NotContains(t, string(rawHistory), apiKey)
}

func TestApplyRequestFailurePolicyPreventsRecordPerformanceDoubleApply(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	apiKey := "sk-delete-once-secret"
	spareKey := "sk-spare-secret"
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Request Delete Policy Once").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{apiKey, spareKey}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{HistoryLimit: 10},
			FailurePolicy: &objects.ChannelFailurePolicy{
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:      "request-delete",
					Name:    "Request delete",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusUnauthorized},
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDeleteKey}},
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	perf := &PerformanceRecord{
		ChannelID:          ch.ID,
		APIKey:             apiKey,
		ResponseStatusCode: http.StatusUnauthorized,
		Success:            false,
		RequestCompleted:   true,
		EndTime:            time.Now(),
	}

	require.True(t, svc.ApplyRequestFailurePolicy(ctx, perf))
	require.True(t, perf.FailurePolicyHandled)
	require.True(t, perf.FailurePolicyRoutingChanged)

	svc.RecordPerformance(ctx, perf)

	updated := client.Channel.GetX(ctx, ch.ID)
	require.Equal(t, []string{spareKey}, updated.Credentials.APIKeys)
	require.Len(t, updated.Settings.KeyHealthCheck.History, 1)
	require.Equal(t, string(objects.FailurePolicyActionDeleteKey), updated.Settings.KeyHealthCheck.History[0].Action)
}

func TestRequestFailurePolicyChannelTargetRecordsChannelHistory(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	minFailures := 1
	ch := client.Channel.Create().
		SetName("Request Channel Policy").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-channel-secret"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			FailurePolicy: &objects.ChannelFailurePolicy{
				ChannelProfiles: []objects.FailurePolicyProfile{{
					ID:      "request-channel-report",
					Name:    "Request channel report",
					Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						MinFailureCount: &minFailures,
						StatusCodes:     []int{http.StatusInternalServerError},
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionReportOnly}},
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	changed := svc.handleRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		ResponseStatusCode: http.StatusInternalServerError,
		Success:            false,
	}, &RetryPolicy{})
	require.False(t, changed)

	updated := client.Channel.GetX(ctx, ch.ID)
	require.NotNil(t, updated.Settings)
	require.NotNil(t, updated.Settings.KeyHealthCheck)
	require.Len(t, updated.Settings.KeyHealthCheck.History, 1)
	entry := updated.Settings.KeyHealthCheck.History[0]
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerRequest, entry.Trigger)
	require.Equal(t, "Request channel report", entry.MatchedPolicy)
	require.Equal(t, string(objects.FailurePolicyActionReportOnly), entry.Action)
}

func TestUpdateChannelPreservesRequestFailurePolicyChannelHistory(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	eventTime := time.Now().Add(-time.Minute)
	ch := client.Channel.Create().
		SetName("Preserve Request History").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-preserve-secret"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				History: []objects.ChannelKeyHealthCheckHistoryEntry{{
					ID:            "channel:1:request",
					CheckedAt:     eventTime,
					Success:       false,
					Trigger:       objects.ChannelKeyHealthCheckTriggerRequest,
					MatchedPolicy: "Request channel report",
					Action:        string(objects.FailurePolicyActionReportOnly),
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	updated, err := svc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:         true,
				IntervalMinutes: 30,
				HistoryLimit:    10,
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updated.Settings)
	require.NotNil(t, updated.Settings.KeyHealthCheck)
	require.Len(t, updated.Settings.KeyHealthCheck.History, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerRequest, updated.Settings.KeyHealthCheck.History[0].Trigger)
	require.Equal(t, "Request channel report", updated.Settings.KeyHealthCheck.History[0].MatchedPolicy)
}

func TestRoutableChannelAPIKeysExcludeBackedOffKeys(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	key1Backoff := now.Add(10 * time.Minute)
	settings := &objects.ChannelSettings{KeyHealthCheck: &objects.ChannelKeyHealthCheck{
		KeyMetadata: []objects.ChannelKeyMetadata{{
			ID:          objects.ChannelAPIKeyFingerprint("key-1"),
			NextCheckAt: &key1Backoff,
		}},
	}}

	got := routableChannelAPIKeys(objects.ChannelCredentials{APIKeys: []string{"key-1", "key-2"}}, nil, settings, now)
	require.Equal(t, []string{"key-2"}, got)

	got = routableChannelAPIKeys(objects.ChannelCredentials{APIKeys: []string{"key-1", "key-2"}}, nil, settings, key1Backoff)
	require.Equal(t, []string{"key-1", "key-2"}, got)
}

func TestBuildChannelWithTransformer_AllKeysBackedOffReturnsError(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(context.Background())
	now := time.Now().Add(30 * time.Minute)
	backedOffID := objects.ChannelAPIKeyFingerprint("backed-off-key")

	entChannel := client.Channel.Create().
		SetName("Backed Off Channel").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"backed-off-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID:          backedOffID,
					NextCheckAt: &now,
				}},
			},
		}).
		SaveX(ctx)

	svc := NewChannelServiceForTest(client)
	built, err := svc.buildChannelWithTransformer(entChannel)
	require.Nil(t, built)
	require.ErrorContains(t, err, "missing api key")
}
