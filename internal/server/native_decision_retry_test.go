package server

import (
	"strings"
	"testing"

	"github.com/looplj/axonhub/internal/ent/channel"
	entmodel "github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/stretchr/testify/require"
)

func setNativeRetryBudgets(t *testing.T, f *nativeDecisionFixture) {
	t.Helper()
	policy := f.system.RetryPolicyOrDefault(f.ctx)
	policy.Enabled = true
	policy.MaxChannelRetries = 1
	policy.MaxSingleChannelRetries = 2
	require.NoError(t, f.system.SetRetryPolicy(f.ctx, policy))
}

func addNativeBackup(t *testing.T, f *nativeDecisionFixture) {
	t.Helper()
	first := f.channels[2]
	backup := f.db.Channel.Create().SetType(first.Type).SetStatus(channel.StatusEnabled).SetName("openai-independent-backup").SetBaseURL(strings.TrimSuffix(first.BaseURL, "/v1") + "/backup/v1").SetSupportedModels(first.SupportedModels).SetDefaultTestModel(first.DefaultTestModel).SetCredentials(objects.ChannelCredentials{APIKey: "backup-key"}).SetEndpoints(first.Endpoints).SetSettings(first.Settings).SaveX(f.ctx)
	m := f.db.Model.Query().Where(entmodel.ModelIDEQ("openai")).OnlyX(f.ctx)
	f.db.Model.UpdateOne(m).SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{
		{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: first.ID, ModelID: "gpt-6-luna"}},
		{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: backup.ID, ModelID: "gpt-6-luna"}},
	}}).SaveX(f.ctx)
	require.NoError(t, f.channelService.ReloadEnabledChannelsCache(f.ctx))
}

func TestNativeDecisionQuotaSkipsSameChannel(t *testing.T) {
	f := newNativeDecisionFixture(t)
	setNativeRetryBudgets(t, f)
	status, headers, body := f.post(t, "/v1/decisions", nativeOpenAIBody(`,"retry_case":"quota_all"`), f.key.Key, nil)
	require.Equal(t, 429, status, string(body))
	require.Contains(t, string(body), `"code":"insufficient_quota"`)
	require.Equal(t, "3", headers.Get("Retry-After"))
	require.Equal(t, "native-synthetic", headers.Get("X-Request-Id"))
	require.Equal(t, "ts-synthetic", headers.Get("X-Typesafe-Request-Id"))
	require.EqualValues(t, 1, f.calls.Load(), "same-channel retry budget must not repeat an upstream quota 429")
	require.Equal(t, 1, f.db.RequestExecution.Query().CountX(f.ctx))
}

func TestNativeDecisionAuthorizedPoolFailover(t *testing.T) {
	for _, mode := range []string{"quota_first", "rate_first", "quota_all"} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeDecisionFixture(t)
			setNativeRetryBudgets(t, f)
			addNativeBackup(t, f)
			status, _, body := f.post(t, "/v1/decisions", nativeOpenAIBody(`,"retry_case":"`+mode+`"`), f.key.Key, nil)
			if mode == "quota_all" {
				require.Equal(t, 429, status, string(body))
			} else {
				require.Equal(t, 200, status, string(body))
			}
			require.EqualValues(t, 2, f.calls.Load())
			executions := f.db.RequestExecution.Query().AllX(f.ctx)
			require.Len(t, executions, 2)
			require.NotEqual(t, executions[0].ChannelID, executions[1].ChannelID)
			for _, row := range executions {
				require.Equal(t, "gpt-6-luna", row.ModelID)
				require.Equal(t, "openai/decisions", row.Format)
			}
			// Both wire requests use the same model/protocol but independent credentials.
			first, second := <-f.received, <-f.received
			require.NotEqual(t, first.Header.Get("Authorization"), second.Header.Get("Authorization"))
		})
	}
}

func TestNativeDecisionLocalLimitsNeverSent(t *testing.T) {
	for _, mode := range []string{"quota", "model_permission", "project_permission"} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeDecisionFixture(t)
			setNativeRetryBudgets(t, f)
			addNativeBackup(t, f)
			key := f.key.Key
			body := nativeOpenAIBody("")
			path := "/v1/decisions"
			switch mode {
			case "quota":
				var zero int64
				f.db.APIKey.UpdateOne(f.key).SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "budget", Profiles: []objects.APIKeyProfile{{Name: "budget", Quota: &objects.APIKeyQuota{Requests: &zero, Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}}}}).SaveX(f.ctx)
				f.keyService.InvalidateAPIKeyCaches(f.ctx, key)
			case "model_permission":
				key = f.blockedKey.Key
			case "project_permission":
				key = f.projectBlockedKey.Key
				body = nativeSystemBody("direct", "")
				path = "/v1/systemone"
			}
			status, _, result := f.post(t, path, body, key, nil)
			require.GreaterOrEqual(t, status, 400, string(result))
			require.EqualValues(t, 0, f.calls.Load())
			require.Equal(t, 0, f.db.RequestExecution.Query().CountX(f.ctx))
		})
	}
}

func TestNativeDecisionErrorHeaders(t *testing.T) {
	for _, path := range []string{"/v1/systemone", "/v1/decisions"} {
		t.Run(path, func(t *testing.T) {
			f := newNativeDecisionFixture(t)
			body := nativeSystemBody("direct", `,"test_error":502`)
			if path == "/v1/decisions" {
				body = nativeOpenAIBody(`,"test_error":502`)
			}
			status, headers, result := f.post(t, path, body, f.key.Key, nil)
			require.Equal(t, 502, status, string(result))
			require.Equal(t, "native-synthetic", headers.Get("X-Request-Id"))
			require.Equal(t, "ts-synthetic", headers.Get("X-Typesafe-Request-Id"))
			require.Equal(t, "3", headers.Get("Retry-After"))
		})
	}
}
