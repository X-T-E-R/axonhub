package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

type managedInfrastructureBridge struct {
	mu      sync.Mutex
	baseURL string
	cmd     *exec.Cmd
}

var defaultInfraBridge = &managedInfrastructureBridge{}

func resolveControlBridgeScript() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("AXONHUB_INFRA_BRIDGE_SCRIPT")); explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if ok {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../../axon-edge-worker/control/axon-bridge.mjs"))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	cwd, err := os.Getwd()
	if err == nil {
		for _, rel := range []string{
			"../axon-edge-worker/control/axon-bridge.mjs",
			"../../axon-edge-worker/control/axon-bridge.mjs",
			"../../../axon-edge-worker/control/axon-bridge.mjs",
			"systems/axon-edge-worker/control/axon-bridge.mjs",
		} {
			candidate := filepath.Clean(filepath.Join(cwd, rel))
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	return "", errors.New("axon-edge-worker control/axon-bridge.mjs not found")
}

func (b *managedInfrastructureBridge) endpoint() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("AXONHUB_INFRA_CONTROL_URL")); explicit != "" {
		return strings.TrimRight(explicit, "/"), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.baseURL != "" && b.cmd != nil && b.cmd.ProcessState == nil {
		return b.baseURL, nil
	}
	script, err := resolveControlBridgeScript()
	if err != nil {
		return "", err
	}
	dataDir := strings.TrimSpace(os.Getenv("AXONHUB_INFRA_DATA_DIR"))
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), fmt.Sprintf("axonhub-infra-bridge-%d", os.Getpid()))
	}
	args := []string{script, "--port", "0", "--data", dataDir}
	if snap := strings.TrimSpace(os.Getenv("TASK51_KOMODO_SNAPSHOT_FILE")); snap != "" {
		args = append(args, "--komodo-snapshot", snap)
	}
	if nm := strings.TrimSpace(os.Getenv("TASK51_KOMODO_NODE_MAP_FILE")); nm != "" {
		args = append(args, "--node-map", nm)
	}
	cmd := exec.Command("node", args...)
	cmd.Dir = filepath.Dir(filepath.Dir(script))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start infrastructure bridge: %w", err)
	}
	type startMsg struct {
		URL string `json:"url"`
		Err error
	}
	ch := make(chan startMsg, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- startMsg{Err: fmt.Errorf("bridge reader panic: %v", r)}
			}
		}()
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			var parsed struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &parsed); err != nil {
				ch <- startMsg{Err: err}
				return
			}
			ch <- startMsg{URL: strings.TrimRight(parsed.URL, "/")}
			return
		}
		if err := scanner.Err(); err != nil {
			ch <- startMsg{Err: err}
			return
		}
		ch <- startMsg{Err: fmt.Errorf("bridge exited before ready: %s", strings.TrimSpace(stderr.String()))}
	}()

	select {
	case msg := <-ch:
		if msg.Err != nil || msg.URL == "" {
			_ = cmd.Process.Kill()
			return "", fmt.Errorf("infrastructure bridge failed: %v", msg.Err)
		}
		b.cmd = cmd
		b.baseURL = msg.URL
		return b.baseURL, nil
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		return "", errors.New("infrastructure bridge startup timed out")
	}
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

type axonBridgeEnvelope struct {
	ClusterEnabled   bool                    `json:"clusterEnabled"`
	ControlNode      string                  `json:"controlNode"`
	PlacementVersion int64                   `json:"placementVersion"`
	Machines         []string                `json:"machines"`
	WorkerAck        *biz.PlacementWorkerAck `json:"workerAck,omitempty"`
	Manifest         *biz.ExecutionManifest  `json:"manifest,omitempty"`
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
	if h.models != nil {
		if m, err := h.models.BuildExecutionManifest(ctx); err == nil {
			env.Manifest = m
		}
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func (h *ClusterHandlers) proxyInfrastructure(c *gin.Context, method, upstreamPath string) {
	baseURL, err := defaultInfraBridge.endpoint()
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, err)
		return
	}
	var bodyReader io.Reader
	if method != http.MethodGet && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512*1024)
		rawBody, err := io.ReadAll(c.Request.Body)
		if err != nil {
			JSONError(c, http.StatusBadRequest, errors.New("invalid request body"))
			return
		}
		bodyReader = bytes.NewReader(rawBody)
	}
	targetURL := baseURL + "/api/v1/" + strings.TrimLeft(upstreamPath, "/")
	if q := c.Request.URL.RawQuery; q != "" {
		targetURL += "?" + q
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, targetURL, bodyReader)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if ctxHeader := h.buildBridgeHeader(c.Request.Context()); ctxHeader != "" {
		req.Header.Set("X-Axon-Context", ctxHeader)
	}
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, fmt.Errorf("infrastructure control unavailable: %w", err))
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		JSONError(c, http.StatusBadGateway, err)
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
