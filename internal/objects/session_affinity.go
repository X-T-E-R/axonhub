package objects

type ChannelContinuationGuard string

const (
	ChannelContinuationGuardStrictReplay ChannelContinuationGuard = "strict_replay"
	ChannelContinuationGuardOff          ChannelContinuationGuard = "off"
)

const (
	DefaultSessionAffinityTTLSeconds = 3600
	MinSessionAffinityTTLSeconds     = 60
	MaxSessionAffinityTTLSeconds     = 604800 // 7 days
)

type ChannelSessionAffinity struct {
	Enabled           bool                     `json:"enabled"`
	TTLSeconds        *int                     `json:"ttlSeconds,omitempty"`
	Anchors           []string                 `json:"anchors,omitempty"`
	ContinuationGuard ChannelContinuationGuard `json:"continuationGuard,omitempty"`
}

func (s *ChannelSessionAffinity) TTLSecondsOrDefault() int {
	if s == nil || s.TTLSeconds == nil || *s.TTLSeconds <= 0 {
		return DefaultSessionAffinityTTLSeconds
	}
	return *s.TTLSeconds
}

func (s *ChannelSessionAffinity) ContinuationGuardOrDefault() ChannelContinuationGuard {
	if s == nil || s.ContinuationGuard == "" {
		return ChannelContinuationGuardStrictReplay
	}
	return s.ContinuationGuard
}

type ChannelIDRewriteOp string

const (
	ChannelIDRewriteOpDerive ChannelIDRewriteOp = "derive"
	ChannelIDRewriteOpDelete ChannelIDRewriteOp = "delete"
)

type ChannelIDRewriteWhen string

const (
	ChannelIDRewriteWhenAlways      ChannelIDRewriteWhen = "always"
	ChannelIDRewriteWhenOnKeySwitch ChannelIDRewriteWhen = "on_key_switch"
)

type ChannelIDRewriteFormat string

const (
	ChannelIDRewriteFormatHex32           ChannelIDRewriteFormat = "hex32"
	ChannelIDRewriteFormatHex64           ChannelIDRewriteFormat = "hex64"
	ChannelIDRewriteFormatPrefixHex8      ChannelIDRewriteFormat = "prefix_hex8"
	ChannelIDRewriteFormatPrefixHex16     ChannelIDRewriteFormat = "prefix_hex16"
	ChannelIDRewriteFormatUUIDv8          ChannelIDRewriteFormat = "uuid_v8"
	ChannelIDRewriteFormatUUIDv5          ChannelIDRewriteFormat = "uuid_v5"
	ChannelIDRewriteFormatAnthropicUserID ChannelIDRewriteFormat = "anthropic_user_id"
)

type ChannelIDRewriteIfMissing string

const (
	ChannelIDRewriteIfMissingSkip     ChannelIDRewriteIfMissing = "skip"
	ChannelIDRewriteIfMissingGenerate ChannelIDRewriteIfMissing = "generate"
)

type ChannelIDRewriteRule struct {
	Target    string                    `json:"target"`
	Op        ChannelIDRewriteOp        `json:"op"`
	When      ChannelIDRewriteWhen      `json:"when,omitempty"`
	Format    ChannelIDRewriteFormat    `json:"format,omitempty"`
	Prefix    string                    `json:"prefix,omitempty"`
	IfMissing ChannelIDRewriteIfMissing `json:"ifMissing,omitempty"`
}
