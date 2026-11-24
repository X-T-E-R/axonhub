package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

func TestBuildQuotaExposureChannel_RedactsProviderAndUnknownComponentData(t *testing.T) {
	rawKey := "tvly-upstream-secret"
	resetAt := time.Now().Add(time.Hour)
	available := true
	used, limit, remaining := 2.0, 10.0, 8.0
	ch := &biz.Channel{Channel: &ent.Channel{
		ID:   7,
		Name: "Tavily",
		Type: channel.TypeOpenai,
		Settings: &objects.ChannelSettings{
			RawHTTP: &objects.ChannelRawHTTP{Enabled: true, Resource: "tavily-search"},
			CredentialPolicies: []objects.ChannelCredentialPolicy{{
				ID:       objects.ChannelAPIKeyFingerprint(rawKey),
				Name:     "safe-name",
				Priority: 3,
				Weight:   25,
			}},
		},
	}}
	item := &biz.ChannelAPIKeyInventoryItem{
		ID:        objects.ChannelAPIKeyFingerprint(rawKey),
		RawKey:    rawKey,
		MaskedKey: objects.MaskChannelAPIKey(rawKey),
		Status:    objects.ChannelKeyStatusActive,
		Policy: &objects.ChannelCredentialPolicy{
			ID:       objects.ChannelAPIKeyFingerprint(rawKey),
			Name:     "safe-name",
			Priority: 3,
			Weight:   25,
		},
		BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
			CheckedAt: time.Now(),
			Success:   true,
			Available: &available,
			Used:      &used,
			Limit:     &limit,
			Remaining: &remaining,
			ResetAt:   &resetAt,
			Components: []objects.BalanceAmount{
				{Amount: 8, Currency: "USD", Kind: objects.BalanceAmountKindRemaining, Label: "remaining"},
				{Amount: 123, Currency: "provider-secret-currency", Kind: objects.BalanceAmountKindUsed, Label: "https://internal.example/key"},
			},
		},
	}

	result := buildQuotaExposureChannel(ch, []*biz.ChannelAPIKeyInventoryItem{item}, time.Now())
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	body := string(encoded)
	require.NotContains(t, body, rawKey)
	require.NotContains(t, body, "provider-secret-currency")
	require.NotContains(t, body, "internal.example")
	require.Contains(t, body, "tavily-search")
	require.Equal(t, "active", result.Keys[0].Status)
	require.Equal(t, 3, result.Keys[0].Priority)
	require.Equal(t, 25, result.Keys[0].Weight)
	require.Equal(t, 8.0, *result.Keys[0].Remaining)
	require.Len(t, result.Keys[0].Components, 2)
	require.Equal(t, "used", result.Keys[0].Components[1].Label)

	cooldown := buildQuotaExposureKey(ch, &biz.ChannelAPIKeyInventoryItem{
		ID:            objects.ChannelAPIKeyFingerprint("cooldown-key"),
		RawKey:        "cooldown-key",
		Status:        objects.ChannelKeyStatusActive,
		CooldownUntil: &resetAt,
		Available:     boolPtr(true),
	}, time.Now())
	require.Equal(t, "cooldown", cooldown.Status)
	require.False(t, *cooldown.Available)
	exhausted := buildQuotaExposureKey(ch, &biz.ChannelAPIKeyInventoryItem{
		ID:     objects.ChannelAPIKeyFingerprint("exhausted-key"),
		RawKey: "exhausted-key",
		Status: objects.ChannelKeyStatusActive,
		BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
			Limit:     floatPtr(1),
			Remaining: floatPtr(0),
			Available: boolPtr(false),
			ResetAt:   &resetAt,
		},
	}, time.Now())
	require.Equal(t, "exhausted", exhausted.Status)
	require.False(t, *exhausted.Available)
}

func TestQuotaRoutes_EnforceToggleAndChannelAuthorization(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:quota-routes?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	makeChannel := func(name, key string) *ent.Channel {
		created, err := client.Channel.Create().
			SetType(channel.TypeOpenai).
			SetName(name).
			SetBaseURL("https://api.example.test/v1").
			SetCredentials(objects.ChannelCredentials{APIKey: key}).
			SetSupportedModels([]string{"gpt-4"}).
			SetDefaultTestModel("gpt-4").
			SetSettings(&objects.ChannelSettings{KeyHealthCheck: &objects.ChannelKeyHealthCheck{KeyMetadata: []objects.ChannelKeyMetadata{{
				ID: objects.ChannelAPIKeyFingerprint(key),
				BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
					CheckedAt: time.Now(),
					Remaining: floatPtr(4),
					Limit:     floatPtr(10),
					Used:      floatPtr(6),
					Available: boolPtr(true),
				},
			}}}}).
			SetStatus(channel.StatusEnabled).
			Save(ctx)
		require.NoError(t, err)
		return created
	}
	allowed := makeChannel("allowed", "allowed-upstream-key")
	blocked := makeChannel("blocked", "blocked-upstream-key")

	channelService := biz.NewChannelServiceForTest(client)
	t.Cleanup(channelService.Stop)
	channelService.SetEnabledChannelsForTest([]*biz.Channel{{Channel: allowed}, {Channel: blocked}})

	apiKey := &ent.APIKey{
		ID:                     42,
		ProjectID:              1,
		Type:                   apikey.TypeUser,
		Status:                 apikey.StatusEnabled,
		Scopes:                 []string{"write_requests"},
		ExposeUpstreamKeyQuota: true,
		Profiles: &objects.APIKeyProfiles{ActiveProfile: "default", Profiles: []objects.APIKeyProfile{{
			Name:       "default",
			ChannelIDs: []int{allowed.ID},
			ModelIDs:   []string{"gpt-4"},
		}}},
	}
	requestContext := func() context.Context {
		requestCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
		return contexts.WithAPIKey(requestCtx, apiKey)
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(requestContext())
		c.Next()
	})
	handlers := &OpenAIHandlers{ChannelService: channelService}
	router.GET("/v1/quotas", handlers.ListQuotas)
	router.GET("/v1/key-quotas", handlers.KeyQuotas)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/quotas?model=gpt-4", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response QuotaExposureResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.True(t, response.Exposed)
	require.Equal(t, 1, response.Summary.AuthorizedChannels)
	require.Len(t, response.Data, 1)
	require.Equal(t, allowed.ID, response.Data[0].ChannelID)
	require.NotContains(t, rec.Body.String(), "blocked-upstream-key")
	require.NotContains(t, rec.Body.String(), "allowed-upstream-key")
	require.Equal(t, objects.ChannelAPIKeyFingerprint("allowed-upstream-key"), response.Data[0].Keys[0].ID)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/key-quotas", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	apiKey.ExposeUpstreamKeyQuota = false
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/quotas", nil))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "key_quota_exposure_disabled")
}

func TestOpenAIModelIncludeQuotas_RespectsAuthorizationAndToggle(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:model-quota?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	channelEntity, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("model-quota-channel").
		SetBaseURL("https://api.example.test/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "model-upstream-key"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{KeyHealthCheck: &objects.ChannelKeyHealthCheck{KeyMetadata: []objects.ChannelKeyMetadata{{
			ID:              objects.ChannelAPIKeyFingerprint("model-upstream-key"),
			BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{Remaining: floatPtr(7), Limit: floatPtr(10), Used: floatPtr(3), Available: boolPtr(true)},
		}}}}).
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.Model.Create().
		SetDeveloper("openai").
		SetModelID("gpt-4").
		SetName("GPT-4").
		SetIcon("test").
		SetGroup("test").
		SetType(model.TypeChat).
		SetModelCard(&objects.ModelCard{}).
		SetStatus(model.StatusEnabled).
		SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{{
			Type:         "channel_model",
			ChannelModel: &objects.ChannelModelAssociation{ChannelID: channelEntity.ID, ModelID: "gpt-4"},
		}}}).
		Save(ctx)
	require.NoError(t, err)

	channelService := biz.NewChannelServiceForTest(client)
	t.Cleanup(channelService.Stop)
	channelService.SetEnabledChannelsForTest([]*biz.Channel{{Channel: channelEntity}})
	systemService := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	modelService := biz.NewModelService(biz.ModelServiceParams{Ent: client, ChannelService: channelService, SystemService: systemService})
	apiKey := &ent.APIKey{ID: 99, ProjectID: 1, Type: apikey.TypeUser, Status: apikey.StatusEnabled, ExposeUpstreamKeyQuota: true, Scopes: []string{"read_channels"}}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		requestCtx := authz.WithTestBypass(ent.NewContext(c.Request.Context(), client))
		requestCtx = contexts.WithAPIKey(requestCtx, apiKey)
		c.Request = c.Request.WithContext(requestCtx)
		c.Next()
	})
	handlers := &OpenAIHandlers{ChannelService: channelService, ModelService: modelService, SystemService: systemService, EntClient: client}
	router.GET("/v1/models", handlers.ListModels)
	router.GET("/v1/models/*model", handlers.RetrieveModel)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?include=quotas", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Data []OpenAIModel `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Data, 1)
	require.NotNil(t, list.Data[0].UpstreamQuota)
	require.Equal(t, 1, list.Data[0].UpstreamQuota.TotalKeys)
	require.Len(t, list.Data[0].UpstreamQuota.Channels, 1)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4?include=all", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var modelResponse OpenAIModel
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &modelResponse))
	require.NotNil(t, modelResponse.UpstreamQuota)
	require.Len(t, modelResponse.UpstreamQuota.Channels, 1)

	apiKey.ExposeUpstreamKeyQuota = false
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?include=all", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list = struct {
		Data []OpenAIModel `json:"data"`
	}{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Data, 1)
	require.Nil(t, list.Data[0].UpstreamQuota)
	require.NotContains(t, rec.Body.String(), "upstream_quota")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4?include=all", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	modelResponse = OpenAIModel{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &modelResponse))
	require.Nil(t, modelResponse.UpstreamQuota)
	require.NotContains(t, rec.Body.String(), "upstream_quota")
}

func TestManagementInventoryProjection_IncludesSafeBalanceSnapshotAndPolicy(t *testing.T) {
	policy := &objects.ChannelCredentialPolicy{
		ID:              objects.ChannelAPIKeyFingerprint("upstream-secret-key"),
		Name:            "key alias",
		Priority:        2,
		Weight:          40,
		EgressMode:      objects.CredentialEgressModeCustomURL,
		ProxyPoolName:   "private-proxy-pool",
		OutboundProxyID: intPtr(17),
		CustomProxy:     &objects.ProxyConfig{Type: objects.ProxyType("url"), URL: "http://proxy.example:3128", Username: "proxy-user", Password: "proxy-password"},
	}
	snapshot := &objects.ChannelKeyBalanceSnapshot{
		Provider:      "provider-internal-name",
		CheckedAt:     time.Now(),
		Success:       true,
		AccountID:     "provider-account-secret",
		AccountStatus: "private-account-state",
		RawSummary:    map[string]any{"source_secret": "must-not-appear"},
		Remaining:     floatPtr(12),
		Limit:         floatPtr(20),
		Used:          floatPtr(8),
		Components: []objects.BalanceAmount{{
			Amount:   12,
			Currency: "internal-currency-name",
			Kind:     objects.BalanceAmountKindRemaining,
			Label:    "https://provider.example/private",
		}},
	}
	items := managementInventoryItemsResponse([]*biz.ChannelAPIKeyInventoryItem{{
		ID:              policy.ID,
		RawKey:          "upstream-secret-key",
		MaskedKey:       "upst****-key",
		Status:          objects.ChannelKeyStatusActive,
		BalanceSnapshot: snapshot,
		Policy:          policy,
	}})
	require.Len(t, items, 1)
	encoded, err := json.Marshal(items[0])
	require.NoError(t, err)
	body := string(encoded)
	require.Contains(t, body, "balanceSnapshot")
	require.Contains(t, body, "policy")
	require.NotContains(t, body, "provider-internal-name")
	require.NotContains(t, body, "provider-account-secret")
	require.NotContains(t, body, "private-account-state")
	require.NotContains(t, body, "source_secret")
	require.NotContains(t, body, "internal-currency-name")
	require.NotContains(t, body, "provider.example")
	require.NotContains(t, body, "proxy.example")
	require.NotContains(t, body, "proxy-password")
	require.NotContains(t, body, "private-proxy-pool")
	require.Equal(t, "key alias", items[0].Policy.Name)
	require.Nil(t, items[0].Policy.CustomProxy)
	require.Nil(t, items[0].Policy.OutboundProxyID)
	require.Empty(t, items[0].Policy.ProxyPoolName)
	require.Len(t, items[0].BalanceSnapshot.Components, 1)
	require.Empty(t, items[0].BalanceSnapshot.Components[0].Currency)
	require.Equal(t, "remaining", items[0].BalanceSnapshot.Components[0].Label)
}

func TestQuotaExposure_LegacyOpenRouterCh45FreeTierReadNormalization(t *testing.T) {
	checkedAt := time.Date(2026, 10, 15, 8, 30, 0, 0, time.UTC)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ch := &biz.Channel{
		Channel: &ent.Channel{
			ID:      45,
			Name:    "openrouter-free-pool",
			Type:    channel.TypeOpenrouter,
			BaseURL: "https://openrouter.ai/api/v1",
			Settings: &objects.ChannelSettings{
				BalanceProbe: &objects.ChannelBalanceProbe{
					Enabled: false,
					Preset:  objects.ChannelBalanceProbePresetOpenRouter,
				},
			},
		},
	}
	legacySnap := &objects.ChannelKeyBalanceSnapshot{
		Success:    true,
		StatusCode: http.StatusOK,
		CheckedAt:  checkedAt,
		Available:  boolPtr(false),
		Limit:      floatPtr(0),
		Used:       floatPtr(0.42),
		Remaining:  floatPtr(-0.42),
		RawSummary: map[string]any{
			"total_credits": 0.0,
			"total_usage":   0.42,
		},
		PrimaryBalance: &objects.BalanceAmount{
			Amount:   -0.42,
			Currency: "USD",
			Kind:     objects.BalanceAmountKindRemaining,
			Label:    "remaining credits",
		},
		Components: []objects.BalanceAmount{
			{Amount: 0, Currency: "USD", Kind: objects.BalanceAmountKindTotal, Label: "total credits"},
			{Amount: 0.42, Currency: "USD", Kind: objects.BalanceAmountKindUsed, Label: "total usage"},
			{Amount: -0.42, Currency: "USD", Kind: objects.BalanceAmountKindRemaining, Label: "remaining credits"},
		},
	}

	entry := buildQuotaExposureChannel(ch, []*biz.ChannelAPIKeyInventoryItem{{
		ID:              objects.ChannelAPIKeyFingerprint("sk-or-v1-secret"),
		RawKey:          "sk-or-v1-secret",
		MaskedKey:       "sk-o****cret",
		Status:          objects.ChannelKeyStatusActive,
		Balance:         -0.42,
		Currency:        "USD",
		Available:       boolPtr(false),
		BalanceSnapshot: legacySnap,
	}}, now)

	require.Len(t, entry.Keys, 1)
	key := entry.Keys[0]
	require.True(t, key.FreeTier, "legacy OpenRouter Ch 45 snapshot must normalize to FreeTier=true")
	require.Equal(t, "active", key.Status, "normalized free-tier key must not show as exhausted")
	require.NotNil(t, key.Available)
	require.True(t, *key.Available)
	require.NotNil(t, key.Remaining)
	require.InDelta(t, 0.0, *key.Remaining, 1e-9)
	require.NotNil(t, key.Used)
	require.InDelta(t, 0.42, *key.Used, 1e-9)
	require.NotNil(t, key.CheckedAt)
	require.Equal(t, checkedAt, *key.CheckedAt, "original checkedAt must be preserved")
}

func intPtr(value int) *int { return &value }
