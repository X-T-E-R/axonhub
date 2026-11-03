package biz

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func IsRawHTTPChannel(c *ent.Channel) bool {
	return c.Settings != nil && c.Settings.RawHTTP != nil && c.Settings.RawHTTP.Enabled
}

// ValidateRawHTTPChannel validates configuration, never the provider payload.
func ValidateRawHTTPChannel(baseURL string, models []string, settings *objects.ChannelSettings) error {
	if settings == nil || settings.RawHTTP == nil || !settings.RawHTTP.Enabled {
		return nil
	}
	raw := settings.RawHTTP
	if raw.Resource == "" || strings.TrimSpace(raw.Resource) != raw.Resource || !slices.Contains(models, raw.Resource) {
		return fmt.Errorf("raw HTTP resource must be an explicit supported model alias")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("raw HTTP base URL must be http(s) with a fixed host and optional path prefix, without credentials, query or fragment")
	}
	if !RawHTTPPathAllowed(u.Path) {
		return fmt.Errorf("invalid raw HTTP base path")
	}
	switch raw.AuthType {
	case "bearer":
		if raw.AuthHeader != "" {
			return fmt.Errorf("bearer authentication does not use a custom header")
		}
	case "header":
		name := http.CanonicalHeaderKey(raw.AuthHeader)
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("invalid raw HTTP authentication header")
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "content-type", "content-encoding", "accept-encoding", "connection", "upgrade", "trailer", "te", "transfer-encoding", "proxy-connection", "keep-alive", "proxy-authorization", "proxy-authenticate":
			return fmt.Errorf("raw HTTP authentication requires an end-to-end credential header")
		}
	default:
		return fmt.Errorf("raw HTTP auth type must be bearer or header")
	}
	return nil
}

func RawHTTPPathAllowed(path string) bool {
	if strings.Contains(path, "\\") {
		return false
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// RawHTTPAPIKey uses the existing enabled-key and key-selection policies.
func (c *Channel) RawHTTPAPIKey(ctx context.Context) string { return getAPIKeyProvider(c).Get(ctx) }

// ResolveRawHTTPChannel reads the current row so disabled channels and changed
// authorization/configuration take effect without waiting for a channel cache.
// The caller must authorize both the channel and logical resource before use.
func (svc *ChannelService) ResolveRawHTTPChannel(ctx context.Context, id int) (*Channel, error) {
	row, err := svc.entFromContext(ctx).Channel.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.Status != channel.StatusEnabled || !IsRawHTTPChannel(row) {
		return nil, fmt.Errorf("raw HTTP channel unavailable")
	}
	return svc.buildChannelWithOutbounds(row)
}
