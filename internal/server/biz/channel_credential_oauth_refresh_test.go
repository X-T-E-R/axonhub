package biz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

func TestOAuthRefreshUsesManagedCredentialEgressAndFailsClosed(t *testing.T) {
	svc, db := setupTestChannelService(t)
	defer db.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "http://provider.invalid/oauth/token", r.URL.String())
		_, _ = io.WriteString(w, `{"access_token":"refreshed"}`)
	}))
	defer proxy.Close()
	row := db.OutboundProxy.Create().SetName("refresh-proxy").SetProxyURL(proxy.URL).SaveX(ctx)
	channel := &ent.Channel{ID: 27, Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{AccessToken: "synthetic"}}, Settings: &objects.ChannelSettings{CredentialPolicies: []objects.ChannelCredentialPolicy{{ID: objects.OAuthDefaultCredentialID, EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: &row.ID}}}}
	client := svc.oauthCredentialHTTPClient(channel, httpclient.NewHttpClient())
	response, err := client.Do(ctx, &httpclient.Request{Method: http.MethodPost, URL: "http://provider.invalid/oauth/token"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.EqualValues(t, 1, calls.Load())
	db.OutboundProxy.UpdateOneID(row.ID).SetStatus(outboundproxy.StatusDisabled).SaveX(ctx)
	outboundProxyEpoch.Add(1)
	_, err = client.Do(ctx, &httpclient.Request{Method: http.MethodPost, URL: "http://provider.invalid/oauth/token"})
	require.ErrorContains(t, err, "egress is blocked")
	require.EqualValues(t, 1, calls.Load())
}
