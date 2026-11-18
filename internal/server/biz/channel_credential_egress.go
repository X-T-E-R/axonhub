package biz

import (
	"context"
	"errors"
	"math"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

var outboundProxyEpoch atomic.Uint64
var errCredentialEgressBlocked = errors.New("credential egress is blocked")

type outboundProxySnapshot struct {
	mu           sync.Mutex
	rows         map[int]*ent.OutboundProxy
	expires      time.Time
	epoch        uint64
	blockedUntil map[int]time.Time
}

func (svc *ChannelService) proxyConcurrentLimit(id int) *int {
	cache := &svc.outboundProxies
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if row := cache.rows[id]; row != nil {
		return row.MaxConcurrent
	}
	return nil
}

func (svc *ChannelService) blockProxyTemporarily(id int) {
	cache := &svc.outboundProxies
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.blockedUntil == nil {
		cache.blockedUntil = make(map[int]time.Time)
	}
	cache.blockedUntil[id] = time.Now().Add(10 * time.Second)
}

func (svc *ChannelService) managedCredentialProxy(ctx context.Context, channelID int, p objects.ChannelCredentialPolicy) (*ent.OutboundProxy, error) {
	cache := &svc.outboundProxies
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := time.Now()
	epoch := outboundProxyEpoch.Load()
	if cache.epoch != epoch || !cache.expires.After(now) {
		rows, err := observationPrimaryClient(svc.db).OutboundProxy.Query().All(authz.WithSystemBypass(ctx, "credential-egress-snapshot"))
		now = time.Now()
		if err != nil {
			if cache.rows == nil || cache.epoch != epoch {
				return nil, errCredentialEgressBlocked
			}
			log.Warn(ctx, "Outbound proxy refresh failed; reusing same-epoch snapshot", log.Cause(err))
			cache.expires = now.Add(time.Second)
		} else {
			cache.rows = make(map[int]*ent.OutboundProxy, len(rows))
			for _, row := range rows {
				cache.rows[row.ID] = row
			}
			if cache.epoch != epoch {
				cache.blockedUntil = nil
			}
			cache.epoch, cache.expires = epoch, now.Add(3*time.Second)
		}
	}
	for id, until := range cache.blockedUntil {
		if !until.After(now) {
			delete(cache.blockedUntil, id)
		}
	}
	usable := func(row *ent.OutboundProxy) bool {
		return row != nil && row.Status == outboundproxy.StatusEnabled && (row.ExpiresAt == nil || row.ExpiresAt.After(now)) &&
			!cache.blockedUntil[row.ID].After(now)
	}
	seed := strconv.Itoa(channelID) + ":" + p.ID
	selectPool := func(name string, exitIP string) *ent.OutboundProxy {
		if name == "" {
			return nil
		}
		var best *ent.OutboundProxy
		bestMatch, bestProbe := false, false
		top := math.Inf(-1)
		for _, row := range cache.rows {
			if row.PoolName != name || !usable(row) {
				continue
			}
			// Manual probes are a preference, not a permanent routing ban.
			probeOK := row.LastTestSuccess == nil || *row.LastTestSuccess
			sameIP := exitIP != "" && row.LastExitIP == exitIP
			if bestProbe && !probeOK || probeOK == bestProbe && bestMatch && !sameIP {
				continue
			}
			// Weighted rendezvous gives stable, deterministic proxy leases per credential.
			u := (float64(hashAPIKey(seed+":"+strconv.Itoa(row.ID))) + 1) / (float64(^uint64(0)) + 1)
			score := math.Log(u) / float64(max(1, row.Weight))
			if best == nil || probeOK && !bestProbe || probeOK == bestProbe && (sameIP && !bestMatch || score > top || score == top && row.ID < best.ID) {
				best, top, bestMatch, bestProbe = row, score, sameIP, probeOK
			}
		}
		return best
	}
	var primary *ent.OutboundProxy
	if p.EgressMode == objects.CredentialEgressModePinnedProxy {
		if p.OutboundProxyID != nil {
			primary = cache.rows[*p.OutboundProxyID]
		}
	} else {
		primary = selectPool(p.ProxyPoolName, "")
	}
	if usable(primary) {
		return primary, nil
	}
	switch p.EgressFailPolicy {
	case objects.CredentialEgressFailPolicyBackupProxy:
		if primary != nil && primary.BackupProxyID != nil {
			if backup := cache.rows[*primary.BackupProxyID]; usable(backup) {
				return backup, nil
			}
		}
	case objects.CredentialEgressFailPolicyPoolFallback:
		name, exitIP := p.ProxyPoolName, ""
		if primary != nil {
			if name == "" {
				name = primary.PoolName
			}
			exitIP = primary.LastExitIP
		}
		if fallback := selectPool(name, exitIP); fallback != nil {
			return fallback, nil
		}
	}
	return nil, errCredentialEgressBlocked
}

func (svc *ChannelService) resolveCredentialClient(ctx context.Context, ch *ent.Channel, id string, base *httpclient.HttpClient) (*httpclient.HttpClient, int, error) {
	p := ch.Settings.CredentialPolicy(id)
	var raw string
	var proxyID int
	var noReuse bool
	switch p.EgressMode {
	case objects.CredentialEgressModeInheritChannel:
		return nil, 0, nil
	case objects.CredentialEgressModeDirect:
	case objects.CredentialEgressModeCustomURL:
		if p.CustomProxy == nil || p.CustomProxy.Type != httpclient.ProxyTypeURL {
			return nil, 0, errCredentialEgressBlocked
		}
		raw, noReuse = p.CustomProxy.URL, p.CustomProxy.DisableConnectionReuse
	case objects.CredentialEgressModePinnedProxy, objects.CredentialEgressModeProxyPool:
		proxy, err := svc.managedCredentialProxy(ctx, credentialOwnerID(ch), p)
		if err != nil {
			return nil, 0, err
		}
		raw, proxyID = proxy.ProxyURL, proxy.ID
	default:
		return nil, 0, errCredentialEgressBlocked
	}
	channelID := strconv.Itoa(ch.ID)
	if raw != "" {
		session := id
		if trace, ok := contexts.GetTrace(ctx); ok && trace != nil && trace.TraceID != "" {
			session = trace.TraceID
		}
		rendered, err := RenderOutboundProxyURL(raw, id, session, channelID)
		if err != nil {
			return nil, 0, errCredentialEgressBlocked
		}
		raw = rendered
		if p.EgressMode == objects.CredentialEgressModeCustomURL && p.CustomProxy.Username != "" && p.CustomProxy.Password != "" {
			parsed, err := url.Parse(raw)
			if err != nil {
				return nil, 0, errCredentialEgressBlocked
			}
			parsed.User = url.UserPassword(p.CustomProxy.Username, p.CustomProxy.Password)
			raw = parsed.String()
		}
	}
	return svc.transportPool.Get(channelID+":"+id, raw, base, noReuse), proxyID, nil
}

// SelectOAuthCredentialEgress selects the policy of the single OAuth credential;
// legacy OAuth channels without a first-class policy retain their existing client.
func (c *Channel) SelectOAuthCredentialEgress(ctx context.Context) error {
	if c.credentialEgress == nil || !c.Credentials.IsOAuth() || c.Settings == nil || len(c.Settings.CredentialPolicies) == 0 {
		return nil
	}
	id := objects.OAuthDefaultCredentialID
	policy := c.Settings.CredentialPolicy(id)
	if !slices.ContainsFunc(c.Settings.CredentialPolicies, func(p objects.ChannelCredentialPolicy) bool { return p.ID == id }) {
		return nil
	}
	client, proxyID, err := c.credentialEgress.resolveCredentialClient(ctx, c.Channel, id, c.HTTPClient)
	if err != nil {
		return err
	}
	candidate := credentialCandidate{id: id, policy: policy, client: client, proxyID: proxyID}
	if proxyID > 0 {
		candidate.proxyLimit = c.credentialEgress.proxyConcurrentLimit(proxyID)
	}
	if c.Settings.KeyHealthCheck != nil {
		for _, meta := range c.Settings.KeyHealthCheck.KeyMetadata {
			if meta.ID == id {
				candidate.snapshot = meta.BalanceSnapshot
				break
			}
		}
	}
	runtime := c.runtime()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	now := time.Now()
	admitted := filterCredentialCandidates([]credentialCandidate{candidate}, runtime, c.CredentialOwnerID(), now, true)
	if len(admitted) == 0 {
		return errors.New("OAuth credential has no available admission slot")
	}
	c.reserveCredential(ctx, admitted[0], now)
	return nil
}
