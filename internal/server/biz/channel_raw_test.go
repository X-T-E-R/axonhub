package biz

import (
	"testing"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/stretchr/testify/require"
)

func TestRawHTTPConfigurationBoundary(t *testing.T) {
	for _, tc := range []struct {
		base, resource, auth, header string
		valid                        bool
	}{
		{"https://api.exa.ai", "exa-search", "header", "x-api-key", true},
		{"https://api.tavily.com", "exa-search", "bearer", "", true},
		{"http://127.0.0.1:8080/prefix%2Ffixed", "exa-search", "bearer", "", true},
		{"https://api.openai.com/v1/images", "exa-search", "bearer", "", true},
		{"file:///etc/passwd", "exa-search", "bearer", "", false},
		{"https://user:password@example.test", "exa-search", "bearer", "", false},
		{"https://example.test?token=x", "exa-search", "bearer", "", false},
		{"https://example.test#fragment", "exa-search", "bearer", "", false},
		{"https://example.test/a/%2e%2e", "exa-search", "bearer", "", false},
		{"https://example.test", "missing", "bearer", "", false},
		{"https://example.test", "exa-search", "body", "", false},
		{"https://example.test", "exa-search", "header", "bad\r\nheader", false},
		{"https://example.test", "exa-search", "header", "Host", false},
		{"https://example.test", "exa-search", "header", "Content-Type", false},
		{"https://example.test", "exa-search", "header", "Connection", false},
	} {
		err := ValidateRawHTTPChannel(tc.base, []string{"exa-search"}, &objects.ChannelSettings{RawHTTP: &objects.ChannelRawHTTP{Enabled: true, Resource: tc.resource, AuthType: tc.auth, AuthHeader: tc.header}})
		if tc.valid {
			require.NoError(t, err, tc.base)
		} else {
			require.Error(t, err, tc.base)
		}
	}
}

func TestRawHTTPSettingsEditorsPreserveProtocol(t *testing.T) {
	raw := &objects.ChannelRawHTTP{Enabled: true, Resource: "search", AuthType: "bearer"}
	current := &objects.ChannelSettings{RawHTTP: raw}
	next := mergeChannelSettingsForUpdate(current, &objects.ChannelSettings{DisableRetries: true})
	require.Equal(t, raw, next.RawHTTP)
	next.RawHTTP.Resource = "changed"
	require.Equal(t, "search", raw.Resource, "settings updates must not mutate cached data")
	next = mergeChannelSettingsForUpdate(current, &objects.ChannelSettings{RawHTTP: &objects.ChannelRawHTTP{Enabled: false}})
	require.False(t, next.RawHTTP.Enabled)
}
