package biz

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/llm/httpclient"
)

func (svc *ChannelService) credentialPolicyTransaction(settings *objects.ChannelSettings) func(context.Context, func(context.Context) error) error {
	if settings == nil || settings.CredentialPolicies == nil {
		return svc.RunInTransaction
	}
	return func(ctx context.Context, fn func(context.Context) error) error {
		return WithOutboundProxyPolicyMutation(ctx, svc.entFromContext(ctx), fn)
	}
}

func pruneCredentialPolicies(settings *objects.ChannelSettings, credentials objects.ChannelCredentials) {
	if settings == nil || len(settings.CredentialPolicies) == 0 {
		return
	}
	ids := credentialPolicyIDs(credentials)
	settings.CredentialPolicies = slices.DeleteFunc(settings.CredentialPolicies, func(p objects.ChannelCredentialPolicy) bool { return !ids[p.ID] })
}

func credentialPolicyIDs(credentials objects.ChannelCredentials) map[string]bool {
	ids := map[string]bool{}
	for _, key := range credentials.GetAllAPIKeys() {
		ids[objects.ChannelAPIKeyFingerprint(key)] = true
	}
	if credentials.IsOAuth() {
		ids[objects.OAuthDefaultCredentialID] = true
	}
	return ids
}

func validateCredentialPolicies(settings *objects.ChannelSettings, credentials objects.ChannelCredentials) error {
	if settings == nil {
		return nil
	}
	ids := credentialPolicyIDs(credentials)
	seen := map[string]bool{}
	for i, policy := range settings.CredentialPolicies {
		if !ids[policy.ID] || seen[policy.ID] {
			return errors.New("credential policy requires a unique existing credential ID")
		}
		seen[policy.ID] = true
		p := policy.WithDefaults()
		p.Name = strings.TrimSpace(p.Name)
		if len(p.Name) > 200 || p.Weight < 1 || p.Weight > 1000000 {
			return errors.New("credential policy name is too long or weight is outside 1..1000000")
		}
		if p.MaxConcurrent != nil && *p.MaxConcurrent < 1 || p.RequestIntervalMs != nil && (*p.RequestIntervalMs < 0 || *p.RequestIntervalMs > 86400000) {
			return errors.New("invalid credential concurrency or request interval")
		}
		if p.ManualQuotaLimit != nil && (*p.ManualQuotaLimit <= 0 || math.IsInf(*p.ManualQuotaLimit, 0) || math.IsNaN(*p.ManualQuotaLimit)) {
			return errors.New("manual quota limit must be positive and finite")
		}
		switch p.EgressFailPolicy {
		case objects.CredentialEgressFailPolicyFailClosed, objects.CredentialEgressFailPolicyBackupProxy, objects.CredentialEgressFailPolicyPoolFallback:
		default:
			return errors.New("invalid credential egress fail policy")
		}
		switch p.EgressMode {
		case objects.CredentialEgressModeInheritChannel, objects.CredentialEgressModeDirect:
		case objects.CredentialEgressModePinnedProxy:
			if p.OutboundProxyID == nil || *p.OutboundProxyID <= 0 {
				return errors.New("pinned proxy requires an outboundProxyId")
			}
		case objects.CredentialEgressModeProxyPool:
			if strings.TrimSpace(p.ProxyPoolName) == "" {
				return errors.New("proxy pool requires a pool name")
			}
		case objects.CredentialEgressModeCustomURL:
			if p.CustomProxy == nil || p.CustomProxy.Type != httpclient.ProxyTypeURL {
				return errors.New("custom proxy requires a URL proxy config")
			}
			if _, err := RenderOutboundProxyURL(p.CustomProxy.URL, p.ID, p.ID, "1"); err != nil {
				return errors.New("invalid credential proxy URL")
			}
		default:
			return errors.New("invalid credential egress mode")
		}
		settings.CredentialPolicies[i] = p
	}
	return nil
}

func (svc *ChannelService) validateCredentialBindings(ctx context.Context, channelID int, settings *objects.ChannelSettings) error {
	return ValidateChannelCredentialBindings(ctx, svc.entFromContext(ctx), channelID, settings)
}

// ValidateChannelCredentialBindings validates the projected primary and backup
// references. Callers hold WithOutboundProxyPolicyMutation through commit.
func ValidateChannelCredentialBindings(ctx context.Context, db *ent.Client, channelID int, settings *objects.ChannelSettings) error {
	if settings == nil || len(settings.CredentialPolicies) == 0 {
		return nil
	}
	readCtx := authz.WithSystemBypass(ctx, "credential-binding-reference-count")
	proxies, err := db.OutboundProxy.Query().All(readCtx)
	if err != nil {
		return err
	}
	byID := make(map[int]*ent.OutboundProxy, len(proxies))
	for _, proxy := range proxies {
		byID[proxy.ID] = proxy
	}
	affected := map[int]bool{}
	for _, policy := range settings.CredentialPolicies {
		if policy.EgressMode != objects.CredentialEgressModePinnedProxy || policy.OutboundProxyID == nil {
			continue
		}
		proxy := byID[*policy.OutboundProxyID]
		if proxy == nil {
			return errors.New("credential outbound proxy does not exist; restore or create its managed proxy first")
		}
		affected[proxy.ID] = true
		if policy.EgressFailPolicy == objects.CredentialEgressFailPolicyBackupProxy && proxy.BackupProxyID != nil {
			affected[*proxy.BackupProxyID] = true
		}
	}
	if len(affected) == 0 {
		return nil
	}
	rows, err := db.Channel.Query().Where(channel.IDNEQ(channelID)).All(readCtx)
	if err != nil {
		return err
	}
	rows = append(rows, &ent.Channel{ID: channelID, Settings: settings})
	refs, _ := outboundProxyPolicyReferenceCounts(proxies, rows)
	for id := range affected {
		proxy := byID[id]
		if proxy == nil {
			return errors.New("credential backup proxy does not exist")
		}
		if proxy.MaxCredentials != nil && refs[id].credentialCount() > *proxy.MaxCredentials {
			return fmt.Errorf("outbound proxy %d allows at most %d credentials", id, *proxy.MaxCredentials)
		}
	}
	return nil
}

func (svc *ChannelService) UpsertChannelCredentialPolicy(ctx context.Context, channelID int, policy objects.ChannelCredentialPolicy) (*ChannelAPIKeyInventoryItem, error) {
	if authz.HasScope(ctx, scopes.ScopeWriteChannels) {
		ctx = authz.WithScopeDecision(ctx, scopes.ScopeWriteChannels)
	}
	row, err := svc.mutableCredentialChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	settings := cloneChannelSettings(row.Settings)
	if settings == nil {
		settings = &objects.ChannelSettings{}
	}
	found := false
	for i, p := range settings.CredentialPolicies {
		if p.ID == policy.ID {
			settings.CredentialPolicies[i] = policy
			found = true
			break
		}
	}
	if !found {
		settings.CredentialPolicies = append(settings.CredentialPolicies, policy)
	}
	if _, err := svc.UpdateChannel(ctx, channelID, &ent.UpdateChannelInput{Settings: settings}); err != nil {
		return nil, err
	}
	items, err := svc.ChannelAPIKeyInventory(ctx, channelID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID == policy.ID {
			return item, nil
		}
	}
	return nil, errors.New("credential no longer exists")
}

func (svc *ChannelService) populateCredentialInventory(ctx context.Context, ch *ent.Channel, item *ChannelAPIKeyInventoryItem) {
	policy := ch.Settings.CredentialPolicy(item.ID)
	item.Policy = &policy
	state := svc.credentialRuntime.snapshot(credentialOwnerID(ch), item.ID)
	item.InFlight = state.inFlight
	if !state.cooldownUntil.IsZero() {
		item.CooldownUntil = &state.cooldownUntil
	}
	if item.BalanceSnapshot != nil {
		snapshot := item.BalanceSnapshot
		item.QuotaResetAt = snapshot.ResetAt
		if snapshot.UsageRatio != nil {
			ratio := 1 - *snapshot.UsageRatio
			item.QuotaRemainingRatio = &ratio
		} else if policy.ManualQuotaLimit != nil && snapshot.Remaining != nil {
			ratio := max(0, min(1, *snapshot.Remaining / *policy.ManualQuotaLimit))
			item.QuotaRemainingRatio = &ratio
		}
	}
	if policy.EgressMode == objects.CredentialEgressModePinnedProxy || policy.EgressMode == objects.CredentialEgressModeProxyPool {
		proxy, err := svc.managedCredentialProxy(ctx, credentialOwnerID(ch), policy)
		if err == nil {
			item.EffectiveProxyName = proxy.Name
			item.EffectiveExitIP = proxy.LastExitIP
		}
	}
}
