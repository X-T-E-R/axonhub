package objects

import "strings"

// codexOfficialClientUAPrefixes matches official Codex client User-Agent prefixes
// aligned with Sub2 IsCodexOfficialClientRequestStrict (systems/sub2api/backend/internal/pkg/openai/request.go:24-40).
var codexOfficialClientUAPrefixes = []string{
	"codex_cli_rs/",
	"codex-tui/",
	"codex_vscode/",
	"codex_vscode_copilot/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
}

// codexOfficialClientOriginators matches the exact official Codex originator set from
// Sub2 (systems/sub2api/backend/internal/pkg/openai/request.go:48-58).
// Each literal corresponds 1:1 to codexOfficialClientUAPrefixes without the trailing slash.
var codexOfficialClientOriginators = []string{
	"codex_cli_rs",
	"codex-tui",
	"codex_vscode",
	"codex_vscode_copilot",
	"codex_app",
	"codex_chatgpt_desktop",
	"codex_atlas",
	"codex_exec",
	"codex_sdk_ts",
}

const codexOfficialClientFamilyPrefix = "codex "

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// IsCodexOfficialClientOriginator returns true if the originator or UA trailer product name
// belongs to an official Codex client, matching Sub2 IsCodexOfficialClientOriginator
// (exact match against codexOfficialClientOriginators or "Codex " family prefix; never a loose "codex_" prefix).
func IsCodexOfficialClientOriginator(originator string) bool {
	trimmed := strings.TrimSpace(originator)
	if trimmed == "" {
		return false
	}
	for _, exact := range codexOfficialClientOriginators {
		if strings.EqualFold(trimmed, exact) {
			return true
		}
	}
	return hasPrefixFold(trimmed, codexOfficialClientFamilyPrefix)
}

// codexUATrailerName extracts the clientInfo.name from the last parenthesized group
// of a codex-rs formatted User-Agent: "{orig}/{ver} ({os}; {arch}) {term} ({name}; {ver})".
// Aligned with Sub2 codexUATrailerName (systems/sub2api/backend/internal/pkg/openai/request.go:123-138),
// with one intentional tightening: the parenthesized trailer must contain a non-empty "; <version>"
// segment so a bare "(codex_cli_rs)" without version is not treated as a valid codex-rs trailer.
func codexUATrailerName(userAgent string) string {
	trimmed := strings.TrimSpace(userAgent)
	last := strings.LastIndex(trimmed, "(")
	if last < 0 {
		return ""
	}
	rest := trimmed[last+1:]
	closeIdx := strings.Index(rest, ")")
	if closeIdx < 0 {
		return ""
	}
	inner := strings.TrimSpace(rest[:closeIdx])
	name, version, ok := strings.Cut(inner, ";")
	if !ok || strings.TrimSpace(version) == "" {
		return ""
	}
	return strings.TrimSpace(name)
}

// IsCodexOfficialClientRequestStrict checks whether the inbound User-Agent header
// belongs to an official Codex client, matching Sub2 IsCodexOfficialClientRequestStrict:
// 1. Official UA prefix (codex_cli_rs/, codex-tui/, codex_vscode/, etc.)
// 2. Official Codex space family prefix ("Codex Desktop/...", "Codex CLI/...", etc.)
// 3. Official "(<name>; <version>)" UA trailer where <name> is an official Codex client name.
// This is a request-hygiene gate (not authentication): it inspects only User-Agent and never
// accepts Originator as a substitute for a missing or invalid User-Agent.
func IsCodexOfficialClientRequestStrict(userAgent string) bool {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return false
	}
	for _, prefix := range codexOfficialClientUAPrefixes {
		if hasPrefixFold(ua, prefix) {
			return true
		}
	}
	if hasPrefixFold(ua, codexOfficialClientFamilyPrefix) {
		return true
	}
	if trailerName := codexUATrailerName(ua); IsCodexOfficialClientOriginator(trailerName) {
		return true
	}
	return false
}
