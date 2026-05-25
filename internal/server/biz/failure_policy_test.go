package biz

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
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
