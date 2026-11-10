package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
)

type RawHTTPHandlersParams struct {
	fx.In
	ChannelService *biz.ChannelService
	Ent            *ent.Client
}
type RawHTTPHandlers struct {
	channels *biz.ChannelService
	db       *ent.Client
}

func NewRawHTTPHandlers(p RawHTTPHandlersParams) *RawHTTPHandlers {
	return &RawHTTPHandlers{p.ChannelService, p.Ent}
}

func (h *RawHTTPHandlers) Proxy(c *gin.Context) {
	ctx := c.Request.Context()
	key, ok := contexts.GetAPIKey(ctx)
	if !ok || !authz.HasScope(ctx, scopes.ScopeWriteRequests) {
		JSONError(c, http.StatusForbidden, errors.New("request access required"))
		return
	}
	id, err := strconv.Atoi(c.Param("channelID"))
	if err != nil || id <= 0 {
		JSONError(c, 404, errors.New("raw HTTP channel unavailable"))
		return
	}
	// The only bypass is retrieval of a fixed channel; both authorization upper
	// bounds are checked before its address or credentials are used.
	row, err := h.channels.ResolveRawHTTPChannel(authz.WithSystemBypass(ctx, "raw-http-channel-lookup"), id)
	if err != nil || !biz.KeyAllowsChannel(key, id, row.Tags) {
		JSONError(c, http.StatusForbidden, errors.New("raw HTTP channel unavailable"))
		return
	}
	raw := row.Settings.RawHTTP
	if !biz.KeyAllowsModel(key, raw.Resource) {
		JSONError(c, http.StatusForbidden, errors.New("raw HTTP resource is not allowed"))
		return
	}
	suffix := strings.TrimPrefix(c.Request.URL.EscapedPath(), "/raw/"+c.Param("channelID"))
	decoded, err := url.PathUnescape(suffix)
	if err != nil || !strings.HasPrefix(suffix, "/") || !biz.RawHTTPPathAllowed(decoded) {
		JSONError(c, http.StatusBadRequest, errors.New("invalid raw HTTP path"))
		return
	}
	ch, err := h.channels.OpenRawHTTPChannel(ctx, row)
	if err != nil {
		var ownerErr *biz.ExecutionOwnerError
		if errors.As(err, &ownerErr) {
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "execution_owner_mismatch", "message": "Raw HTTP channel does not execute on this node."}, "execution": "not_executed"})
			return
		}
		JSONError(c, http.StatusForbidden, errors.New("raw HTTP channel unavailable"))
		return
	}
	base, _ := url.Parse(ch.BaseURL) // validated when the current channel is loaded
	escapedPrefix := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = strings.TrimRight(base.Path, "/") + decoded
	base.RawPath = escapedPrefix + suffix
	base.RawQuery = c.Request.URL.RawQuery
	base.ForceQuery = c.Request.URL.ForceQuery
	upstreamKey := h.channels.RawHTTPAPIKey(ctx, ch)
	native := ch.HTTPClient.GetNativeClient()
	transport := native.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	// Preserve the channel proxy/TLS policy but own framing and compression. A
	// fresh, non-reused connection also prevents Transport's implicit replay on
	// stale pooled connections, even for opaque idempotent provider operations.
	tr := transport.(*http.Transport).Clone()
	tr.DisableCompression = true
	defer tr.CloseIdleConnections()
	started := time.Now()
	status := request.StatusFailed
	stream := false
	defer func() {
		if ctx.Err() != nil {
			status = request.StatusCanceled
		}
		h.observe(context.WithoutCancel(ctx), key, id, raw.Resource, c.ClientIP(), status, stream, time.Since(started))
	}()
	proxy := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL = base
			p.Out.Host = base.Host
			p.Out.GetBody = nil
			p.Out.Trailer = nil
			for _, name := range middleware.DefaultAPIKeyConfig.Headers {
				p.Out.Header.Del(name)
			}
			for _, name := range []string{"Connection", "Upgrade", "Te", "Cookie", "X-Project-ID", "X-Channel-ID", "X-Forwarded-Host", "X-Forwarded-For", "X-Forwarded-Proto", "Forwarded"} {
				p.Out.Header.Del(name)
			}
			p.Out.Header.Del(raw.AuthHeader)
			if raw.AuthType == "bearer" {
				p.Out.Header.Set("Authorization", "Bearer "+upstreamKey)
			} else {
				p.Out.Header.Set(raw.AuthHeader, upstreamKey)
			}
		},
		ModifyResponse: func(r *http.Response) error {
			stream = strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream")
			// Credentials are never returned even if an upstream reflects them as headers.
			for _, name := range middleware.DefaultAPIKeyConfig.Headers {
				r.Header.Del(name)
			}
			r.Header.Del(raw.AuthHeader)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream connection failed", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(c.Writer, c.Request)
	if c.Writer.Status() < 400 {
		status = request.StatusCompleted
	}
}

// Use the existing request inventory, storing only metadata. Unknown usage has
// no UsageLog row, so it remains absent rather than fabricated zero tokens.
func (h *RawHTTPHandlers) observe(ctx context.Context, key *ent.APIKey, channelID int, resource, ip string, status request.Status, stream bool, latency time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = authz.WithSystemBypass(ctx, "raw-http-request-observation")
	now := time.Now()
	omitted := objects.Disposition{Intent: "omit", Location: "none", Outcome: "omitted", CapturedAt: now}
	_, err := h.db.Request.Create().SetProjectID(key.ProjectID).SetAPIKeyID(key.ID).SetChannelID(channelID).
		SetModelID(resource).SetFormat("http/raw").SetSource(request.SourceAPI).SetStatus(status).SetStream(stream).SetClientIP(ip).
		SetMetricsLatencyMs(latency.Milliseconds()).SetRequestBody(objects.JSONRawMessage("null")).
		SetEvidenceDisposition(&objects.EvidenceDisposition{Version: 1, RequestBody: omitted, ResponseBody: omitted, ResponseChunks: omitted}).Save(ctx)
	if err != nil {
		log.Warn(ctx, "raw HTTP request metadata could not be stored")
	}
}
