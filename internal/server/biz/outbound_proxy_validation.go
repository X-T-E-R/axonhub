package biz

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// OutboundProxyKind identifies whether a proxy URL is literal or contains
// credential/session/channel placeholders.
type OutboundProxyKind string

const (
	OutboundProxyKindStatic          OutboundProxyKind = "static"
	OutboundProxyKindDynamicTemplate OutboundProxyKind = "dynamic_template"
)

// OutboundProxyStatus is the administrative enablement state for a proxy.
type OutboundProxyStatus string

const (
	OutboundProxyStatusEnabled  OutboundProxyStatus = "enabled"
	OutboundProxyStatusDisabled OutboundProxyStatus = "disabled"
)

var outboundProxyTemplateTokens = []string{"{credential_id}", "{session}", "{channel_id}"}

func isOutboundProxyTemplate(raw string) bool {
	for _, token := range outboundProxyTemplateTokens {
		if strings.Contains(raw, token) {
			return true
		}
	}
	return false
}

// ValidateOutboundProxyURL checks the syntax and supported scheme of a managed
// proxy endpoint. It deliberately does not reject private proxy hosts: local
// Resin sidecars are valid proxy endpoints. Probe destinations are validated
// separately and are never taken from this URL.
func ValidateOutboundProxyURL(kind OutboundProxyKind, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("proxy URL is required")
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("proxy URL is invalid")
	}
	if kind != OutboundProxyKindStatic && kind != OutboundProxyKindDynamicTemplate {
		return fmt.Errorf("unsupported outbound proxy kind")
	}

	hasTemplate := isOutboundProxyTemplate(raw)
	if kind == OutboundProxyKindStatic && hasTemplate {
		return fmt.Errorf("static proxy URL must not contain template placeholders")
	}
	unknownCheck := raw
	for _, token := range outboundProxyTemplateTokens {
		unknownCheck = strings.ReplaceAll(unknownCheck, token, "")
	}
	if strings.ContainsAny(unknownCheck, "{}") {
		return fmt.Errorf("proxy URL contains an unsupported template placeholder")
	}

	parseable := raw
	for _, token := range outboundProxyTemplateTokens {
		parseable = strings.ReplaceAll(parseable, token, "probe")
	}
	proxyURL, err := url.Parse(parseable)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" || proxyURL.Hostname() == "" || proxyURL.Opaque != "" {
		return fmt.Errorf("proxy URL must be an absolute URL with a host")
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy URL scheme must be http, https, socks5, or socks5h")
	}
	if proxyURL.Fragment != "" {
		return fmt.Errorf("proxy URL must not contain a fragment")
	}
	if strings.HasSuffix(proxyURL.Host, ":") {
		return fmt.Errorf("proxy URL port is invalid")
	}
	if port := proxyURL.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return fmt.Errorf("proxy URL port is invalid")
		}
	}

	// Template substitutions may add authentication/path/query data, but must
	// not redirect the proxy connection to a different endpoint by changing the
	// scheme, host, or port.
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return fmt.Errorf("proxy URL must be an absolute URL with a host")
	}
	if strings.Contains(raw[:schemeEnd], "{") {
		return fmt.Errorf("proxy URL placeholders are not allowed in the scheme")
	}
	authorityEnd := len(raw)
	if relative := strings.IndexAny(raw[schemeEnd+3:], "/?#"); relative >= 0 {
		authorityEnd = schemeEnd + 3 + relative
	}
	authority := raw[schemeEnd+3 : authorityEnd]
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if strings.ContainsAny(authority, "{}") {
		return fmt.Errorf("proxy URL placeholders are not allowed in the host or port")
	}

	return nil
}

// RenderOutboundProxyURL substitutes safe, URL-escaped values into a managed
// proxy URL template. Template markers are accepted only outside the scheme,
// host, and port so request data cannot change the configured proxy endpoint.
func RenderOutboundProxyURL(raw, credentialID, session, channelID string) (string, error) {
	kind := OutboundProxyKindStatic
	if isOutboundProxyTemplate(raw) {
		kind = OutboundProxyKindDynamicTemplate
	}
	if err := ValidateOutboundProxyURL(kind, raw); err != nil {
		return "", err
	}

	escape := func(value string) string {
		// QueryEscape encodes URL delimiters; change '+' to %20 because '+' is
		// a literal plus in userinfo and path components, not a space.
		return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
	}
	values := map[string]string{
		"{credential_id}": escape(credentialID),
		"{session}":       escape(session),
		"{channel_id}":    escape(channelID),
	}
	rendered := raw
	for _, token := range outboundProxyTemplateTokens {
		rendered = strings.ReplaceAll(rendered, token, values[token])
	}

	parsed, err := url.Parse(rendered)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("rendered proxy URL is invalid")
	}

	return parsed.String(), nil
}

// maskOutboundProxyURL returns a display-only URL with userinfo, query values,
// and path contents removed; this also protects against secrets placed outside
// the conventional password field.
func maskOutboundProxyURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[redacted]"
	}
	if parsed.User != nil {
		parsed.User = url.User("redacted")
	}
	if parsed.Path != "" || parsed.RawPath != "" {
		parsed.Path = "/redacted"
		parsed.RawPath = ""
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""

	return parsed.String()
}
