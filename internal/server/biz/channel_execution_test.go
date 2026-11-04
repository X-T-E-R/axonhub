package biz

import (
	"context"
	"github.com/google/uuid"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestClusterRemoteOAuthMetadataDoesNotConstructProvider(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:cluster-oauth-"+uuid.NewString()+"?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(context.Background())
	ch := client.Channel.Create().SetName("oauth").SetType(channel.TypeCodex).SetStatus(channel.StatusEnabled).SetBaseURL("http://127.0.0.1:1").SetCredentials(objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(24 * time.Hour)}}).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SaveX(ctx)
	config := cluster.Config{Enabled: true, NodeID: "a", ControlNode: "a", Nodes: map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.1:2"}, Channels: map[int]cluster.ChannelBinding{ch.ID: {Owner: "b", Identity: "account"}}}
	cache := xcache.Config{Mode: xcache.ModeMemory}
	sys := NewSystemService(SystemServiceParams{Ent: client, CacheConfig: cache})
	svc := NewChannelService(ChannelServiceParams{Ent: client, CacheConfig: cache, SystemService: sys, HttpClient: httpclient.NewHttpClient(), Node: config})
	defer svc.Stop()
	require.NoError(t, svc.ReloadEnabledChannelsCache(ctx))
	loaded := svc.GetEnabledChannel(ch.ID)
	require.NotNil(t, loaded)
	require.Contains(t, loaded.GetModelEntries(), "m")
	require.Nil(t, loaded.Outbound)
	require.Nil(t, loaded.HTTPClient)
	require.Nil(t, loaded.startTokenProvider)
	require.Nil(t, loaded.stopTokenProvider)
	_, err := svc.buildChannelWithTransformer(ch)
	require.ErrorAs(t, err, new(*ExecutionOwnerError))
	// A second channel with the same OAuth identity cannot get a second owner.
	duplicate := client.Channel.Create().SetName("duplicate").SetType(ch.Type).SetStatus(channel.StatusEnabled).SetBaseURL(ch.BaseURL).SetCredentials(ch.Credentials).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SaveX(ctx)
	svc.node.Channels[duplicate.ID] = cluster.ChannelBinding{Owner: "a", Identity: "different-label"}
	require.ErrorContains(t, svc.validateExecutionIdentities(ctx), "shares an upstream credential")
	// Identity placement also survives token rotation or different current tokens.
	svc.node.Channels[duplicate.ID] = cluster.ChannelBinding{Owner: "a", Identity: "account"}
	require.ErrorContains(t, svc.node.Validate(), "more than one owner")
}
