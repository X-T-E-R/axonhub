package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

// resolveInfrastructureControlURL validates and returns the operator-configured
// managed control service endpoint (e.g. http://127.0.0.1:18751). It never spawns
// child processes or accepts target overrides from HTTP request parameters.
func resolveInfrastructureControlURL() (string, *url.URL, error) {
	raw := strings.TrimSpace(os.Getenv("AXONHUB_INFRA_CONTROL_URL"))
	if raw == "" {
		return "", nil, errors.New("AXONHUB_INFRA_CONTROL_URL is not configured")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", nil, errors.New("invalid AXONHUB_INFRA_CONTROL_URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("AXONHUB_INFRA_CONTROL_URL must not contain credentials, query or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	isLoopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback) {
		return "", nil, errors.New("AXONHUB_INFRA_CONTROL_URL must use https or loopback http")
	}
	cleanBase := strings.TrimRight(parsed.String(), "/")
	return cleanBase, parsed, nil
}

func requireSystemChannelScope(c *gin.Context, scope scopes.ScopeSlug) bool {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	if err := authz.RequireScope(ctx, scope); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return false
	}
	principal, _ := authz.GetPrincipal(ctx)
	if principal.IsSystem() || principal.IsTest() {
		return true
	}
	user, ok := contexts.GetUser(ctx)
	if !ok {
		JSONError(c, http.StatusForbidden, errors.New("administrator access required"))
		return false
	}
	if user.IsOwner || slices.Contains(user.Scopes, string(scope)) || slices.Contains(user.Scopes, "*") {
		return true
	}
	for _, role := range user.Edges.Roles {
		if role.IsSystemRole() && (slices.Contains(role.Scopes, string(scope)) || slices.Contains(role.Scopes, "*")) {
			return true
		}
	}
	JSONError(c, http.StatusForbidden, fmt.Errorf("system-level %s required", scope))
	return false
}

// axonBridgeEnvelope carries only bounded cluster placement and Worker ACK metadata
// (typically < 300 bytes) rather than serializing the full model manifest on every call.
type axonBridgeEnvelope struct {
	ClusterEnabled   bool                    `json:"clusterEnabled"`
	ControlNode      string                  `json:"controlNode"`
	PlacementVersion int64                   `json:"placementVersion"`
	Machines         []string                `json:"machines"`
	WorkerAck        *biz.PlacementWorkerAck `json:"workerAck,omitempty"`
}

func (h *ClusterHandlers) buildBridgeHeader(ctx context.Context) string {
	if h.channels == nil {
		return ""
	}
	placementCtx, err := h.channels.ReadInfrastructurePlacementContext(ctx)
	if err != nil {
		return ""
	}
	env := axonBridgeEnvelope{
		ClusterEnabled:   placementCtx.ClusterEnabled,
		ControlNode:      placementCtx.ControlNode,
		PlacementVersion: placementCtx.PlacementVersion,
		Machines:         placementCtx.Machines,
		WorkerAck:        placementCtx.WorkerAck,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

var infraHTTPClient = &http.Client{
	Timeout: 25 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (h *ClusterHandlers) proxyInfrastructure(c *gin.Context, method, upstreamPath string) {
	baseURL, parsedBase, err := resolveInfrastructureControlURL()
	if err != nil {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"code":    "infrastructure_control_unconfigured",
				"message": "Managed infrastructure control endpoint is not configured on this control node.",
			},
		})
		return
	}
	var bodyReader io.Reader
	if method != http.MethodGet && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)
		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			JSONError(c, http.StatusBadRequest, errors.New("invalid or oversized request body"))
			return
		}
		bodyReader = bytes.NewReader(rawBody)
	}
	targetURL := baseURL + "/api/v1/" + strings.TrimLeft(upstreamPath, "/")
	if c.Request.URL.Query().Get("refresh") == "1" || c.Request.URL.Query().Get("source") != "" {
		q := url.Values{}
		if c.Request.URL.Query().Get("refresh") == "1" {
			q.Set("refresh", "1")
		}
		if src := c.Request.URL.Query().Get("source"); src != "" {
			q.Set("source", src)
		}
		targetURL += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, targetURL, bodyReader)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("failed to construct control request"))
		return
	}
	req.Host = parsedBase.Host
	req.Header.Set("Content-Type", "application/json")
	if ctxHeader := h.buildBridgeHeader(c.Request.Context()); ctxHeader != "" {
		req.Header.Set("X-Axon-Context", ctxHeader)
	}
	resp, err := infraHTTPClient.Do(req)
	if err != nil {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"code":    "infrastructure_control_unavailable",
				"message": "Managed infrastructure control service is unreachable.",
			},
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"code":    "infrastructure_control_redirect_rejected",
				"message": "Upstream redirects are not permitted on the infrastructure control channel.",
			},
		})
		return
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{
				"code":    "infrastructure_control_bad_response",
				"message": "Invalid response from managed infrastructure control service.",
			},
		})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(resp.StatusCode, "application/json; charset=utf-8", payload)
}

func (h *ClusterHandlers) InfrastructureState(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodGet, "state")
}

func (h *ClusterHandlers) InfrastructureTopology(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodGet, "topology")
}

func (h *ClusterHandlers) InfrastructureGetNode(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodGet, "nodes/"+c.Param("id"))
}

func (h *ClusterHandlers) InfrastructurePutNode(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPut, "nodes/"+c.Param("id"))
}

func (h *ClusterHandlers) InfrastructureNodeAction(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPost, "nodes/"+c.Param("id")+"/actions/"+c.Param("step"))
}

func (h *ClusterHandlers) InfrastructurePutRule(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPut, "rules/"+c.Param("id"))
}

func (h *ClusterHandlers) InfrastructureDeleteRule(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodDelete, "rules/"+c.Param("id"))
}

func (h *ClusterHandlers) InfrastructurePutDraft(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPut, "draft")
}

func (h *ClusterHandlers) InfrastructureValidate(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPost, "validate")
}

func (h *ClusterHandlers) InfrastructureExplain(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPost, "explain")
}

func (h *ClusterHandlers) InfrastructureReleases(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodGet, "releases")
}

func (h *ClusterHandlers) InfrastructureGetRelease(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodGet, "releases/"+c.Param("id"))
}

func (h *ClusterHandlers) InfrastructurePreviewRelease(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeReadChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPost, "releases/preview")
}

func (h *ClusterHandlers) InfrastructureCreateRelease(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPost, "releases")
}

func (h *ClusterHandlers) InfrastructureActivateRelease(c *gin.Context) {
	if !requireSystemChannelScope(c, scopes.ScopeWriteChannels) {
		return
	}
	h.proxyInfrastructure(c, http.MethodPost, "releases/"+c.Param("id")+"/activate")
}
