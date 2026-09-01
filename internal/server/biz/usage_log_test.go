package biz

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm"
)

func TestUsageLogService_CreateUsageLog_PromptWriteCachedTokens(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	p, err := client.Project.Create().
		SetName("test-project").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	req, err := client.Request.Create().
		SetProjectID(p.ID).
		SetModelID("test-model").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{},
		Ent:         client,
	})
	channelService := NewChannelServiceForTest(client)
	svc := NewUsageLogService(client, systemService, channelService)

	usage := &llm.Usage{
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		PromptTokensDetails: &llm.PromptTokensDetails{
			CachedTokens:      2,
			WriteCachedTokens: 3,
		},
	}

	created, err := svc.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     req.ID,
		ProjectID:     p.ID,
		ChannelID:     0,
		ActualModelID: "test-model",
		Usage:         usage,
		Source:        usagelog.SourceAPI,
		Format:        "openai/chat_completions",
		APIKeyID:      nil,
	})
	require.NoError(t, err)
	require.NotNil(t, created)

	require.Equal(t, int64(2), created.PromptCachedTokens)
	require.Equal(t, int64(3), created.PromptWriteCachedTokens)
}

func TestUsageLogService_CreateUsageLog_WithPriceReferenceID(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create project
	p, err := client.Project.Create().
		SetName("test-project").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	// Create channel
	ch, err := client.Channel.Create().
		SetName("test-channel").
		SetType("openai").
		SetBaseURL("https://api.openai.com/v1").
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		Save(ctx)
	require.NoError(t, err)

	// Create model price with reference ID
	_, err = client.ChannelModelPrice.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetReferenceID("test-ref-123").
		SetPrice(objects.ModelPrice{
			Items: []objects.ModelPriceItem{
				{
					ItemCode: objects.PriceItemCodeUsage,
					Pricing: objects.Pricing{
						Mode:         objects.PricingModeUsagePerUnit,
						UsagePerUnit: toDecimalPtr("0.03"),
					},
				},
				{
					ItemCode: objects.PriceItemCodeCompletion,
					Pricing: objects.Pricing{
						Mode:         objects.PricingModeUsagePerUnit,
						UsagePerUnit: toDecimalPtr("0.06"),
					},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	// Create request
	req, err := client.Request.Create().
		SetProjectID(p.ID).
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{},
		Ent:         client,
	})
	require.NoError(t, systemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody:            true,
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(2),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))
	initialCharge := int64(2<<20) - 1
	client.ManagedObservabilityState.UpdateOneID(1).
		SetChargedBytes(initialCharge).
		SetUnderPressure(false).
		SetLastError("capacity_reconciliation_pending").
		ExecX(ctx)
	channelService := NewChannelServiceForTest(client)

	// Preload the channel with model prices
	enabledCh, err := channelService.buildChannelWithTransformer(ch)
	require.NoError(t, err)
	channelService.preloadModelPrices(ctx, enabledCh)

	// Add to enabled channels list so it can be found by GetEnabledChannel
	channelService.SetEnabledChannelsForTest([]*Channel{enabledCh})

	// Verify cache contains the model price
	require.NotNil(t, enabledCh.cachedModelPrices["gpt-4"])
	require.Equal(t, "test-ref-123", enabledCh.cachedModelPrices["gpt-4"].ReferenceID)

	svc := NewUsageLogService(client, systemService, channelService)
	callbackCount := 0
	svc.OnUsageLogCreated = func() { callbackCount++ }

	// Create usage log with price calculation
	usage := &llm.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	channelID := ch.ID
	created, err := svc.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     req.ID,
		ProjectID:     p.ID,
		ChannelID:     channelID,
		ActualModelID: "gpt-4",
		Usage:         usage,
		Source:        usagelog.SourceAPI,
		Format:        "openai/chat_completions",
		APIKeyID:      nil,
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Equal(t, int64(1000), created.PromptTokens)
	require.Equal(t, int64(500), created.CompletionTokens)
	require.Equal(t, int64(1500), created.TotalTokens)
	require.Equal(t, 1, callbackCount)

	// Verify price_reference_id is set
	require.Equal(t, "test-ref-123", created.CostPriceReferenceID)
	require.NotNil(t, created.TotalCost)
	require.NotEmpty(t, created.CostItems)

	// Verify cost calculation is correct
	// (1000 / 1_000_000) * 0.03 + (500 / 1_000_000) * 0.06 = 0.00003 + 0.00003 = 0.00006
	require.InDelta(t, 0.00006, *created.TotalCost, 0.0000001)

	costItemsJSON, err := json.Marshal(created.CostItems)
	require.NoError(t, err)
	expectedCharge := int64(4<<10 + len(created.ModelID) + len(created.Format) + len(created.CostPriceReferenceID) + len(costItemsJSON))
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.Equal(t, initialCharge+expectedCharge, state.ChargedBytes)
	require.True(t, state.UnderPressure)
	require.Equal(t, "capacity_reconciliation_pending", state.LastError)
	require.True(t, client.Request.GetX(ctx, req.ID).ManagedObservability)
}

func TestUsageLogService_CreateUsageLog_ManagedMarkFailurePreservesCoreRow(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:usage-mark-degraded?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proj := client.Project.Create().SetName("usage-mark-degraded").SetStatus(project.StatusActive).SaveX(ctx)
	system := NewSystemService(SystemServiceParams{Ent: client})
	require.NoError(t, system.SetStoragePolicy(ctx, &StoragePolicy{
		ManagedObservabilityHardMiB: lo.ToPtr(2),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))
	service := NewUsageLogService(client, system, NewChannelServiceForTest(client))
	callbackCount := 0
	service.OnUsageLogCreated = func() { callbackCount++ }

	created, err := service.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID: 9999, ProjectID: proj.ID, ChannelID: 0,
		ActualModelID: "model", Usage: &llm.Usage{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9},
		Source: usagelog.SourceAPI, Format: "openai/chat_completions",
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Equal(t, int64(9), created.TotalTokens)
	require.Equal(t, 1, callbackCount)
	require.Equal(t, 1, client.UsageLog.Query().CountX(ctx))
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.True(t, state.UnderPressure)
	require.Equal(t, "usage_group_mark:failed", state.LastError)
}

func TestUsageLogService_CreateUsageLog_AccountingFailurePreservesCoreRow(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:usage-accounting-degraded?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proj := client.Project.Create().SetName("usage-accounting-degraded").SetStatus(project.StatusActive).SaveX(ctx)
	parent := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("model").
		SetRequestBody(objects.JSONRawMessage(`{}`)).
		SetStatus(request.StatusCompleted).
		SaveX(ctx)

	accountingClient := enttest.NewEntClient(t, "sqlite3", "file:usage-accounting-unavailable?mode=memory&_fk=0")
	require.NoError(t, accountingClient.Close())
	system := NewSystemService(SystemServiceParams{Ent: accountingClient})
	service := NewUsageLogService(client, system, NewChannelServiceForTest(client))
	callbackCount := 0
	service.OnUsageLogCreated = func() { callbackCount++ }

	created, err := service.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID: parent.ID, ProjectID: proj.ID, ChannelID: 0,
		ActualModelID: "model", Usage: &llm.Usage{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9},
		Source: usagelog.SourceAPI, Format: "openai/chat_completions",
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Equal(t, int64(9), created.TotalTokens)
	require.Equal(t, 1, callbackCount)
	require.Equal(t, 1, client.UsageLog.Query().CountX(ctx))
}

func TestUsageLogService_CreateUsageLog_InsertFailureReturnsErrorWithoutCallback(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:usage-insert-failure?mode=memory&_fk=1")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proj := client.Project.Create().SetName("usage-insert-failure").SetStatus(project.StatusActive).SaveX(ctx)
	channel := client.Channel.Create().
		SetType(entchannel.TypeOpenai).
		SetName("usage-insert-failure").
		SetCredentials(objects.ChannelCredentials{}).
		SetSupportedModels([]string{"model"}).
		SetDefaultTestModel("model").
		SaveX(ctx)
	parent := client.Request.Create().
		SetProjectID(proj.ID).
		SetChannelID(channel.ID).
		SetModelID("model").
		SetRequestBody(objects.JSONRawMessage(`{}`)).
		SetStatus(request.StatusCompleted).
		SaveX(ctx)
	system := NewSystemService(SystemServiceParams{Ent: client})
	service := NewUsageLogService(client, system, NewChannelServiceForTest(client))
	callbackCount := 0
	service.OnUsageLogCreated = func() { callbackCount++ }

	created, err := service.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID: parent.ID, ProjectID: proj.ID, ChannelID: channel.ID,
		ActualModelID: "model", Usage: &llm.Usage{TotalTokens: 9},
		Source: usagelog.Source("invalid"), Format: "openai/chat_completions",
	})
	require.Error(t, err)
	require.Nil(t, created)
	require.Zero(t, callbackCount)
	require.Zero(t, client.UsageLog.Query().CountX(ctx))
}

func TestUsageLogService_CreateUsageLog_WithCachedTokens(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create project
	p, err := client.Project.Create().
		SetName("test-project").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	// Create channel
	ch, err := client.Channel.Create().
		SetName("test-channel").
		SetType("openai").
		SetBaseURL("https://api.openai.com/v1").
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		Save(ctx)
	require.NoError(t, err)

	// Create model price with reference ID
	// Input tokens: $0.03 per 1M tokens
	// Completion tokens: $0.06 per 1M tokens
	// Cached tokens: $0.015 per 1M tokens (50% discount)
	_, err = client.ChannelModelPrice.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetReferenceID("test-ref-cached").
		SetPrice(objects.ModelPrice{
			Items: []objects.ModelPriceItem{
				{
					ItemCode: objects.PriceItemCodeUsage,
					Pricing: objects.Pricing{
						Mode:         objects.PricingModeUsagePerUnit,
						UsagePerUnit: toDecimalPtr("0.03"),
					},
				},
				{
					ItemCode: objects.PriceItemCodeCompletion,
					Pricing: objects.Pricing{
						Mode:         objects.PricingModeUsagePerUnit,
						UsagePerUnit: toDecimalPtr("0.06"),
					},
				},
				{
					ItemCode: objects.PriceItemCodePromptCachedToken,
					Pricing: objects.Pricing{
						Mode:         objects.PricingModeUsagePerUnit,
						UsagePerUnit: toDecimalPtr("0.015"),
					},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	// Create request
	req, err := client.Request.Create().
		SetProjectID(p.ID).
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{},
		Ent:         client,
	})
	channelService := NewChannelServiceForTest(client)

	// Preload the channel with model prices
	enabledCh, err := channelService.buildChannelWithTransformer(ch)
	require.NoError(t, err)
	channelService.preloadModelPrices(ctx, enabledCh)

	// Add to enabled channels list so it can be found by GetEnabledChannel
	channelService.SetEnabledChannelsForTest([]*Channel{enabledCh})

	// Verify cache contains the model price
	require.NotNil(t, enabledCh.cachedModelPrices["gpt-4"])
	require.Equal(t, "test-ref-cached", enabledCh.cachedModelPrices["gpt-4"].ReferenceID)

	svc := NewUsageLogService(client, systemService, channelService)

	// Create usage log with cached tokens
	// Total prompt tokens: 1000 (includes 300 cached tokens)
	// Billable prompt tokens: 700 (1000 - 300)
	// Cached tokens: 300 (read from cache, charged at discounted rate)
	// Completion tokens: 500
	usage := &llm.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		PromptTokensDetails: &llm.PromptTokensDetails{
			CachedTokens: 300,
		},
	}

	channelID := ch.ID
	created, err := svc.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     req.ID,
		ProjectID:     p.ID,
		ChannelID:     channelID,
		ActualModelID: "gpt-4",
		Usage:         usage,
		Source:        usagelog.SourceAPI,
		Format:        "openai/chat_completions",
		APIKeyID:      nil,
	})
	require.NoError(t, err)
	require.NotNil(t, created)

	// Verify price_reference_id is set
	require.Equal(t, "test-ref-cached", created.CostPriceReferenceID)
	require.NotNil(t, created.TotalCost)
	require.NotEmpty(t, created.CostItems)

	// Verify cost calculation excludes cached tokens from input cost
	// Expected cost:
	// - Input tokens (billable): (700 / 1_000_000) * 0.03 = 0.000021
	// - Cached tokens: (300 / 1_000_000) * 0.015 = 0.0000045
	// - Completion tokens: (500 / 1_000_000) * 0.06 = 0.00003
	// Total: 0.000021 + 0.0000045 + 0.00003 = 0.0000555
	expectedCost := 0.0000555
	require.InDelta(t, expectedCost, *created.TotalCost, 0.0000001)

	// Verify cost items breakdown
	require.Len(t, created.CostItems, 3)

	// Find each cost item and verify
	var inputItem, cachedItem, completionItem *objects.CostItem

	for i := range created.CostItems {
		switch created.CostItems[i].ItemCode {
		case objects.PriceItemCodeUsage:
			inputItem = &created.CostItems[i]
		case objects.PriceItemCodePromptCachedToken:
			cachedItem = &created.CostItems[i]
		case objects.PriceItemCodeCompletion:
			completionItem = &created.CostItems[i]
		}
	}

	require.NotNil(t, inputItem, "input cost item should exist")
	require.NotNil(t, cachedItem, "cached cost item should exist")
	require.NotNil(t, completionItem, "completion cost item should exist")

	// Verify input tokens quantity excludes cached tokens
	require.Equal(t, int64(700), inputItem.Quantity, "input quantity should be 700 (1000 - 300 cached)")
	require.InDelta(t, 0.000021, inputItem.Subtotal.InexactFloat64(), 0.0000001)

	// Verify cached tokens quantity
	require.Equal(t, int64(300), cachedItem.Quantity, "cached quantity should be 300")
	require.InDelta(t, 0.0000045, cachedItem.Subtotal.InexactFloat64(), 0.0000001)

	// Verify completion tokens quantity
	require.Equal(t, int64(500), completionItem.Quantity, "completion quantity should be 500")
	require.InDelta(t, 0.00003, completionItem.Subtotal.InexactFloat64(), 0.0000001)
}

func toDecimalPtr(s string) *decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return &d
}
