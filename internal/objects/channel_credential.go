package objects

import "time"

const OAuthDefaultCredentialID = "oauth_default"

type CredentialEgressMode string

const (
	CredentialEgressModeInheritChannel CredentialEgressMode = "inherit_channel"
	CredentialEgressModeDirect         CredentialEgressMode = "direct"
	CredentialEgressModePinnedProxy    CredentialEgressMode = "pinned_proxy"
	CredentialEgressModeProxyPool      CredentialEgressMode = "proxy_pool"
	CredentialEgressModeCustomURL      CredentialEgressMode = "custom_url"
)

type CredentialEgressFailPolicy string

const (
	CredentialEgressFailPolicyFailClosed   CredentialEgressFailPolicy = "fail_closed"
	CredentialEgressFailPolicyBackupProxy  CredentialEgressFailPolicy = "backup_proxy"
	CredentialEgressFailPolicyPoolFallback CredentialEgressFailPolicy = "pool_fallback"
)

type ChannelCredentialPolicy struct {
	ID                string                     `json:"id"`
	Name              string                     `json:"name,omitempty"`
	Priority          int                        `json:"priority,omitempty"`
	Weight            int                        `json:"weight,omitempty"`
	MaxConcurrent     *int                       `json:"maxConcurrent,omitempty"`
	RequestIntervalMs *int                       `json:"requestIntervalMs,omitempty"`
	ManualQuotaLimit  *float64                   `json:"manualQuotaLimit,omitempty"`
	EgressMode        CredentialEgressMode       `json:"egressMode,omitempty"`
	OutboundProxyID   *int                       `json:"outboundProxyId,omitempty"`
	ProxyPoolName     string                     `json:"proxyPoolName,omitempty"`
	CustomProxy       *ProxyConfig               `json:"customProxy,omitempty"`
	EgressFailPolicy  CredentialEgressFailPolicy `json:"egressFailPolicy,omitempty"`
}

func (p ChannelCredentialPolicy) WithDefaults() ChannelCredentialPolicy {
	if p.Weight == 0 {
		p.Weight = 100
	}
	if p.EgressMode == "" {
		p.EgressMode = CredentialEgressModeInheritChannel
	}
	if p.EgressFailPolicy == "" {
		p.EgressFailPolicy = CredentialEgressFailPolicyFailClosed
	}
	return p
}

func (s *ChannelSettings) CredentialPolicy(id string) ChannelCredentialPolicy {
	if s != nil {
		for _, policy := range s.CredentialPolicies {
			if policy.ID == id {
				return policy.WithDefaults()
			}
		}
	}
	return (ChannelCredentialPolicy{ID: id}).WithDefaults()
}

func (w ChannelQuotaWindow) ActiveAt(now time.Time) bool {
	return w.ResetAt == nil || w.ResetAt.After(now)
}

func (w ChannelQuotaWindow) EffectiveUsageRatio() (float64, bool) {
	if w.UsageRatio != nil {
		r := *w.UsageRatio
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		return r, true
	}
	if w.Limit != nil && *w.Limit > 0 {
		if w.Used != nil {
			r := *w.Used / *w.Limit
			if r < 0 {
				r = 0
			}
			if r > 1 {
				r = 1
			}
			return r, true
		}
		if w.Remaining != nil {
			r := 1 - (*w.Remaining / *w.Limit)
			if r < 0 {
				r = 0
			}
			if r > 1 {
				r = 1
			}
			return r, true
		}
	}
	return 0, false
}

func (w ChannelQuotaWindow) ExhaustedAt(now time.Time) bool {
	if !w.ActiveAt(now) {
		return false
	}
	if w.Limit != nil && *w.Limit > 0 {
		if w.Remaining != nil && *w.Remaining <= 0 {
			return true
		}
		if w.Used != nil && *w.Used >= *w.Limit {
			return true
		}
	}
	if w.UsageRatio != nil && *w.UsageRatio >= 1.0 {
		return true
	}
	return false
}

// ExhaustedAt evaluates whether the credential is exhausted at time now.
// Multi-window snapshots evaluate every hard window independently so an earlier
// short-window reset (e.g. 5h) never releases a key whose longer window (e.g. 7d)
// is still active and exhausted. Unlimited and free-tier snapshots without an
// exhausted hard window are never treated as exhausted.
func (s *ChannelKeyBalanceSnapshot) ExhaustedAt(now time.Time) bool {
	if s == nil {
		return false
	}
	if s.Unlimited != nil && *s.Unlimited {
		return false
	}
	if len(s.Windows) > 0 {
		for _, w := range s.Windows {
			if w.ExhaustedAt(now) {
				return true
			}
		}
		// Even when all rolling windows still have capacity, an independent hard
		// account-level total quota (e.g. Sub2 total quota $0) must still mark exhausted.
		if (s.FreeTier == nil || !*s.FreeTier) && s.Limit != nil && *s.Limit > 0 && s.Remaining != nil && *s.Remaining <= 0 {
			if s.ResetAt == nil || s.ResetAt.After(now) {
				return true
			}
		}
		return false
	}
	if s.FreeTier != nil && *s.FreeTier {
		// Free-tier accounts with zero prepaid credit are not hard-blocked.
		return false
	}
	return (s.ResetAt == nil || s.ResetAt.After(now)) &&
		((s.Available != nil && !*s.Available) || (s.Limit != nil && *s.Limit > 0 && s.Remaining != nil && *s.Remaining <= 0))
}

// WorstUsageRatioAt returns the highest usage ratio across all active hard
// windows at time now (for quota-aware ranking). Falls back to scalar ratio.
func (s *ChannelKeyBalanceSnapshot) WorstUsageRatioAt(now time.Time) (float64, bool) {
	if s == nil {
		return 0, false
	}
	if s.Unlimited != nil && *s.Unlimited {
		return 0, true
	}
	if len(s.Windows) > 0 {
		best := 0.0
		found := false
		for _, w := range s.Windows {
			if !w.ActiveAt(now) {
				continue
			}
			r, ok := w.EffectiveUsageRatio()
			if !ok {
				continue
			}
			if !found || r > best {
				best = r
				found = true
			}
		}
		return best, found
	}
	if s.UsageRatio != nil {
		r := *s.UsageRatio
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		return r, true
	}
	if s.Limit != nil && *s.Limit > 0 && s.Remaining != nil {
		r := 1 - (*s.Remaining / *s.Limit)
		if r < 0 {
			r = 0
		}
		if r > 1 {
			r = 1
		}
		return r, true
	}
	return 0, false
}

type ChannelBalanceProbeScriptMode string

const (
	ChannelBalanceProbeScriptModeNone    ChannelBalanceProbeScriptMode = "none"
	ChannelBalanceProbeScriptModeExpr    ChannelBalanceProbeScriptMode = "expr"
	ChannelBalanceProbeScriptModeCommand ChannelBalanceProbeScriptMode = "command"
)

type ChannelBalanceProbeScript struct {
	Mode       ChannelBalanceProbeScriptMode `json:"mode,omitempty"`
	Expression string                        `json:"expression,omitempty"`
	Command    []string                      `json:"command,omitempty"`
}
