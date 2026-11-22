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
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xjson"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
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
	ctx := contexts.EnsureContainer(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
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
	// The only bypass is retrieval of a fixed channel; channel kind and both
	// authorization upper bounds are checked before status, address, or
	// credentials are inspected so unauthorized callers cannot probe whether a
	// forbidden channel is enabled or disabled.
	row, err := h.channels.ResolveRawHTTPChannel(authz.WithSystemBypass(ctx, "raw-http-channel-lookup"), id)
	if err != nil {
		if ent.IsNotFound(err) {
			JSONError(c, http.StatusForbidden, errors.New("raw HTTP channel unavailable"))
			return
		}
		JSONError(c, http.StatusServiceUnavailable, errors.New("raw HTTP channel unavailable"))
		return
	}
	if !biz.IsRawHTTPChannel(row) || !biz.KeyAllowsChannel(key, id, row.Tags) {
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
	if row.Status != channel.StatusEnabled {
		JSONError(c, http.StatusServiceUnavailable, errors.New("raw HTTP channel unavailable"))
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
		JSONError(c, http.StatusServiceUnavailable, errors.New("raw HTTP channel unavailable"))
		return
	}
	base, _ := url.Parse(ch.BaseURL) // validated when the current channel is loaded
	escapedPrefix := strings.TrimRight(base.EscapedPath(), "/")
	base.Path = strings.TrimRight(base.Path, "/") + decoded
	base.RawPath = escapedPrefix + suffix
	base.RawQuery = c.Request.URL.RawQuery
	base.ForceQuery = c.Request.URL.ForceQuery

	started := time.Now()
	status := request.StatusFailed
	stream := false
	defer func() {
		canceled := ctx.Err() != nil
		contexts.FinishChannelCredentialAttempt(ctx, 0, false, true)
		if canceled {
			status = request.StatusCanceled
		}
		h.observe(context.WithoutCancel(ctx), key, id, raw.Resource, c.ClientIP(), status, stream, time.Since(started), httpclient.CorrelationHeaders(c.Request.Header))
	}()

	upstreamKey := h.channels.RawHTTPAPIKey(ctx, ch)
	if upstreamKey == "" {
		JSONError(c, http.StatusServiceUnavailable, errors.New("raw HTTP channel has no eligible credentials"))
		return
	}

	maxAttempts := min(ch.RoutableCredentialCount(), rawHTTPMaxAttempts)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	retryTransport := &rawHTTPRetryTransport{
		channels:       h.channels,
		channel:        ch,
		defaultClient:  ch.HTTPClient,
		authType:       raw.AuthType,
		authHeader:     raw.AuthHeader,
		initialKey:     upstreamKey,
		maxAttempts:    maxAttempts,
		disableRetries: ch.Settings != nil && ch.Settings.DisableRetries,
	}
	defer retryTransport.CloseIdleConnections()

	proxy := &httputil.ReverseProxy{
		Transport:     retryTransport,
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
			p.Out.Header.Del("Ah-Request-Id")
			p.Out.Header.Set(httpclient.ClientRequestIDHeader, c.Request.Header.Get(httpclient.ClientRequestIDHeader))
			setRawHTTPAuth(p.Out.Header, raw.AuthType, raw.AuthHeader, upstreamKey)
		},
		ModifyResponse: func(r *http.Response) error {
			stream = strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream")
			// Credentials are never returned even if an upstream reflects them as headers.
			for _, name := range middleware.DefaultAPIKeyConfig.Headers {
				r.Header.Del(name)
			}
			r.Header.Del(raw.AuthHeader)
			// Upstream reflections must not override the ingress correlation IDs.
			r.Header.Del(httpclient.ClientRequestIDHeader)
			r.Header.Del("Ah-Request-Id")
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
func (h *RawHTTPHandlers) observe(ctx context.Context, key *ent.APIKey, channelID int, resource, ip string, status request.Status, stream bool, latency time.Duration, headers http.Header) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx = authz.WithSystemBypass(ctx, "raw-http-request-observation")
	now := time.Now()
	omitted := objects.Disposition{Intent: "omit", Location: "none", Outcome: "omitted", CapturedAt: now}
	requestHeaders, _ := xjson.Marshal(headers)
	_, err := h.db.Request.Create().SetProjectID(key.ProjectID).SetAPIKeyID(key.ID).SetChannelID(channelID).
		SetModelID(resource).SetFormat("http/raw").SetSource(request.SourceAPI).SetStatus(status).SetStream(stream).SetClientIP(ip).
		SetMetricsLatencyMs(latency.Milliseconds()).SetRequestHeaders(requestHeaders).SetRequestBody(objects.JSONRawMessage("null")).
		SetEvidenceDisposition(&objects.EvidenceDisposition{Version: 1, RequestBody: omitted, ResponseBody: omitted, ResponseChunks: omitted}).Save(ctx)
	if err != nil {
		log.Warn(ctx, "raw HTTP request metadata could not be stored")
	}
}
