package objects

import "strings"

// codexOfficialClientUAPrefixes matches official Codex client User-Agent prefixes
// aligned with Sub2 IsCodexOfficialClientRequestStrict.
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

const codexOfficialClientFamilyPrefix = "codex "

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// IsCodexOfficialClientOriginator returns true if the originator or UA trailer product name
// belongs to an official Codex client.
func IsCodexOfficialClientOriginator(originator string) bool {
	trimmed := strings.TrimSpace(originator)
	if trimmed == "" {
		return false
	}
	return strings.EqualFold(trimmed, "codex-tui") ||
		hasPrefixFold(trimmed, "codex_") ||
		hasPrefixFold(trimmed, codexOfficialClientFamilyPrefix)
}

func codexUATrailerName(userAgent string) string {
	trimmed := strings.TrimSpace(userAgent)
	if !strings.HasSuffix(trimmed, ")") {
		return ""
	}
	openIdx := strings.LastIndex(trimmed, "(")
	if openIdx < 0 {
		return ""
	}
	trailer := trimmed[openIdx+1 : len(trimmed)-1]
	name, version, ok := strings.Cut(trailer, ";")
	if !ok {
		return ""
	}
	if strings.TrimSpace(version) == "" {
		return ""
	}
	return strings.TrimSpace(name)
}

// IsCodexOfficialClientRequestStrict checks whether the inbound User-Agent header
// belongs to an official Codex client, matching Sub2 IsCodexOfficialClientRequestStrict:
// 1. Official UA prefix (codex_cli_rs/, codex-tui/, codex_vscode/, etc.)
// 2. Official Codex space family prefix ("Codex Desktop/...", "Codex CLI/...", etc.)
// 3. Official "(<name>; <version>)" UA trailer where <name> is an official Codex client name.
// It inspects only User-Agent and never accepts Originator as a substitute for a missing or invalid User-Agent.
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
