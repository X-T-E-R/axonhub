package biz

import (
	"net/http"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

// Token refresh runs outside request admission, but must keep the same network
// identity and fail-closed policy as the OAuth credential's ordinary traffic.
type oauthCredentialTransport struct {
	service *ChannelService
	channel *ent.Channel
	base    *httpclient.HttpClient
}

func (t *oauthCredentialTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	client, _, err := t.service.resolveCredentialClient(request.Context(), t.channel, objects.OAuthDefaultCredentialID, t.base)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = t.base
	}
	transport := client.GetNativeClient().Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(request)
}

func (svc *ChannelService) oauthCredentialHTTPClient(ch *ent.Channel, base *httpclient.HttpClient) *httpclient.HttpClient {
	if !ch.Credentials.IsOAuth() || ch.Settings == nil || len(ch.Settings.CredentialPolicies) == 0 {
		return base
	}
	native := *base.GetNativeClient()
	native.Transport = &oauthCredentialTransport{service: svc, channel: ch, base: base}
	return httpclient.NewHttpClientWithClient(&native)
}
