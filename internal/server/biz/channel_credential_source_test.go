package biz

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestCredentialSourceLiveCacheAndSharedRuntime(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	cache := xcache.Config{Mode: xcache.ModeMemory}
	sys := NewSystemService(SystemServiceParams{Ent: db, CacheConfig: cache})
	svc := NewChannelService(ChannelServiceParams{Ent: db, CacheConfig: cache, SystemService: sys, HttpClient: httpclient.NewHttpClient()})
	t.Cleanup(func() { svc.Stop(); _ = db.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	keyID := objects.ChannelAPIKeyFingerprint("key-a")
	source := db.Channel.Create().SetName("holder").SetType(channel.TypeOpenai).
		SetBaseURL("https://holder.example/v1").SetStatus(channel.StatusDisabled).
		SetSupportedModels([]string{"other-model"}).SetDefaultTestModel("other-model").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"key-a", "key-b"}}).
		SetSettings(&objects.ChannelSettings{CredentialPolicies: []objects.ChannelCredentialPolicy{{ID: keyID, Priority: 10, MaxConcurrent: lo.ToPtr(1)}}}).SaveX(ctx)
	dependent, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Name: "responses", Type: channel.TypeOpenaiResponses, BaseURL: lo.ToPtr("https://dependent.example/api"),
		SupportedModels: []string{"sibling-model"}, DefaultTestModel: "sibling-model",
		Settings: &objects.ChannelSettings{
			CredentialSourceChannelID: &source.ID, Proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
			ModelMappings: []objects.ModelMapping{{From: "alias", To: "sibling-model"}},
		},
	})
	require.NoError(t, err)
	require.Empty(t, dependent.Credentials.GetAllAPIKeys())
	dependent, err = svc.UpdateChannelStatus(ctx, dependent.ID, channel.StatusEnabled)
	require.NoError(t, err)
	require.NoError(t, svc.ReloadEnabledChannelsCache(ctx))
	require.Nil(t, svc.GetEnabledChannel(source.ID))
	cached := svc.GetEnabledChannel(dependent.ID)
	require.NotNil(t, cached)
	require.Equal(t, source.ID, cached.CredentialOwnerID())
	require.Equal(t, dependent.Type, cached.Type)
	require.Equal(t, dependent.BaseURL, cached.BaseURL)
	require.Equal(t, dependent.SupportedModels, cached.SupportedModels)
	require.Equal(t, dependent.Settings.Proxy, cached.Settings.Proxy)
	model, err := cached.ChooseModel("alias")
	require.NoError(t, err)
	require.Equal(t, "sibling-model", model)
	owner, err := svc.GetChannel(ctx, source.ID)
	require.NoError(t, err)

	a := contexts.EnsureContainer(ctx)
	require.Equal(t, "key-a", getAPIKeyProvider(owner).Get(a))
	items, err := svc.ChannelAPIKeyInventory(ctx, dependent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, items[0].InFlight)
	b := contexts.EnsureContainer(ctx)
	require.Equal(t, "key-b", getAPIKeyProvider(cached).Get(b), "source slot is occupied")
	contexts.FinishChannelCredentialAttempt(b, http.StatusOK, false, false)
	contexts.FinishChannelCredentialAttempt(a, http.StatusTooManyRequests, true, false)
	b = contexts.EnsureContainer(ctx)
	require.Equal(t, "key-b", getAPIKeyProvider(cached).Get(b), "source key is cooling down")
	contexts.FinishChannelCredentialAttempt(b, http.StatusOK, false, false)
	items, err = svc.ChannelAPIKeyInventory(ctx, dependent.ID)
	require.NoError(t, err)
	require.NotNil(t, items[0].CooldownUntil)

	require.NoError(t, svc.AddChannelAPIKey(ctx, source.ID, "key-new"))
	newKeyID := objects.ChannelAPIKeyFingerprint("key-new")
	_, err = svc.UpsertChannelCredentialPolicy(ctx, source.ID, objects.ChannelCredentialPolicy{ID: newKeyID, Priority: 20})
	require.NoError(t, err)
	require.NoError(t, svc.ReloadEnabledChannelsCache(ctx))
	current := svc.GetEnabledChannel(dependent.ID)
	sourceAfter := db.Channel.GetX(ctx, source.ID)
	require.NotSame(t, cached, current)
	require.True(t, sourceAfter.UpdatedAt.After(source.UpdatedAt))
	require.Equal(t, sourceAfter.UpdatedAt, current.credentialSourceUpdatedAt)
	require.Equal(t, dependent.UpdatedAt, current.UpdatedAt)
	b = contexts.EnsureContainer(ctx)
	require.Equal(t, "key-new", getAPIKeyProvider(current).Get(b))
	contexts.FinishChannelCredentialAttempt(b, http.StatusOK, false, false)

	require.NoError(t, svc.DisableAPIKey(ctx, dependent.ID, "key-new", http.StatusUnauthorized, "runtime test"))
	require.True(t, svc.GetEnabledChannel(dependent.ID).IsAPIKeyDisabled("key-new"))
	require.Empty(t, db.Channel.GetX(ctx, dependent.ID).DisabledAPIKeys)
	require.Len(t, db.Channel.GetX(ctx, source.ID).DisabledAPIKeys, 1)
}

func TestCredentialSourceRawHTTPRefreshesPolicyWithoutDependentWrite(t *testing.T) {
	svc, db := setupTestChannelService(t)
	t.Cleanup(func() { svc.Stop(); _ = db.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	source := db.Channel.Create().SetName("llm-holder").SetType(channel.TypeOpenaiResponses).
		SetBaseURL("https://holder.example/v1").SetStatus(channel.StatusDisabled).
		SetSupportedModels([]string{"chat"}).SetDefaultTestModel("chat").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"first", "second"}}).
		SetSettings(&objects.ChannelSettings{CredentialPolicies: []objects.ChannelCredentialPolicy{{ID: objects.ChannelAPIKeyFingerprint("first"), Priority: 10}}}).SaveX(ctx)
	dependent, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Name: "raw-dependent", Type: channel.TypeOpenai, BaseURL: lo.ToPtr("https://raw.example/provider"),
		SupportedModels: []string{"raw"}, DefaultTestModel: "raw",
		Settings: &objects.ChannelSettings{CredentialSourceChannelID: &source.ID, RawHTTP: &objects.ChannelRawHTTP{Enabled: true, AuthType: "bearer", Resource: "raw"}},
	})
	require.NoError(t, err)
	dependent, err = svc.UpdateChannelStatus(ctx, dependent.ID, channel.StatusEnabled)
	require.NoError(t, err)
	open := func() *Channel {
		t.Helper()
		row, err := svc.ResolveRawHTTPChannel(ctx, dependent.ID)
		require.NoError(t, err)
		ch, err := svc.OpenRawHTTPChannel(ctx, row)
		require.NoError(t, err)
		require.Equal(t, dependent.BaseURL, ch.BaseURL)
		require.Equal(t, dependent.UpdatedAt, ch.UpdatedAt)
		return ch
	}
	require.Equal(t, "first", svc.RawHTTPAPIKey(ctx, open()))
	_, err = svc.UpsertChannelCredentialPolicy(ctx, source.ID, objects.ChannelCredentialPolicy{ID: objects.ChannelAPIKeyFingerprint("second"), Priority: 20})
	require.NoError(t, err)
	require.Equal(t, "second", svc.RawHTTPAPIKey(ctx, open()))
	_, err = svc.DeleteChannelAPIKey(ctx, source.ID, objects.ChannelAPIKeyFingerprint("second"))
	require.NoError(t, err)
	require.Equal(t, "first", svc.RawHTTPAPIKey(ctx, open()))
}

func TestCredentialSourceTopologyAndAdminGuards(t *testing.T) {
	svc, db := setupTestChannelService(t)
	t.Cleanup(func() { svc.Stop(); _ = db.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	create := func(name string, source *int) (*ent.Channel, error) {
		return svc.CreateChannel(ctx, ent.CreateChannelInput{
			Name: name, Type: channel.TypeOpenai, BaseURL: lo.ToPtr("https://example.test"),
			SupportedModels: []string{"m"}, DefaultTestModel: "m", Credentials: objects.ChannelCredentials{APIKey: "own-" + name},
			Settings: &objects.ChannelSettings{CredentialSourceChannelID: source},
		})
	}
	source, err := create("source", nil)
	require.NoError(t, err)
	dependent, err := create("dependent", &source.ID)
	require.NoError(t, err)
	other, err := create("other", nil)
	require.NoError(t, err)
	update := func(id int, ref *int) (*ent.Channel, error) {
		return svc.UpdateChannel(ctx, id, &ent.UpdateChannelInput{Settings: &objects.ChannelSettings{
			CredentialSourceChannelID: ref, DisableRetries: true,
		}})
	}
	_, err = update(dependent.ID, &dependent.ID)
	require.ErrorContains(t, err, "cannot reference itself")
	_, err = create("chain", &dependent.ID)
	require.ErrorContains(t, err, "delegates its credentials")
	_, err = update(source.ID, &other.ID)
	require.ErrorContains(t, err, fmt.Sprintf("#%d dependent", dependent.ID))
	_, err = svc.UpdateChannelStatus(ctx, source.ID, channel.StatusArchived)
	require.ErrorContains(t, err, "dependent")
	require.ErrorContains(t, svc.BulkArchiveChannels(ctx, []int{source.ID}), "dependent")
	require.ErrorContains(t, svc.DeleteChannel(ctx, source.ID), "dependent")
	require.ErrorContains(t, svc.BulkDeleteChannels(ctx, []int{source.ID}), "dependent")

	_, err = svc.DeleteChannelAPIKey(ctx, dependent.ID, "key")
	for _, err := range []error{
		err,
		svc.AddChannelAPIKey(ctx, dependent.ID, "key"),
		svc.DisableChannelAPIKey(ctx, dependent.ID, "key", "admin"),
		svc.EnableAPIKey(ctx, dependent.ID, "key"),
		svc.EnableAllAPIKeys(ctx, dependent.ID),
		svc.EnableSelectedAPIKeys(ctx, dependent.ID, []string{"key"}),
		svc.ArchiveChannelAPIKey(ctx, dependent.ID, "key", "admin"),
		svc.RestoreChannelAPIKey(ctx, dependent.ID, "key"),
	} {
		require.ErrorContains(t, err, fmt.Sprintf("edit source channel #%d", source.ID))
	}
	_, err = svc.DeleteDisabledAPIKeys(ctx, dependent.ID, []string{"key"})
	require.ErrorContains(t, err, "edit source channel")
	_, err = svc.UpsertChannelCredentialPolicy(ctx, dependent.ID, objects.ChannelCredentialPolicy{})
	require.ErrorContains(t, err, "edit source channel")

	current, err := update(dependent.ID, &source.ID)
	require.NoError(t, err)
	require.Equal(t, &source.ID, current.Settings.CredentialSourceChannelID)
	require.True(t, current.Settings.DisableRetries)
	current, err = svc.UpdateChannel(ctx, dependent.ID, &ent.UpdateChannelInput{Name: lo.ToPtr("sibling")})
	require.NoError(t, err)
	require.Equal(t, &source.ID, current.Settings.CredentialSourceChannelID)
	current, err = update(dependent.ID, lo.ToPtr(0))
	require.NoError(t, err)
	require.Nil(t, current.Settings.CredentialSourceChannelID)
	_, err = update(dependent.ID, &source.ID)
	require.NoError(t, err)
	current, err = update(dependent.ID, nil)
	require.NoError(t, err)
	require.Nil(t, current.Settings.CredentialSourceChannelID)
	require.NoError(t, svc.DeleteChannel(ctx, source.ID))
	_, err = update(dependent.ID, &source.ID)
	require.ErrorContains(t, err, "missing, deleted, or archived")
	_, err = svc.UpdateChannelStatus(ctx, other.ID, channel.StatusArchived)
	require.NoError(t, err)
	_, err = update(dependent.ID, &other.ID)
	require.ErrorContains(t, err, "missing, deleted, or archived")
}

func TestCredentialSourceOAuthRotationWritesOwnerAndReusesFreshToken(t *testing.T) {
	svc, db := setupTestChannelService(t)
	t.Cleanup(func() { svc.Stop(); _ = db.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	old := &objects.OAuthCredentials{AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Hour)}
	source := db.Channel.Create().SetName("oauth-holder").SetType(channel.TypeCodex).
		SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").
		SetCredentials(objects.ChannelCredentials{OAuth: old}).SaveX(ctx)
	dependent, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Name: "oauth-dependent", Type: channel.TypeCodex, DefaultTestModel: "m", SupportedModels: []string{"m"},
		Settings: &objects.ChannelSettings{CredentialSourceChannelID: &source.ID},
	})
	require.NoError(t, err)
	fresh := &objects.OAuthCredentials{AccessToken: "synthetic-fresh", RefreshToken: "synthetic-rotated", ExpiresAt: time.Now().Add(time.Hour)}
	calls := 0
	refresh := func(_ context.Context, creds *objects.OAuthCredentials) (*objects.OAuthCredentials, error) {
		calls++
		require.Equal(t, old.RefreshToken, creds.RefreshToken)
		return fresh, nil
	}
	received, err := svc.coordinateOAuthRefresh(dependent)(ctx, old, refresh)
	require.NoError(t, err)
	require.Equal(t, fresh.AccessToken, received.AccessToken)
	received, err = svc.coordinateOAuthRefresh(source)(ctx, old, refresh)
	require.NoError(t, err)
	require.Equal(t, fresh.AccessToken, received.AccessToken)
	require.Equal(t, 1, calls)
	require.Equal(t, fresh.RefreshToken, db.Channel.GetX(ctx, source.ID).Credentials.OAuth.RefreshToken)
	require.Nil(t, db.Channel.GetX(ctx, dependent.ID).Credentials.OAuth)
}
