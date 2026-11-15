package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/internal/objects"
)

func TestManagedCredentialProxy_ManualProbePreferenceAndRuntimeRecovery(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	ctx := authz.WithTestBypass(context.Background())
	create := func(name string, success bool) *ent.OutboundProxy {
		return client.OutboundProxy.Create().SetName(name).SetKind(outboundproxy.KindStatic).
			SetProxyURL("http://127.0.0.1:19101").SetPoolName("pool").SetStatus(outboundproxy.StatusEnabled).
			SetLastTestSuccess(success).SaveX(ctx)
	}
	failed := create("manual-failure", false)
	preferred := create("manual-success", true)
	pinned := objects.ChannelCredentialPolicy{ID: "credential", EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: lo.ToPtr(failed.ID)}
	row, err := svc.managedCredentialProxy(ctx, 1, pinned)
	require.NoError(t, err)
	require.Equal(t, failed.ID, row.ID)
	pool := objects.ChannelCredentialPolicy{ID: "credential", EgressMode: objects.CredentialEgressModeProxyPool, ProxyPoolName: "pool"}
	row, err = svc.managedCredentialProxy(ctx, 1, pool)
	require.NoError(t, err)
	require.Equal(t, preferred.ID, row.ID)
	svc.blockProxyTemporarily(preferred.ID)
	row, err = svc.managedCredentialProxy(ctx, 1, pool)
	require.NoError(t, err)
	require.Equal(t, failed.ID, row.ID, "stale manual failure remains an enabled pool fallback")
	svc.blockProxyTemporarily(failed.ID)
	_, err = svc.managedCredentialProxy(ctx, 1, pinned)
	require.ErrorIs(t, err, errCredentialEgressBlocked)
	svc.outboundProxies.blockedUntil[failed.ID] = time.Now().Add(-time.Second)
	row, err = svc.managedCredentialProxy(ctx, 1, pinned)
	require.NoError(t, err)
	require.Equal(t, failed.ID, row.ID, "runtime cooldown recovers without another manual probe")
}

func TestManagedCredentialProxy_RefreshFailureReusesOnlySameEpochSnapshot(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	ctx := authz.WithTestBypass(context.Background())
	proxy := client.OutboundProxy.Create().SetName("pinned").SetKind(outboundproxy.KindStatic).
		SetProxyURL("http://127.0.0.1:19101").SetStatus(outboundproxy.StatusEnabled).SaveX(ctx)
	policy := objects.ChannelCredentialPolicy{ID: "credential", EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: lo.ToPtr(proxy.ID)}
	_, err := svc.managedCredentialProxy(ctx, 1, policy)
	require.NoError(t, err)
	client.OutboundProxy.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(context.Context, ent.Query) (ent.Value, error) {
			return nil, errors.New("test transient database failure")
		})
	}))
	cache := &svc.outboundProxies
	cache.expires = time.Now().Add(-time.Second)
	before := time.Now()
	row, err := svc.managedCredentialProxy(ctx, 1, policy)
	require.NoError(t, err)
	require.Equal(t, proxy.ID, row.ID)
	require.True(t, cache.expires.After(before))
	require.True(t, cache.expires.Before(before.Add(2*time.Second)))
	cache.epoch = ^outboundProxyEpoch.Load()
	_, err = svc.managedCredentialProxy(ctx, 1, policy)
	require.ErrorIs(t, err, errCredentialEgressBlocked, "an invalidated administrative snapshot cannot be reused")
	cache.epoch, cache.rows = outboundProxyEpoch.Load(), nil
	cache.expires = time.Now().Add(-time.Second)
	_, err = svc.managedCredentialProxy(ctx, 1, policy)
	require.ErrorIs(t, err, errCredentialEgressBlocked, "first load still requires a known configuration")
}

func TestManagedCredentialProxy_SlowRefreshHonorsCurrentTime(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	ctx := authz.WithTestBypass(t.Context())
	expiry := time.Now().Add(500 * time.Millisecond)
	proxy := client.OutboundProxy.Create().SetName("slow-refresh-expiry").SetKind(outboundproxy.KindStatic).
		SetProxyURL("http://127.0.0.1:19101").SetStatus(outboundproxy.StatusEnabled).SetExpiresAt(expiry).SaveX(ctx)
	policy := objects.ChannelCredentialPolicy{ID: "review-credential", EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: lo.ToPtr(proxy.ID)}
	_, err := svc.managedCredentialProxy(ctx, 1, policy)
	require.NoError(t, err)
	client.OutboundProxy.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(context.Context, ent.Query) (ent.Value, error) {
			time.Sleep(1100 * time.Millisecond)
			return nil, errors.New("review transient slow database failure")
		})
	}))
	svc.outboundProxies.expires = time.Now().Add(-time.Second)
	row, err := svc.managedCredentialProxy(ctx, 1, policy)
	if err == nil && row != nil {
		t.Errorf("returned expired proxy: selected at %s, proxy expiry %s", time.Now().UTC(), *row.ExpiresAt)
	}
	if !svc.outboundProxies.expires.After(time.Now()) {
		t.Errorf("same-epoch fallback's one-second retry cache was already expired when refresh returned: %s", svc.outboundProxies.expires.UTC())
	}
}
