package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestResponsesWebSocketExecutionMarkerIsGatewayOwned(t *testing.T) {
	var calls atomic.Int32
	bodies := make(chan []byte, 2)
	h := wsTestHandler(func(_ context.Context, request *httpclient.Request, observer orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
		bodies <- request.Body
		terminal := `{"type":"response.completed","execution":"not_executed","response":{"id":"r","status":"completed","execution":"native-response","output":[{"type":"future_output","execution":"native-output","n":9007199254740993}]}}`
		if calls.Add(1) == 2 {
			terminal = `{"type":"error","execution":"not_executed","error":{"code":"upstream_failure","execution":"native-detail","future":{"n":9007199254740993}}}`
		}
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{
			{Data: []byte(`{"type":"response.created","execution":"not_executed","response":{"id":"r","execution":"native-created"}}`)},
			{Data: []byte(`{"type":"response.output_text.delta","execution":"not_executed","delta":"partial"}`)},
			{Data: []byte(terminal)},
		})}, nil
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	for _, frame := range []string{
		`{"type":"response.create","stream_id":"lane","model":"m","input":"full"}`,
		`{"type":"response.create","stream_id":"lane","model":"m","previous_response_id":"r","input":"delta"}`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
		for _, expected := range []string{"response.created", "response.output_text.delta", "terminal"} {
			event := wsRead(t, conn)
			require.NotContains(t, event, "execution")
			require.Equal(t, "lane", wsEventString(event, "stream_id"))
			if expected != "terminal" {
				require.Equal(t, expected, wsEventString(event, "type"))
			} else if wsEventString(event, "type") == "response.completed" {
				require.Contains(t, string(event["response"]), `"execution":"native-response"`)
				require.Contains(t, string(event["response"]), `"execution":"native-output"`)
			} else {
				require.Equal(t, "error", wsEventString(event, "type"))
				require.Contains(t, string(event["error"]), `"execution":"native-detail"`)
				require.Contains(t, string(event["error"]), `9007199254740993`)
			}
		}
	}
	<-bodies
	expanded := <-bodies
	require.Contains(t, string(expanded), `"execution":"native-output"`)
	require.Contains(t, string(expanded), `9007199254740993`)
	for _, frame := range []string{
		`{"type":"response.create","model":"m","previous_response_id":"missing","input":"delta"}`,
		`{"type":"response.create","model":"m","generate":false}`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
		require.Equal(t, "not_executed", wsEventString(wsRead(t, conn), "execution"))
	}
	require.Equal(t, int32(2), calls.Load())
}

func TestResponsesWebSocketCurrentGlobalIPPolicy(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ws-ip-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client})
	setBlocked := func(blocked []string) {
		require.NoError(t, system.SetSecuritySettings(ctx, biz.SecuritySettings{BlockedIPs: blocked, CyberSessionBlockTTLSeconds: 600}))
	}
	setBlocked(nil)
	var calls atomic.Int32
	h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		calls.Add(1)
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion("r", `[]`)})}, nil
	})
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Request = c.Request.WithContext(ent.NewContext(c.Request.Context(), client)) })
	router.Use(middleware.WithIPBlocklist(system))
	router.GET("/v1/responses", h.Serve)
	router.GET("/control", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	server := httptest.NewServer(router)
	defer server.Close()
	conn := wsTestDial(t, server)
	send := func() {
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "offline"}))
	}
	control := func(expected int) {
		response, err := http.Get(server.URL + "/control")
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, expected, response.StatusCode)
	}
	send()
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	setBlocked([]string{"127.0.0.1", "::1"})
	control(http.StatusForbidden)
	send()
	rejected := wsRead(t, conn)
	require.Equal(t, "ip_blocked", wsErrorCode(rejected))
	require.Equal(t, "403", string(rejected["status"]))
	require.Equal(t, "not_executed", wsEventString(rejected, "execution"))
	require.Equal(t, int32(1), calls.Load())
	blockedConn, handshake, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if blockedConn != nil {
		blockedConn.Close()
	}
	require.Error(t, err)
	require.NotNil(t, handshake)
	require.NoError(t, handshake.Body.Close())
	require.Equal(t, http.StatusForbidden, handshake.StatusCode)
	setBlocked(nil)
	control(http.StatusNoContent)
	send()
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	require.Equal(t, int32(2), calls.Load())
}

func TestResponsesWebSocketActualPipelineProviderErrorBoundary(t *testing.T) {
	const sentinel = "synthetic-ws-error-payload"
	client := enttest.NewEntClient(t, "sqlite3", "file:ws-error-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	proj := client.Project.Create().SetName("ws-project").SetStatus(project.StatusActive).SaveX(ctx)
	owner := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("test").SetStatus(user.StatusActivated).SaveX(ctx)
	key := client.APIKey.Create().SetName("test").SetKey("ah-ws-offline-error").SetType(apikey.TypeUser).SetStatus(apikey.StatusEnabled).SetProjectID(proj.ID).SetUserID(owner.ID).SaveX(ctx)
	client.DataStorage.Create().SetName("primary").SetDescription("offline").SetType(datastorage.TypeDatabase).SetPrimary(true).SetStatus(datastorage.StatusActive).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: client})
	projects := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cache, Ent: client})
	keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cache, Ent: client, ProjectService: projects, KeyPrefix: "ah"})
	defer keys.Stop()
	auth := &biz.AuthService{APIKeyService: keys}
	channels := biz.NewChannelServiceForTest(client)
	usage := biz.NewUsageLogService(client, system, channels)
	storage := &biz.DataStorageService{AbstractService: &biz.AbstractService{}, SystemService: system, Cache: xcache.NewFromConfig[ent.DataStorage](cache)}
	requests := biz.NewRequestService(client, system, usage, storage, biz.NewLiveStreamRegistry())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"model\":\"m\",\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"error\",\"status\":503,\"code\":%q,\"message\":%q,\"error\":{\"type\":\"server_error\",\"message\":%q},\"execution\":\"not_executed\"}\n\n", sentinel, sentinel, sentinel)
	}))
	defer upstream.Close()
	row := client.Channel.Create().SetType(channel.TypeOpenai).SetName("offline-native").SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "offline-unused"}).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SaveX(ctx)
	native, err := responses.NewOutboundTransformer(upstream.URL, "offline-unused")
	require.NoError(t, err)
	selector := &wsPipelineSelector{}
	selector.channel.Store(&biz.Channel{Channel: row, Outbound: native})
	processor := orchestrator.NewChatCompletionOrchestrator(channels, nil, requests, httpclient.NewHttpClient(), responses.NewInboundTransformer(), system, usage, nil, nil, nil, nil, orchestrator.NewChannelLimiterManager(), nil).WithChannelSelector(selector)
	processor.PromptProtecter = nil
	processor.PromptProvider = wsPromptProvider{}
	h := newResponsesWebSocketHandler(&ChatCompletionHandlers{ChatCompletionOrchestrator: processor}, auth)
	var logMu sync.Mutex
	var diagnostic strings.Builder
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)
	log.GetGlobalLogger().AddHook(log.HookFunc(func(_ context.Context, message string, fields ...log.Field) []log.Field {
		if active.Load() {
			encoder := zapcore.NewMapObjectEncoder()
			for _, field := range fields {
				field.AddTo(encoder)
			}
			encoded, _ := json.Marshal(encoder.Fields)
			logMu.Lock()
			fmt.Fprintf(&diagnostic, "%s %s\n", message, encoded)
			logMu.Unlock()
		}
		return fields
	}))
	previousSlog := slog.Default()
	slog.SetDefault(log.GetGlobalLogger().AsSlog())
	defer slog.SetDefault(previousSlog)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(ent.NewContext(log.ForceEnableLevel(c.Request.Context(), log.DebugLevel), client))
	})
	router.Use(middleware.WithAPIKeyConfig(auth, nil))
	router.GET("/v1/responses", h.Serve)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer " + key.Key}})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": sentinel}))
	var types []string
	for {
		event := wsRead(t, conn)
		types = append(types, wsEventString(event, "type"))
		if wsEventString(event, "type") == "error" {
			require.NotContains(t, event, "execution")
			break
		}
	}
	require.Equal(t, []string{"response.created", "response.output_text.delta", "error"}, types)
	// The FIFO barrier joins the prior canonical persistence/error reporting.
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "generate": false}))
	for {
		event := wsRead(t, conn)
		if wsErrorCode(event) == "unsupported_parameter" {
			require.Equal(t, "not_executed", wsEventString(event, "execution"))
			break
		}
	}
	require.Equal(t, int32(1), calls.Load())
	logMu.Lock()
	captured := diagnostic.String()
	logMu.Unlock()
	require.Contains(t, captured, "request process failed")
	require.Contains(t, captured, "upstream_response_error")
	require.NotContains(t, captured, sentinel)
}
