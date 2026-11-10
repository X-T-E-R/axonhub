package objects

import "testing"

func TestIsCodexOfficialClientRequestStrict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		userAgent string
		want      bool
	}{
		{
			name:      "codex cli rs windows",
			userAgent: "codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64) xterm-256color",
			want:      true,
		},
		{
			name:      "codex tui",
			userAgent: "codex-tui/0.104.0 (Mac OS 26.3.0; arm64) iTerm.app/3.6.6",
			want:      true,
		},
		{
			name:      "codex desktop space family",
			userAgent: "Codex Desktop/0.142.3 (Windows 10.0.22635; x86_64) unknown (Codex Desktop; 26.623.61825)",
			want:      true,
		},
		{
			name:      "codex cli space family",
			userAgent: "Codex CLI/0.118.0 (Linux; x86_64)",
			want:      true,
		},
		{
			name:      "codex trailer fallback",
			userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (Codex Desktop; 26.304.20756)",
			want:      true,
		},
		{
			name:      "codex originator override with official codex-tui trailer",
			userAgent: "cccc/0.104.0 (Mac OS 26.3.0; arm64) iTerm.app/3.6.6 (codex-tui; 0.104.0)",
			want:      true,
		},
		{
			name:      "codex originator override with official codex_cli_rs trailer",
			userAgent: "custom_wrapper/1.0 (Linux; x86_64) (codex_cli_rs; 0.159.2)",
			want:      true,
		},
		{
			name:      "unknown codex_ prefix in trailer is rejected",
			userAgent: "curl/8.7.1 (codex_unknown; 1.0.0)",
			want:      false,
		},
		{
			name:      "unknown codex_ prefix in primary UA is rejected",
			userAgent: "codex_unknown/1.0.0 (Linux; x86_64)",
			want:      false,
		},
		{
			name:      "codex vscode prefix",
			userAgent: "codex_vscode/1.2.3 (Windows 10.0.22635; x86_64)",
			want:      true,
		},
		{
			name:      "codex sdk ts prefix",
			userAgent: "codex_sdk_ts/0.1.0",
			want:      true,
		},
		{
			name:      "missing user agent",
			userAgent: "",
			want:      false,
		},
		{
			name:      "whitespace user agent",
			userAgent: "   \t  ",
			want:      false,
		},
		{
			name:      "opencode client",
			userAgent: "opencode/1.2.15 ai-sdk/provider-utils/3.0.20 runtime/bun/1.3.9",
			want:      false,
		},
		{
			name:      "curl client",
			userAgent: "curl/8.7.1",
			want:      false,
		},
		{
			name:      "python httpx client",
			userAgent: "python-httpx/0.27.0",
			want:      false,
		},
		{
			name:      "openai node sdk",
			userAgent: "OpenAI/JS 4.67.1",
			want:      false,
		},
		{
			name:      "non-codex trailer",
			userAgent: "CustomWrapper/1.0 (Mac OS 26.3.0; arm64) (Other Desktop; 1.0.0)",
			want:      false,
		},
		{
			name:      "trailer without version",
			userAgent: "CustomWrapper/1.0 (Codex Desktop;   )",
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCodexOfficialClientRequestStrict(tc.userAgent); got != tc.want {
				t.Fatalf("IsCodexOfficialClientRequestStrict(%q) = %v, want %v", tc.userAgent, got, tc.want)
			}
		})
	}
}

func TestChannelPoliciesAllowsRequestFormat(t *testing.T) {
	t.Parallel()

	unconfigured := ChannelPolicies{}
	for _, format := range []string{"openai/chat_completions", "openai/responses", "openai/responses_compact", "anthropic/messages", "gemini/contents"} {
		if !unconfigured.AllowsRequestFormat(format) {
			t.Fatalf("unconfigured policy should allow %q", format)
		}
	}

	allowResponsesOnly := ChannelPolicies{
		AllowRequestFormats: []string{"openai/responses", "openai/responses_compact"},
		DenyRequestFormats:  []string{"openai/chat_completions", "anthropic/messages"},
	}
	if !allowResponsesOnly.AllowsRequestFormat("openai/responses") {
		t.Fatal("expected openai/responses to be allowed")
	}
	if !allowResponsesOnly.AllowsRequestFormat("openai/responses_compact") {
		t.Fatal("expected openai/responses_compact to be allowed")
	}
	for _, denied := range []string{"openai/chat_completions", "anthropic/messages", "gemini/contents", "ollama/chat"} {
		if allowResponsesOnly.AllowsRequestFormat(denied) {
			t.Fatalf("expected %q to be denied", denied)
		}
	}
}
