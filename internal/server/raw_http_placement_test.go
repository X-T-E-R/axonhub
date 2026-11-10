package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

func TestRawHTTPClusterOwnerPlacementAndDataNode(t *testing.T) {
	f := newNativeDecisionFixture(t)
	var calls atomic.Int32
	received := make(chan rawCapture, 8)
	canceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		received <- rawCapture{body: body, header: r.Header.Clone(), uri: r.RequestURI, host: r.Host}
		if r.URL.Path == "/private/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: opaque\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Provider-Opaque", "preserved")
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	id := createRawChannel(t, f, upstream.URL+"/private", "opaque-media", "bearer", "")
	makeNode := func(name string) (*biz.ChannelService, *httptest.Server) {
		node := cluster.Config{Enabled: true, NodeID: name, ControlNode: "control", Nodes: map[string]string{"control": "http://127.0.0.1:1", "same-host": "http://127.0.0.1:2"},
			Channels: map[int]cluster.ChannelBinding{id: {Owner: "control", Identity: "opaque-account"}}}
		hc := httpclient.NewHttpClient()
		t.Cleanup(hc.CloseIdleConnections)
		svc := biz.NewChannelService(biz.ChannelServiceParams{Ent: f.db, SystemService: f.system, HttpClient: hc, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Node: node})
		t.Cleanup(svc.Stop)
		// Use the production data-node gate, Key auth, and raw handler. There is no
		// synthetic owner dispatch or bypass of request authorization here.
		router := gin.New()
		router.Use(api.DataNodeRoutes(node), middleware.WithEntClient(f.db))
		auth := &biz.AuthService{APIKeyService: f.keyService, SystemService: f.system}
		raw := router.Group("/raw", middleware.WithAPIKeyConfig(auth, nil))
		raw.Match([]string{"POST", "GET", "DELETE"}, "/:channelID/*path", api.NewRawHTTPHandlers(api.RawHTTPHandlersParams{ChannelService: svc, Ent: f.db}).Proxy)
		router.GET("/admin/forbidden", func(c *gin.Context) { c.Status(200) })
		server := httptest.NewServer(router)
		t.Cleanup(server.Close)
		return svc, server
	}
	control, source := makeNode("control")
	target, owner := makeNode("same-host")
	models := biz.NewModelService(biz.ModelServiceParams{Ent: f.db, SystemService: f.system, ChannelService: control})
	manifest, err := models.BuildExecutionManifest(f.ctx)
	require.NoError(t, err)
	require.Equal(t, map[int]string{id: "control"}, manifest.RawChannels)

	payload := []byte{0, 255, 128, 13, 10}
	path := fmt.Sprintf("/raw/%d/v1/images/edits/a%%2Fb?x=%%FF&x=+&flag", id)
	do := func(server *httptest.Server, key string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest("POST", server.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=opaque")
		req.Header.Set("Content-Encoding", "dcz")
		req.Header.Set("X-LLM-Model", "not-routing-authority")
		res, err := server.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, res.Header, body
	}
	status, _, body := do(owner, f.key.Key)
	require.Equal(t, 409, status, string(body))
	require.Contains(t, string(body), `"execution":"not_executed"`)
	require.Zero(t, calls.Load())
	status, _, body = do(source, f.key.Key)
	require.Equal(t, 429, status, string(body))
	require.Equal(t, payload, body)
	<-received

	// Commit a new primary placement snapshot, intentionally leaving both
	// services' in-memory provider caches stale. Raw execution must fence on it.
	state := biz.ChannelPlacementState{PlacementSnapshot: cluster.PlacementSnapshot{Version: 1, Owners: map[string]string{"opaque-account": "same-host"}}}
	rawState, err := json.Marshal(state)
	require.NoError(t, err)
	f.db.System.Create().SetKey("channel_execution_placement_v1").SetValue(string(rawState)).SaveX(f.ctx)
	require.Equal(t, "control", control.NodeConfig().Owner(id))
	require.Equal(t, "control", target.NodeConfig().Owner(id))
	manifest, err = models.BuildExecutionManifest(f.ctx)
	require.NoError(t, err)
	require.Equal(t, map[int]string{id: "same-host"}, manifest.RawChannels)
	require.EqualValues(t, 1, manifest.PlacementVersion)
	wire, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.Contains(t, string(wire), fmt.Sprintf(`"raw_channels":{"%d":"same-host"}`, id))
	require.NotContains(t, string(wire), "synthetic-upstream")

	status, _, body = do(source, f.key.Key)
	require.Equal(t, 409, status, string(body))
	require.EqualValues(t, 1, calls.Load(), "former owner must not contact upstream or replay")
	status, _, _ = do(owner, "wrong-key")
	require.Equal(t, 401, status)
	status, _, _ = do(owner, f.blockedKey.Key)
	require.Equal(t, 403, status)
	status, headers, body := do(owner, f.key.Key)
	require.Equal(t, 429, status)
	require.Equal(t, payload, body)
	require.Equal(t, "preserved", headers.Get("X-Provider-Opaque"))
	require.Equal(t, "9", headers.Get("Retry-After"))
	capture := <-received
	require.Equal(t, "/private/v1/images/edits/a%2Fb?x=%FF&x=+&flag", capture.uri)
	require.Equal(t, payload, capture.body)
	require.Equal(t, "dcz", capture.header.Get("Content-Encoding"))
	require.Equal(t, "Bearer synthetic-upstream-opaque-media", capture.header.Get("Authorization"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/raw/%d/stream", owner.URL, id), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.key.Key)
	res, err := owner.Client().Do(req)
	require.NoError(t, err)
	chunk := make([]byte, len("data: opaque\n\n"))
	_, err = io.ReadFull(res.Body, chunk)
	require.NoError(t, err)
	require.Equal(t, "data: opaque\n\n", string(chunk))
	cancel()
	res.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("data-node cancellation did not reach the same-host upstream")
	}
	<-received
	resp, err := owner.Client().Get(owner.URL + "/admin/forbidden")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 404, resp.StatusCode, "raw admission must not expose data-node management")

	f.db.Channel.UpdateOneID(id).SetStatus(channel.StatusDisabled).SaveX(f.ctx)
	manifest, err = models.BuildExecutionManifest(f.ctx)
	require.NoError(t, err)
	require.Empty(t, manifest.RawChannels, "disabled raw routes disappear without a channel-cache reload")
	status, _, _ = do(owner, f.key.Key)
	require.Equal(t, 403, status)
	require.EqualValues(t, 3, calls.Load())
	require.Zero(t, f.db.UsageLog.Query().CountX(f.ctx))
}
