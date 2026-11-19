package biz

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
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

// Each channel retains its own transport. Only token exchange and persistence
// are deduplicated, keyed by the shared credential owner.
func (svc *ChannelService) coordinateOAuthRefresh(ch *ent.Channel) oauth.RefreshCoordinator {
	id := credentialOwnerID(ch)
	return func(ctx context.Context, current *oauth.OAuthCredentials, refresh oauth.RefreshFunc) (*oauth.OAuthCredentials, error) {
		ctx = authz.WithSystemBypass(ctx, "refresh-channel-oauth-credentials")
		result, err, _ := svc.oauthRefresh.Do(strconv.Itoa(id), func() (any, error) {
			db := observationPrimaryClient(svc.entFromContext(ctx))
			owner, err := credentialSourceChannel(ctx, db, id)
			if err != nil {
				return nil, err
			}
			stored := owner.Credentials.OAuth
			legacy := ch.Type == channel.TypeAntigravity
			if stored == nil {
				if legacy {
					token, _, _ := strings.Cut(owner.Credentials.APIKey, "|")
					legacyCreds := *current
					legacyCreds.RefreshToken = token
					stored = &legacyCreds
				} else {
					stored, err = oauth.ParseCredentialsJSON(owner.Credentials.APIKey)
					if err != nil {
						return nil, fmt.Errorf("credential source #%d no longer has OAuth credentials", id)
					}
				}
			}
			if stored.AccessToken != "" && !stored.IsExpired(time.Now()) &&
				(stored.AccessToken != current.AccessToken || stored.RefreshToken != current.RefreshToken || stored.ExpiresAt.After(current.ExpiresAt)) {
				return stored, nil // Another transformer already refreshed this owner.
			}
			fresh, err := refresh(ctx, stored)
			if err != nil {
				return nil, err
			}
			updated := owner.Credentials
			updated.OAuth = fresh
			if legacy || owner.Type == channel.TypeAntigravity {
				project, err := extractProjectIDFromAntigravityCreds(updated.APIKey)
				if err != nil {
					return nil, err
				}
				updated.APIKey = fresh.RefreshToken + "|" + project
			} else {
				updated.APIKey, err = fresh.ToJSON()
				if err != nil {
					return nil, err
				}
			}
			if err := channelAPIKeyStateUpdate(db, owner).SetCredentials(updated).Exec(ctx); err != nil {
				return nil, fmt.Errorf("persist OAuth refresh for credential source #%d: %w", id, err)
			}
			svc.reloadChannelsAfterCommit(ctx)
			return fresh, nil
		})
		if err != nil {
			return nil, err
		}
		fresh, ok := result.(*oauth.OAuthCredentials)
		if !ok {
			return nil, fmt.Errorf("unexpected OAuth refresh result type %T", result)
		}
		return fresh, nil
	}
}
