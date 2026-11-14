package biz

import (
	"context"
	"testing"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/stretchr/testify/require"
)

func TestCredentialPolicyRemovedWhenKeyDeletedOrReplaced(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proxy := client.OutboundProxy.Create().SetName("bound").SetProxyURL("http://proxy.example:8080").SaveX(ctx)
	first, second := "first-upstream-key", "second-upstream-key"
	policy := objects.ChannelCredentialPolicy{ID: objects.ChannelAPIKeyFingerprint(first), EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: &proxy.ID}
	row := client.Channel.Create().SetName("keys").SetType(channel.TypeOpenai).SetCredentials(objects.ChannelCredentials{APIKeys: []string{first, second}}).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SetSettings(&objects.ChannelSettings{CredentialPolicies: []objects.ChannelCredentialPolicy{policy}}).SaveX(ctx)
	_, err := svc.DeleteChannelAPIKey(ctx, row.ID, policy.ID)
	require.NoError(t, err)
	current := client.Channel.GetX(ctx, row.ID)
	require.Empty(t, current.Settings.CredentialPolicies)
	proxies := NewOutboundProxyService(OutboundProxyServiceParams{Ent: client})
	require.NoError(t, proxies.Delete(ctx, proxy.ID))

	policy = objects.ChannelCredentialPolicy{ID: objects.ChannelAPIKeyFingerprint(second), Name: "removed-with-key"}
	current.Settings.CredentialPolicies = []objects.ChannelCredentialPolicy{policy}
	client.Channel.UpdateOneID(row.ID).SetSettings(current.Settings).SaveX(ctx)
	_, err = svc.UpdateChannel(ctx, row.ID, &ent.UpdateChannelInput{Credentials: &objects.ChannelCredentials{APIKey: "replacement-key"}})
	require.NoError(t, err)
	require.Empty(t, client.Channel.GetX(ctx, row.ID).Settings.CredentialPolicies)
}
