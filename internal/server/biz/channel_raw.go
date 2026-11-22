package biz

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/looplj/axonhub/llm/auth"
	"golang.org/x/net/http/httpguts"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func IsRawHTTPChannel(c *ent.Channel) bool {
	return c != nil && c.Settings != nil && c.Settings.RawHTTP != nil && c.Settings.RawHTTP.Enabled
}

// ValidateRawHTTPChannel validates configuration, never the provider payload.
func ValidateRawHTTPChannel(baseURL string, models []string, settings *objects.ChannelSettings) error {
	if settings == nil || settings.RawHTTP == nil || !settings.RawHTTP.Enabled {
		return nil
	}
	raw := settings.RawHTTP
	if raw.Resource == "" || strings.TrimSpace(raw.Resource) != raw.Resource || !slices.Contains(models, raw.Resource) {
		return fmt.Errorf("raw HTTP resource must be an explicit supported model alias")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("raw HTTP base URL must be http(s) with a fixed host and optional path prefix, without credentials, query or fragment")
	}
	if !RawHTTPPathAllowed(u.Path) {
		return fmt.Errorf("invalid raw HTTP base path")
	}
	switch raw.AuthType {
	case "bearer":
		if raw.AuthHeader != "" {
			return fmt.Errorf("bearer authentication does not use a custom header")
		}
	case "header":
		name := http.CanonicalHeaderKey(raw.AuthHeader)
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("invalid raw HTTP authentication header")
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "content-type", "content-encoding", "accept-encoding", "connection", "upgrade", "trailer", "te", "transfer-encoding", "proxy-connection", "keep-alive", "proxy-authorization", "proxy-authenticate":
			return fmt.Errorf("raw HTTP authentication requires an end-to-end credential header")
		}
	default:
		return fmt.Errorf("raw HTTP auth type must be bearer or header")
	}
	return nil
}

func RawHTTPPathAllowed(path string) bool {
	if strings.Contains(path, "\\") {
		return false
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

type rawHTTPKeyProviderConfig struct {
	strategy         objects.ChannelKeySelectionStrategy
	likelyTTLMinutes int
	exactTTLMinutes  int
}

type rawHTTPKeyProvider struct {
	mu              sync.Mutex
	keys            []string
	config          rawHTTPKeyProviderConfig
	provider        auth.APIKeyProvider
	updatedAt       time.Time
	sourceUpdatedAt time.Time
}

// RawHTTPAPIKey retains the existing providers' counters and affinity caches
// across requests. A changed routable key set or effective policy replaces only
// that channel's provider, using the freshly authorized channel snapshot.
func (svc *ChannelService) RawHTTPAPIKey(ctx context.Context, ch *Channel) string {
	value, _ := svc.rawHTTPKeyProviders.LoadOrStore(ch.ID, &rawHTTPKeyProvider{})
	state := value.(*rawHTTPKeyProvider)
	state.mu.Lock()
	defer state.mu.Unlock()

	selection := channelKeySelectionForProvider(ch)
	config := rawHTTPKeyProviderConfig{
		strategy:         selection.StrategyOrDefault(),
		likelyTTLMinutes: selection.LikelyAffinityTTLMinutesOrDefault(),
		exactTTLMinutes:  selection.ExactAffinityTTLMinutesOrDefault(),
	}
	if state.provider == nil || !slices.Equal(state.keys, ch.cachedEnabledAPIKeys) || state.config != config || !state.updatedAt.Equal(ch.UpdatedAt) || !state.sourceUpdatedAt.Equal(ch.credentialSourceUpdatedAt) {
		state.keys = slices.Clone(ch.cachedEnabledAPIKeys)
		state.updatedAt = ch.UpdatedAt
		state.sourceUpdatedAt = ch.credentialSourceUpdatedAt
		state.config = config
		state.provider = getAPIKeyProvider(ch)
	}
	return state.provider.Get(ctx)
}

// ResolveRawHTTPChannel reads the current channel row from primary so disabled
// channels and changed authorization/configuration take effect without waiting
// for a channel cache. The caller must verify IsRawHTTPChannel, channel/resource
// authorization, and channel status before use so disabled status is never
// disclosed to unauthorized callers.
func (svc *ChannelService) ResolveRawHTTPChannel(ctx context.Context, id int) (*ent.Channel, error) {
	return observationPrimaryClient(svc.db).Channel.Get(ctx, id)
}

// OpenRawHTTPChannel checks committed placement for an authorized row before
// building its transport. A stale Worker or provider cache cannot execute it
// on the former owner; existing upstream streams keep their original snapshot.
func (svc *ChannelService) OpenRawHTTPChannel(ctx context.Context, row *ent.Channel) (*Channel, error) {
	if row == nil || !IsRawHTTPChannel(row) || row.Status != channel.StatusEnabled {
		return nil, fmt.Errorf("raw HTTP channel unavailable")
	}
	if svc.node.Enabled {
		_, state, err := readChannelPlacement(ctx, svc.db)
		if err != nil {
			return nil, err
		}
		node := svc.node.AtPlacement(state.PlacementSnapshot)
		if !node.Owns(row.ID) {
			return nil, &ExecutionOwnerError{ChannelID: row.ID, Owner: node.Owner(row.ID)}
		}
	}
	source, err := resolveCredentialOwner(authz.WithSystemBypass(ctx, "open-raw-channel-credentials"), observationPrimaryClient(svc.db), row)
	if err != nil {
		return nil, err
	}
	ch, err := svc.buildChannelOnOwner(withEffectiveCredentials(row, source))
	if err != nil {
		return nil, err
	}
	if source != row {
		ch.credentialSourceUpdatedAt = source.UpdatedAt
	}
	return ch, nil
}
