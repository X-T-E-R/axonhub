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

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/net/http/httpguts"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
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
	for _, replayPath := range raw.SafeReplayPaths {
		if replayPath == "" || strings.TrimSpace(replayPath) != replayPath || !strings.HasPrefix(replayPath, "/") || strings.ContainsAny(replayPath, "?#") || !RawHTTPPathAllowed(replayPath) {
			return fmt.Errorf("invalid raw HTTP safe replay path %q", replayPath)
		}
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
	mu            sync.Mutex
	config        rawHTTPKeyProviderConfig
	traceCache    *lru.Cache[string, string]
	affinityCache *lru.Cache[string, stickyCacheEntry]
}

// ReserveRawHTTPCredential atomically selects and reserves a credential on the
// freshly authorized channel snapshot using the shared service credentialRuntime.
// Sticky/affinity caches are preserved across requests without retaining a stale
// channel snapshot. Pass allowSoftFallback=false when selecting a retry alternate
// so cooling, quota-exhausted, and excluded keys are strictly ineligible.
func (svc *ChannelService) ReserveRawHTTPCredential(
	ctx context.Context,
	ch *Channel,
	excluded map[string]struct{},
	allowSoftFallback bool,
) *RawHTTPCredentialAttempt {
	if ch == nil {
		return nil
	}
	if ch.apiKeyOverride != "" {
		if _, skip := excluded[ch.apiKeyOverride]; skip || contexts.ChannelCredentialExcluded(ctx, ch.ID, ch.apiKeyOverride) {
			return nil
		}
		return &RawHTTPCredentialAttempt{
			ChannelID:         ch.ID,
			CredentialOwnerID: ch.CredentialOwnerID(),
			Key:               ch.apiKeyOverride,
			KeyID:             objects.ChannelAPIKeyFingerprint(ch.apiKeyOverride),
			StartedAt:         time.Now(),
			EgressClient:      ch.HTTPClient,
		}
	}

	selection := channelKeySelectionForProvider(ch)
	config := rawHTTPKeyProviderConfig{
		strategy:         selection.StrategyOrDefault(),
		likelyTTLMinutes: selection.LikelyAffinityTTLMinutesOrDefault(),
		exactTTLMinutes:  selection.ExactAffinityTTLMinutesOrDefault(),
	}

	value, _ := svc.rawHTTPKeyProviders.LoadOrStore(ch.ID, &rawHTTPKeyProvider{})
	state := value.(*rawHTTPKeyProvider)
	state.mu.Lock()
	if state.config != config || (config.strategy == objects.ChannelKeySelectionStrategyTraceSticky && state.traceCache == nil) || (config.strategy == objects.ChannelKeySelectionStrategyCacheAffinity && state.affinityCache == nil) {
		state.config = config
		if config.strategy == objects.ChannelKeySelectionStrategyTraceSticky {
			state.traceCache, _ = lru.New[string, string](traceStickyLRUSize)
			state.affinityCache = nil
		} else if config.strategy == objects.ChannelKeySelectionStrategyCacheAffinity {
			state.affinityCache, _ = lru.New[string, stickyCacheEntry](traceStickyLRUSize)
			state.traceCache = nil
		} else {
			state.traceCache = nil
			state.affinityCache = nil
		}
	}
	traceCache := state.traceCache
	affinityCache := state.affinityCache
	state.mu.Unlock()

	var sticky func([]string) string
	switch config.strategy {
	case objects.ChannelKeySelectionStrategyTraceSticky:
		if trace, ok := contexts.GetTrace(ctx); ok && trace != nil && trace.TraceID != "" && traceCache != nil {
			sticky = func(keys []string) string {
				if old, ok := traceCache.Get(trace.TraceID); ok && !slices.Contains(keys, old) {
					ch.logCredentialAffinityEscape(ctx, old, keys)
				}
				return selectStickyKey(traceCache, keys, trace.TraceID)
			}
		}
	case objects.ChannelKeySelectionStrategyCacheAffinity:
		if id, ok := contexts.GetChannelKeyAffinityID(ctx); ok && id != "" && affinityCache != nil {
			likelyTTL := time.Duration(config.likelyTTLMinutes) * time.Minute
			exactTTL := time.Duration(config.exactTTLMinutes) * time.Minute
			sticky = func(keys []string) string {
				if old, ok := affinityCache.Get(id); ok && old.ExpiresAt.After(time.Now()) && !slices.Contains(keys, old.Key) {
					ch.logCredentialAffinityEscape(ctx, old.Key, keys)
				}
				ttl := likelyTTL
				if cacheAffinityTierFromID(id) == "exact" {
					ttl = exactTTL
				}
				return selectTTLStickyKey(affinityCache, keys, id, ttl)
			}
		}
	}

	effectiveExcluded := excluded
	clonedExcluded := false
	ensureClonedExcluded := func() {
		if !clonedExcluded {
			effectiveExcluded = cloneStringSet(excluded)
			clonedExcluded = true
		}
	}
	if !allowSoftFallback && svc.db != nil {
		// Narrow retry selection against the authoritative primary owner row by
		// intersecting the request's authorized snapshot with currently routable
		// credentials (excluding disabled, archived, and deleted keys). Fail
		// closed if current eligibility cannot be established.
		lookupCtx, cancel := xcontext.DetachWithTimeout(ctx, 2*time.Second)
		owner, err := observationPrimaryClient(svc.db).Channel.Get(
			authz.WithSystemBypass(lookupCtx, "raw-http-retry-credential-refresh"),
			ch.CredentialOwnerID(),
		)
		cancel()
		if err != nil || owner == nil || owner.Status != channel.StatusEnabled {
			return nil
		}
		currentRoutable := owner.Credentials.GetRoutableAPIKeys(owner.DisabledAPIKeys, channelArchivedAPIKeys(owner.Settings))
		routableSet := make(map[string]struct{}, len(currentRoutable))
		for _, k := range currentRoutable {
			routableSet[k] = struct{}{}
		}
		for _, key := range ch.cachedEnabledAPIKeys {
			if _, ok := routableSet[key]; !ok {
				ensureClonedExcluded()
				effectiveExcluded[key] = struct{}{}
			}
		}
	}

	return ch.reserveCredentialAttempt(ctx, config.strategy, sticky, effectiveExcluded, allowSoftFallback)
}

func cloneStringSet(src map[string]struct{}) map[string]struct{} {
	dst := make(map[string]struct{}, len(src)+1)
	for k := range src {
		dst[k] = struct{}{}
	}
	return dst
}

// RawHTTPAPIKey reserves an initial Raw HTTP credential on the current channel
// snapshot and binds it to the request context for legacy callers.
func (svc *ChannelService) RawHTTPAPIKey(ctx context.Context, ch *Channel) string {
	attempt := svc.ReserveRawHTTPCredential(ctx, ch, nil, true)
	if attempt == nil {
		contexts.WithChannelAPIKey(ctx, "")
		return ""
	}
	contexts.WithChannelCredentialEgress(ctx, attempt.Key, attempt.EgressClient, attempt.legacyFinishFunc())
	return attempt.Key
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
