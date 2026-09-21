package api

import (
	"net/http"
	"strings"

	"github.com/looplj/axonhub/llm/httpclient"
)

var blockedProviderResponseHeaders = map[string]struct{}{
	"Alt-Svc":            {},
	"Connection":         {},
	"Content-Encoding":   {},
	"Content-Length":     {},
	"Content-Type":       {},
	"Keep-Alive":         {},
	"Proxy-Authenticate": {},
	"Proxy-Connection":   {},
	"Te":                 {},
	"Trailer":            {},
	"Transfer-Encoding":  {},
	"Upgrade":            {},
}

// copyProviderResponseHeaders preserves protocol metadata that downstream
// clients need to continue a request. Pass-through channels may opt into all
// end-to-end headers, while transport framing, credentials, and cookies remain
// owned by AxonHub.
func copyProviderResponseHeaders(dst, src http.Header, fullPassThrough bool) {
	if len(src) == 0 {
		return
	}

	connectionHeaders := make(map[string]struct{})
	for _, value := range src.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if key := http.CanonicalHeaderKey(strings.TrimSpace(token)); key != "" {
				connectionHeaders[key] = struct{}{}
			}
		}
	}

	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if !shouldForwardProviderResponseHeader(canonical, fullPassThrough, connectionHeaders) {
			continue
		}
		dst[canonical] = append([]string(nil), values...)
	}
}

func shouldForwardProviderResponseHeader(key string, fullPassThrough bool, connectionHeaders map[string]struct{}) bool {
	if key == "" || httpclient.IsSensitiveHeader(key) {
		return false
	}
	if _, blocked := blockedProviderResponseHeaders[key]; blocked {
		return false
	}
	if _, connectionScoped := connectionHeaders[key]; connectionScoped {
		return false
	}
	if strings.HasPrefix(strings.ToLower(key), "ah-") {
		return false
	}
	if fullPassThrough {
		return true
	}
	return strings.HasPrefix(strings.ToLower(key), "x-codex-")
}
