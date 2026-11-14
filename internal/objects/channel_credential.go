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

func (s *ChannelKeyBalanceSnapshot) ExhaustedAt(now time.Time) bool {
	return s != nil && (s.ResetAt == nil || s.ResetAt.After(now)) &&
		((s.Available != nil && !*s.Available) || (s.Limit != nil && *s.Limit > 0 && s.Remaining != nil && *s.Remaining <= 0))
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
