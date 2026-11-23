package api

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestBuildQuotaExposure_SharedWalletDedupAcrossKeysAndLinkedChannels(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	now := time.Now().UTC().Truncate(time.Second)
	sharedSnap := &objects.ChannelKeyBalanceSnapshot{
		Success:        true,
		CheckedAt:      now,
		Available:      lo.ToPtr(true),
		Limit:          lo.ToPtr(100.0),
		Used:           lo.ToPtr(50.0),
		Remaining:      lo.ToPtr(50.0),
		WalletScope:    "shared_wallet",
		PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 50, Kind: objects.BalanceAmountKindRemaining},
	}

	// Owner Exa channel with 8 keys sharing 1 team wallet ($50 remaining).
	exaKeys := []string{"exa-1", "exa-2", "exa-3", "exa-4", "exa-5", "exa-6", "exa-7", "exa-8"}
	ownerEnt, err := client.Channel.Create().
		SetName("Exa Raw").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.exa.ai").
		SetSupportedModels([]string{"exa"}).
		SetDefaultTestModel("exa").
		SetCredentials(objects.ChannelCredentials{APIKeys: exaKeys}).
		SetSettings(&objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:      true,
				Preset:       objects.ChannelBalanceProbePresetExa,
				SharedWallet: true,
			},
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				SharedBalanceSnapshot: sharedSnap,
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	// Linked sibling channel referencing ownerEnt via CredentialSourceChannelID.
	linkedEnt, err := client.Channel.Create().
		SetName("Exa Linked").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.exa.ai").
		SetSupportedModels([]string{"exa-fast"}).
		SetDefaultTestModel("exa-fast").
		SetCredentials(objects.ChannelCredentials{}).
		SetSettings(&objects.ChannelSettings{
			CredentialSourceChannelID: &ownerEnt.ID,
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:      true,
				Preset:       objects.ChannelBalanceProbePresetExa,
				SharedWallet: true,
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	// Unlimited & FreeTier channels must NOT corrupt USD wallet totals!
	orFreeEnt, err := client.Channel.Create().
		SetName("OpenRouter Free").
		SetType(channel.TypeOpenrouter).
		SetBaseURL("https://openrouter.ai/api/v1").
		SetSupportedModels([]string{"openrouter/free"}).
		SetDefaultTestModel("openrouter/free").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-or-free-1"}}).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID: objects.ChannelAPIKeyFingerprint("sk-or-free-1"),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						Success:        true,
						CheckedAt:      now,
						Available:      lo.ToPtr(true),
						FreeTier:       lo.ToPtr(true),
						Remaining:      lo.ToPtr(0.0),
						PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 0, Kind: objects.BalanceAmountKindRemaining},
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	unlimEnt, err := client.Channel.Create().
		SetName("NewAPI Unlimited").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.n.icu/v1").
		SetSupportedModels([]string{"gpt-4o"}).
		SetDefaultTestModel("gpt-4o").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-unlim-1"}}).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID: objects.ChannelAPIKeyFingerprint("sk-unlim-1"),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						Success:        true,
						CheckedAt:      now,
						Available:      lo.ToPtr(true),
						Unlimited:      lo.ToPtr(true),
						Remaining:      lo.ToPtr(999999.0),
						PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 999999, Kind: objects.BalanceAmountKindRemaining},
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	// Percentage rolling-window channel must NOT poison USD totals as unclassified!
	pctEnt, err := client.Channel.Create().
		SetName("Kimi Rolling Pct").
		SetType(channel.TypeMoonshotCoding).
		SetBaseURL("https://api.kimi.com/coding/v1").
		SetSupportedModels([]string{"exa"}).
		SetDefaultTestModel("exa").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-kimi-pct-1"}}).
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID: objects.ChannelAPIKeyFingerprint("sk-kimi-pct-1"),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						Success:        true,
						CheckedAt:      now,
						Available:      lo.ToPtr(true),
						Remaining:      lo.ToPtr(80.0),
						PrimaryBalance: &objects.BalanceAmount{Currency: "%", Amount: 80, Kind: objects.BalanceAmountKindRemaining},
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	chSvc := biz.NewChannelServiceForTest(client)

	channels := []*biz.Channel{
		{Channel: ownerEnt},
		{Channel: linkedEnt},
		{Channel: orFreeEnt},
		{Channel: unlimEnt},
		{Channel: pctEnt},
	}

	resp, err := buildQuotaExposureForChannels(ctx, chSvc, channels)
	require.NoError(t, err)
	require.Len(t, resp.Data, 5)

	// Per-channel summary on 8-key SharedWallet Exa must report $50 (NOT $50 * 8 = $400)!
	var exaExposure *QuotaExposureChannel
	var linkedExposure *QuotaExposureChannel
	for _, c := range resp.Data {
		switch c.ChannelName {
		case "Exa Raw":
			exaExposure = c
		case "Exa Linked":
			linkedExposure = c
		}
	}
	require.NotNil(t, exaExposure)
	require.True(t, exaExposure.SharedWallet)
	require.NotNil(t, exaExposure.QuotaSummary.TotalRemaining)
	require.InDelta(t, 50.0, *exaExposure.QuotaSummary.TotalRemaining, 1e-6, "8-key SharedWallet Exa must NOT multiply by 8")
	require.NotNil(t, linkedExposure)
	require.Equal(t, exaExposure.WalletID, linkedExposure.WalletID, "linked channel must share identical canonical wallet_id with owner")

	// Real /v1/models?include=quotas code path via OpenAIHandlers.attachOpenAIModelQuota:
	// Ensure linked channel sharing Exa's wallet + Unlimited + FreeTier + % rolling window
	// produces exact USD total $50.0 (NOT $100, NOT $999999+50, NOT suppressed by %).
	ownerEnt.Status = channel.StatusEnabled
	linkedEnt.Status = channel.StatusEnabled
	linkedEnt.SupportedModels = []string{"exa"}
	orFreeEnt.Status = channel.StatusEnabled
	orFreeEnt.SupportedModels = []string{"exa"}
	unlimEnt.Status = channel.StatusEnabled
	unlimEnt.SupportedModels = []string{"exa"}
	pctEnt.Status = channel.StatusEnabled

	_, err = client.Model.Create().
		SetDeveloper("test").
		SetModelID("exa").
		SetName("Exa").
		SetIcon("exa").
		SetGroup("test").
		SetType(model.TypeChat).
		SetModelCard(&objects.ModelCard{}).
		SetStatus(model.StatusEnabled).
		SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{
			{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: ownerEnt.ID, ModelID: "exa"}},
			{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: linkedEnt.ID, ModelID: "exa"}},
			{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: orFreeEnt.ID, ModelID: "exa"}},
			{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: unlimEnt.ID, ModelID: "exa"}},
			{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: pctEnt.ID, ModelID: "exa"}},
		}}).
		Save(ctx)
	require.NoError(t, err)

	t.Cleanup(chSvc.Stop)
	chSvc.SetEnabledChannelsForTest(channels)
	sysSvc := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	modelSvc := biz.NewModelService(biz.ModelServiceParams{Ent: client, ChannelService: chSvc, SystemService: sysSvc})
	keyEnt := &ent.APIKey{ID: 99, ProjectID: 1, Type: apikey.TypeUser, Status: apikey.StatusEnabled, ExposeUpstreamKeyQuota: true, Scopes: []string{"read_channels"}}

	handlers := &OpenAIHandlers{ChannelService: chSvc, ModelService: modelSvc, SystemService: sysSvc, EntClient: client}
	modelCtx := contexts.WithAPIKey(ctx, keyEnt)
	modelObj := &OpenAIModel{ID: "exa"}
	require.NoError(t, handlers.attachOpenAIModelQuota(modelCtx, modelObj, "exa"))
	require.NotNil(t, modelObj.UpstreamQuota)
	require.NotNil(t, modelObj.UpstreamQuota.TotalRemaining)
	require.InDelta(t, 50.0, *modelObj.UpstreamQuota.TotalRemaining, 1e-6, "attachOpenAIModelQuota must deduplicate SharedWallet across linked aliases and ignore Unlimited/FreeTier/%")
	require.Equal(t, "USD", modelObj.UpstreamQuota.Currency)
}
