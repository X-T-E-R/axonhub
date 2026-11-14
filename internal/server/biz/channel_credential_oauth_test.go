package biz

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

var oauthCredentialFixtureID atomic.Uint64

func setupOAuthCredentialChannel(t *testing.T, svc *ChannelService, client *ent.Client, settings *objects.ChannelSettings) *Channel {
	t.Helper()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	entCh, err := client.Channel.Create().
		SetType(channel.TypeCodex).
		SetName(fmt.Sprintf("%s-oauth-%d", t.Name(), oauthCredentialFixtureID.Add(1))).
		SetBaseURL("https://chatgpt.com/backend-api/codex").
		SetCredentials(objects.ChannelCredentials{
			OAuth: &objects.OAuthCredentials{
				AccessToken:  "synthetic-oauth-access",
				RefreshToken: "synthetic-oauth-refresh",
				ExpiresAt:    time.Now().Add(24 * time.Hour),
			},
		}).
		SetSupportedModels([]string{"gpt-5"}).
		SetDefaultTestModel("gpt-5").
		SetStatus(channel.StatusEnabled).
		SetSettings(settings).
		Save(ctx)
	require.NoError(t, err)

	built, err := svc.buildChannelWithOutbounds(entCh)
	require.NoError(t, err)
	if built.stopTokenProvider != nil {
		t.Cleanup(built.stopTokenProvider)
	}
	return built
}

func TestChannel_SelectOAuthCredentialEgress_MaxConcurrentAndFinishRelease(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()

	// Legacy OAuth channel without credential policies returns nil without claiming a slot.
	legacyCh := setupOAuthCredentialChannel(t, svc, client, nil)
	legacyCtx := contexts.EnsureContainer(context.Background())
	require.NoError(t, legacyCh.SelectOAuthCredentialEgress(legacyCtx))
	_, legacyHasClient := contexts.GetChannelCredentialEgress(legacyCtx)
	require.False(t, legacyHasClient)

	// OAuthDefault with maxConcurrent=1 admits one in-flight request, denies the second,
	// and admits again once Finish releases the slot.
	ch := setupOAuthCredentialChannel(t, svc, client, &objects.ChannelSettings{
		CredentialPolicies: []objects.ChannelCredentialPolicy{
			{
				ID:            objects.OAuthDefaultCredentialID,
				Name:          "oauth-primary",
				MaxConcurrent: lo.ToPtr(1),
				EgressMode:    objects.CredentialEgressModeDirect,
			},
		},
	})

	ctx1 := contexts.EnsureContainer(context.Background())
	require.NoError(t, ch.SelectOAuthCredentialEgress(ctx1))
	c1, ok1 := contexts.GetChannelCredentialEgress(ctx1)
	require.True(t, ok1)
	require.NotNil(t, c1)

	ctx2 := contexts.EnsureContainer(context.Background())
	err := ch.SelectOAuthCredentialEgress(ctx2)
	require.ErrorContains(t, err, "no available admission slot")
	_, ok2 := contexts.GetChannelCredentialEgress(ctx2)
	require.False(t, ok2)

	contexts.FinishChannelCredentialAttempt(ctx1, http.StatusOK, false, false)

	ctx3 := contexts.EnsureContainer(context.Background())
	require.NoError(t, ch.SelectOAuthCredentialEgress(ctx3))
	c3, ok3 := contexts.GetChannelCredentialEgress(ctx3)
	require.True(t, ok3)
	require.NotNil(t, c3)
	contexts.FinishChannelCredentialAttempt(ctx3, http.StatusOK, false, false)
}

func TestChannel_SelectOAuthCredentialEgress_SharedPinnedProxyMaxConcurrentAcrossOAuthAndAPIKey(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proxySvc := NewOutboundProxyService(OutboundProxyServiceParams{Ent: client})
	sharedProxy, err := proxySvc.Create(ctx, OutboundProxyCreateInput{
		Name:          "shared-slot-1-proxy",
		Kind:          OutboundProxyKindStatic,
		ProxyURL:      "http://shared-proxy.example.com:8080",
		Status:        OutboundProxyStatusEnabled,
		MaxConcurrent: lo.ToPtr(1),
	})
	require.NoError(t, err)

	oauthCh := setupOAuthCredentialChannel(t, svc, client, &objects.ChannelSettings{
		CredentialPolicies: []objects.ChannelCredentialPolicy{
			{
				ID:               objects.OAuthDefaultCredentialID,
				Name:             "oauth-pinned",
				EgressMode:       objects.CredentialEgressModePinnedProxy,
				OutboundProxyID:  lo.ToPtr(sharedProxy.ID),
				EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
			},
		},
	})

	apiKey := "sk-shared-proxy-api-key"
	entAPICh, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName(fmt.Sprintf("api-cred-%d", time.Now().UnixNano())).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{apiKey}}).
		SetSupportedModels([]string{"gpt-4o"}).
		SetDefaultTestModel("gpt-4o").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:               objects.ChannelAPIKeyFingerprint(apiKey),
					Name:             "api-pinned",
					EgressMode:       objects.CredentialEgressModePinnedProxy,
					OutboundProxyID:  lo.ToPtr(sharedProxy.ID),
					EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	apiCh, err := svc.buildChannelWithOutbounds(entAPICh)
	require.NoError(t, err)
	apiProvider := getAPIKeyProvider(apiCh)

	// 1. OAuth acquires the single proxy slot -> API-key channel is denied while OAuth holds it.
	oauthCtx1 := contexts.EnsureContainer(context.Background())
	require.NoError(t, oauthCh.SelectOAuthCredentialEgress(oauthCtx1))
	oauthClient, oauthOK := contexts.GetChannelCredentialEgress(oauthCtx1)
	require.True(t, oauthOK)
	require.NotNil(t, oauthClient)

	apiCtxDenied := contexts.EnsureContainer(context.Background())
	require.Empty(t, apiProvider.Get(apiCtxDenied))

	// Release OAuth -> API-key channel now acquires the shared proxy slot.
	contexts.FinishChannelCredentialAttempt(oauthCtx1, http.StatusOK, false, false)

	apiCtxHeld := contexts.EnsureContainer(context.Background())
	require.Equal(t, apiKey, apiProvider.Get(apiCtxHeld))
	apiClient, apiOK := contexts.GetChannelCredentialEgress(apiCtxHeld)
	require.True(t, apiOK)
	require.NotNil(t, apiClient)
	require.NotSame(t, oauthClient, apiClient)

	// 2. While API-key holds the shared proxy slot -> OAuth is denied until API-key Finishes.
	oauthCtxDenied := contexts.EnsureContainer(context.Background())
	err = oauthCh.SelectOAuthCredentialEgress(oauthCtxDenied)
	require.ErrorContains(t, err, "no available admission slot")

	contexts.FinishChannelCredentialAttempt(apiCtxHeld, http.StatusOK, false, false)

	oauthCtx2 := contexts.EnsureContainer(context.Background())
	require.NoError(t, oauthCh.SelectOAuthCredentialEgress(oauthCtx2))
	contexts.FinishChannelCredentialAttempt(oauthCtx2, http.StatusOK, false, false)
}
