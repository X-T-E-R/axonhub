package biz

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/looplj/axonhub/internal/objects"
)

// protectedIDRewriteHeaders are authentication, framing, and hop-by-hop HTTP
// headers that idRewriteRules are forbidden from overwriting or deleting.
var protectedIDRewriteHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"x-goog-api-key":      {},
	"api-key":             {},
	"host":                {},
	"content-length":      {},
	"transfer-encoding":   {},
	"connection":          {},
	"upgrade":             {},
	"te":                  {},
	"trailer":             {},
	"keep-alive":          {},
	"cookie":              {},
	"set-cookie":          {},
	"content-encoding":    {},
	"content-type":        {},
}

// continuationOwnedJSONPaths are stateful continuation cursor paths managed
// exclusively by sessionAffinity.continuationGuard and forbidden from raw
// idRewriteRules manipulation.
var continuationOwnedJSONPaths = map[string]struct{}{
	"previous_response_id": {},
}

type CompiledAnchorKind uint8

const (
	CompiledAnchorHeader CompiledAnchorKind = iota + 1
	CompiledAnchorJSON
)

type CompiledSessionAnchor struct {
	Raw          string
	Kind         CompiledAnchorKind
	HeaderKey    string // canonical HTTP header key
	JSONPath     string // literal dot path
	EscapedPath  string // gjson-escaped literal path
}

type CompiledSessionAffinity struct {
	Enabled           bool
	TTLSeconds        int
	Anchors           []CompiledSessionAnchor
	ContinuationGuard objects.ChannelContinuationGuard
}

type CompiledIDRewriteTargetKind uint8

const (
	CompiledIDRewriteTargetHeader CompiledIDRewriteTargetKind = iota + 1
	CompiledIDRewriteTargetJSON
)

type CompiledIDRewriteRule struct {
	Target        string
	Kind          CompiledIDRewriteTargetKind
	HeaderKey     string // canonical HTTP header key
	JSONPath      string // literal dot path
	EscapedPath   string // gjson/sjson-escaped literal path
	LeafKeyNeedle []byte // `"<leafSegment>"` precomputed for AVX2 fast path-miss check
	Op            objects.ChannelIDRewriteOp
	When          objects.ChannelIDRewriteWhen
	Format        objects.ChannelIDRewriteFormat
	Prefix        string
	IfMissing     objects.ChannelIDRewriteIfMissing
}

// CompiledIDRewritePlan is precompiled once at channel build time so the hot
// request path does zero rule parsing and zero body work when HasJSONRules is false.
type CompiledIDRewritePlan struct {
	HeaderRules  []CompiledIDRewriteRule
	JSONRules    []CompiledIDRewriteRule
	HasJSONRules bool
}

func (p *CompiledIDRewritePlan) Empty() bool {
	return p == nil || (len(p.HeaderRules) == 0 && len(p.JSONRules) == 0)
}

func ValidateChannelSessionAffinity(cfg *objects.ChannelSessionAffinity) error {
	_, err := CompileChannelSessionAffinity(cfg)
	return err
}

func CompileChannelSessionAffinity(cfg *objects.ChannelSessionAffinity) (*CompiledSessionAffinity, error) {
	if cfg == nil {
		return nil, nil
	}
	if cfg.TTLSeconds != nil {
		ttl := *cfg.TTLSeconds
		if ttl < objects.MinSessionAffinityTTLSeconds || ttl > objects.MaxSessionAffinityTTLSeconds {
			return nil, fmt.Errorf("sessionAffinity.ttlSeconds must be between %d and %d", objects.MinSessionAffinityTTLSeconds, objects.MaxSessionAffinityTTLSeconds)
		}
	}
	switch cfg.ContinuationGuard {
	case "", objects.ChannelContinuationGuardStrictReplay, objects.ChannelContinuationGuardOff:
	default:
		return nil, fmt.Errorf("sessionAffinity.continuationGuard %q is unsupported", cfg.ContinuationGuard)
	}
	if len(cfg.Anchors) > 16 {
		return nil, fmt.Errorf("sessionAffinity.anchors supports at most 16 entries")
	}
	compiledAnchors := make([]CompiledSessionAnchor, 0, len(cfg.Anchors))
	for i, raw := range cfg.Anchors {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, fmt.Errorf("sessionAffinity.anchors[%d] must not be empty", i)
		}
		if strings.HasPrefix(trimmed, "header:") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "header:"))
			if name == "" || !isValidHTTPHeaderName(name) {
				return nil, fmt.Errorf("sessionAffinity.anchors[%d] has invalid header name %q", i, name)
			}
			if _, protected := protectedIDRewriteHeaders[strings.ToLower(name)]; protected {
				return nil, fmt.Errorf("sessionAffinity.anchors[%d] cannot use protected header %q", i, name)
			}
			compiledAnchors = append(compiledAnchors, CompiledSessionAnchor{
				Raw:       trimmed,
				Kind:      CompiledAnchorHeader,
				HeaderKey: http.CanonicalHeaderKey(name),
			})
			continue
		}
		if strings.HasPrefix(trimmed, "json:") {
			path := strings.TrimSpace(strings.TrimPrefix(trimmed, "json:"))
			escaped, err := validateAndEscapeLiteralJSONPath(path)
			if err != nil {
				return nil, fmt.Errorf("sessionAffinity.anchors[%d]: %w", i, err)
			}
			compiledAnchors = append(compiledAnchors, CompiledSessionAnchor{
				Raw:         trimmed,
				Kind:        CompiledAnchorJSON,
				JSONPath:    path,
				EscapedPath: escaped,
			})
			continue
		}
		return nil, fmt.Errorf("sessionAffinity.anchors[%d] must start with 'header:' or 'json:'", i)
	}
	return &CompiledSessionAffinity{
		Enabled:           cfg.Enabled,
		TTLSeconds:        cfg.TTLSecondsOrDefault(),
		Anchors:           compiledAnchors,
		ContinuationGuard: cfg.ContinuationGuardOrDefault(),
	}, nil
}

func ValidateChannelIDRewriteRules(rules []objects.ChannelIDRewriteRule) error {
	_, err := CompileChannelIDRewriteRules(rules)
	return err
}

func CompileChannelIDRewriteRules(rules []objects.ChannelIDRewriteRule) (*CompiledIDRewritePlan, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	if len(rules) > 32 {
		return nil, fmt.Errorf("idRewriteRules supports at most 32 rules")
	}
	plan := &CompiledIDRewritePlan{}
	for i, rule := range rules {
		target := strings.TrimSpace(rule.Target)
		if target == "" {
			return nil, fmt.Errorf("idRewriteRules[%d].target is required", i)
		}
		var compiled CompiledIDRewriteRule
		compiled.Target = target

		switch {
		case strings.HasPrefix(target, "header:"):
			name := strings.TrimSpace(strings.TrimPrefix(target, "header:"))
			if name == "" || !isValidHTTPHeaderName(name) {
				return nil, fmt.Errorf("idRewriteRules[%d].target has invalid header name %q", i, name)
			}
			if _, protected := protectedIDRewriteHeaders[strings.ToLower(name)]; protected {
				return nil, fmt.Errorf("idRewriteRules[%d].target header %q is protected and cannot be rewritten", i, name)
			}
			compiled.Kind = CompiledIDRewriteTargetHeader
			compiled.HeaderKey = http.CanonicalHeaderKey(name)
		case strings.HasPrefix(target, "json:"):
			path := strings.TrimSpace(strings.TrimPrefix(target, "json:"))
			escaped, err := validateAndEscapeLiteralJSONPath(path)
			if err != nil {
				return nil, fmt.Errorf("idRewriteRules[%d].target: %w", i, err)
			}
			if _, owned := continuationOwnedJSONPaths[strings.ToLower(path)]; owned {
				return nil, fmt.Errorf("idRewriteRules[%d].target path %q is owned by sessionAffinity.continuationGuard and cannot be rewritten directly", i, path)
			}
			compiled.Kind = CompiledIDRewriteTargetJSON
			compiled.JSONPath = path
			compiled.EscapedPath = escaped
			leafSeg := path
			if idx := strings.LastIndexByte(path, '.'); idx >= 0 {
				leafSeg = path[idx+1:]
			}
			if leafSeg != "" {
				// Omit the leading '"' so JSON syntax quotes do not trip Go's bytes.Index
				// cutover heuristic; every literal JSON key "<leafSeg>" contains <leafSeg>".
				compiled.LeafKeyNeedle = []byte(leafSeg + `"`)
			}
		default:
			return nil, fmt.Errorf("idRewriteRules[%d].target must start with 'header:' or 'json:'", i)
		}

		switch rule.Op {
		case objects.ChannelIDRewriteOpDerive, objects.ChannelIDRewriteOpDelete:
			compiled.Op = rule.Op
		default:
			return nil, fmt.Errorf("idRewriteRules[%d].op %q is unsupported (use 'derive' or 'delete')", i, rule.Op)
		}

		switch rule.When {
		case "", objects.ChannelIDRewriteWhenAlways:
			compiled.When = objects.ChannelIDRewriteWhenAlways
		case objects.ChannelIDRewriteWhenOnKeySwitch:
			compiled.When = objects.ChannelIDRewriteWhenOnKeySwitch
		default:
			return nil, fmt.Errorf("idRewriteRules[%d].when %q is unsupported", i, rule.When)
		}

		switch rule.IfMissing {
		case "", objects.ChannelIDRewriteIfMissingSkip:
			compiled.IfMissing = objects.ChannelIDRewriteIfMissingSkip
		case objects.ChannelIDRewriteIfMissingGenerate:
			if compiled.Op == objects.ChannelIDRewriteOpDelete {
				return nil, fmt.Errorf("idRewriteRules[%d].ifMissing 'generate' is invalid for delete rules", i)
			}
			compiled.IfMissing = objects.ChannelIDRewriteIfMissingGenerate
		default:
			return nil, fmt.Errorf("idRewriteRules[%d].ifMissing %q is unsupported", i, rule.IfMissing)
		}

		if compiled.Op == objects.ChannelIDRewriteOpDerive {
			switch rule.Format {
			case objects.ChannelIDRewriteFormatHex32,
				objects.ChannelIDRewriteFormatHex64,
				objects.ChannelIDRewriteFormatPrefixHex8,
				objects.ChannelIDRewriteFormatPrefixHex16,
				objects.ChannelIDRewriteFormatUUIDv8,
				objects.ChannelIDRewriteFormatUUIDv5,
				objects.ChannelIDRewriteFormatAnthropicUserID:
				compiled.Format = rule.Format
			default:
				return nil, fmt.Errorf("idRewriteRules[%d].format %q is unsupported", i, rule.Format)
			}
			if len(rule.Prefix) > 32 {
				return nil, fmt.Errorf("idRewriteRules[%d].prefix must be at most 32 characters", i)
			}
			for _, r := range rule.Prefix {
				if r < 0x20 || r > 0x7e {
					return nil, fmt.Errorf("idRewriteRules[%d].prefix must contain printable ASCII only", i)
				}
			}
			compiled.Prefix = rule.Prefix
		}

		if compiled.Kind == CompiledIDRewriteTargetHeader {
			plan.HeaderRules = append(plan.HeaderRules, compiled)
		} else {
			plan.JSONRules = append(plan.JSONRules, compiled)
			plan.HasJSONRules = true
		}
	}
	return plan, nil
}

// validateAndEscapeLiteralJSONPath rejects wildcards, array queries, modifiers,
// and empty segments, accepting only literal dot-separated object field names.
func validateAndEscapeLiteralJSONPath(path string) (string, error) {
	if path == "" || len(path) > 256 {
		return "", fmt.Errorf("json path must be 1..256 characters")
	}
	if strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..") {
		return "", fmt.Errorf("json path %q must not have empty segments", path)
	}
	// Reject gjson wildcards, array selectors, modifiers, pipes, and escape tricks.
	for _, r := range path {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("json path %q contains invalid control or non-ASCII character", path)
		}
		switch r {
		case '*', '?', '#', '@', '[', ']', '|', '\\', ',', ':', '!', ' ':
			return "", fmt.Errorf("json path %q must be a literal dot path without wildcards or query operators", path)
		}
	}
	segments := strings.Split(path, ".")
	if len(segments) > 8 {
		return "", fmt.Errorf("json path %q exceeds maximum nesting depth of 8", path)
	}
	for _, seg := range segments {
		if seg == "" {
			return "", fmt.Errorf("json path %q has an empty segment", path)
		}
	}
	return path, nil
}
