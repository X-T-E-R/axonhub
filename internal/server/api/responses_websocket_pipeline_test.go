package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type wsPipelineSelector struct{ channel atomic.Pointer[biz.Channel] }

func (s *wsPipelineSelector) Select(context.Context, *llm.Request) ([]*orchestrator.ChannelModelsCandidate, error) {
	return []*orchestrator.ChannelModelsCandidate{{Channel: s.channel.Load(), Models: []biz.ChannelModelEntry{{RequestModel: "m", ActualModel: "upstream-m"}}}}, nil
}

type wsPromptProvider struct{ prompts []*ent.Prompt }

func (p wsPromptProvider) GetEnabledPrompts(context.Context, int) ([]*ent.Prompt, error) {
	return p.prompts, nil
}

func TestResponsesWebSocketActualPipelineNativeAndHTTPControl(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ws-pipeline-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	proj := client.Project.Create().SetName("ws-project").SetStatus(project.StatusActive).SaveX(ctx)
	owner := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("test").SetStatus(user.StatusActivated).SaveX(ctx)
	key := client.APIKey.Create().SetName("test").SetKey("ah-ws-test").SetType(apikey.TypeUser).SetStatus(apikey.StatusEnabled).SetProjectID(proj.ID).SetUserID(owner.ID).SaveX(ctx)
	client.DataStorage.Create().SetName("primary").SetDescription("test primary").SetType(datastorage.TypeDatabase).SetPrimary(true).SetStatus(datastorage.StatusActive).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: client})
	require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 2, MaxSingleChannelRetries: 2}))
	require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true}))
	projectService := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cache, Ent: client})
	keyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cache, Ent: client, ProjectService: projectService, KeyPrefix: "ah"})
	defer keyService.Stop()
	auth := &biz.AuthService{APIKeyService: keyService}
	channelService := biz.NewChannelServiceForTest(client)
	usage := biz.NewUsageLogService(client, system, channelService)
	storage := &biz.DataStorageService{AbstractService: &biz.AbstractService{}, SystemService: system, Cache: xcache.NewFromConfig[ent.DataStorage](cache)}
	requests := biz.NewRequestService(client, system, usage, storage, biz.NewLiveStreamRegistry())
	var calls atomic.Int32
	received := make(chan []byte, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		received <- body
		require.Equal(t, "Bearer upstream-key", r.Header.Get("Authorization"))
		if strings.Contains(string(body), "force_failure") {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"server_error","message":"offline failure"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &fields))
		if _, chat := fields["messages"]; chat {
			fmt.Fprint(w, "data: {\"id\":\"chat1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"HTTP answer\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"object\":\"response\",\"model\":\"upstream-m\",\"status\":\"completed\",\"created_at\":1,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg\",\"phase\":\"commentary\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\",\"future_signature\":\"opaque\"}],\"future\":9007199254740993}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n", n)
	}))
	defer upstream.Close()
	row := client.Channel.Create().SetType(channel.TypeOpenai).SetName("native").SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "upstream-key"}).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SetSettings(&objects.ChannelSettings{PassThroughBody: lo.ToPtr(false)}).SaveX(ctx)
	native, err := responses.NewOutboundTransformer(upstream.URL, "upstream-key")
	require.NoError(t, err)
	selected := &biz.Channel{Channel: row, Outbound: native}
	selector := &wsPipelineSelector{}
	selector.channel.Store(selected)
	processor := orchestrator.NewChatCompletionOrchestrator(channelService, nil, requests, httpclient.NewHttpClient(), responses.NewInboundTransformer(), system, usage, nil, nil, nil, nil, orchestrator.NewChannelLimiterManager(), nil).WithChannelSelector(selector)
	processor.PromptProtecter = nil
	processor.PromptProvider = wsPromptProvider{prompts: []*ent.Prompt{
		{Role: "system", Content: "injected-prefix", Order: 1, Settings: objects.PromptSettings{Action: objects.PromptAction{Type: objects.PromptActionTypePrepend}}},
		{Role: "user", Content: "injected-suffix", Order: 2, Settings: objects.PromptSettings{Action: objects.PromptAction{Type: objects.PromptActionTypeAppend}}},
	}}
	h := newResponsesWebSocketHandler(&ChatCompletionHandlers{ChatCompletionOrchestrator: processor}, auth)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(ent.NewContext(c.Request.Context(), client))
	})
	router.Use(middleware.WithAPIKeyConfig(auth, nil))
	router.GET("/v1/responses", h.Serve)
	router.POST("/v1/responses", (&ChatCompletionHandlers{ChatCompletionOrchestrator: processor}).ChatCompletion)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, handshake, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer " + key.Key}})
	if err != nil && handshake != nil {
		body, _ := io.ReadAll(handshake.Body)
		t.Fatalf("WebSocket handshake %d: %s: %v", handshake.StatusCode, body, err)
	}
	require.NoError(t, err)
	defer conn.Close()
	send := func(body string) { require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(body))) }
	send(`{"type":"response.create","model":"m","instructions":"client-instructions","store":false,"input":[{"role":"user","content":"full","future":{"n":9007199254740993}}]}`)
	event := wsRead(t, conn)
	require.Equal(t, "response.completed", wsEventString(event, "type"))
	require.Contains(t, string(event["response"]), "9007199254740993")
	var full map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(<-received, &full))
	require.Equal(t, `"upstream-m"`, string(full["model"]))
	require.NotContains(t, full, "instructions")
	var input []json.RawMessage
	require.NoError(t, json.Unmarshal(full["input"], &input))
	require.Len(t, input, 4)
	require.Contains(t, string(input[0]), "injected-prefix")
	require.Contains(t, string(input[1]), "client-instructions")
	require.Contains(t, string(input[2]), "9007199254740993")
	require.Contains(t, string(input[3]), "injected-suffix")
	send(`{"type":"response.create","model":"m","previous_response_id":"resp_1","instructions":"current-instructions","input":[{"role":"user","content":"delta"}]}`)
	event = wsRead(t, conn)
	require.Equal(t, "response.completed", wsEventString(event, "type"))
	var delta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(<-received, &delta))
	require.NotContains(t, delta, "previous_response_id")
	require.NoError(t, json.Unmarshal(delta["input"], &input))
	require.Len(t, input, 6)
	require.Contains(t, string(input[1]), "current-instructions")
	require.Contains(t, string(input[3]), "future_signature")
	require.Contains(t, string(input[3]), "9007199254740993")
	require.Contains(t, string(input[4]), "delta")
	// A rejected control frame is a FIFO barrier after terminal persistence.
	send(`{"type":"response.create","model":"m","generate":false}`)
	require.Equal(t, "unsupported_parameter", wsErrorCode(wsRead(t, conn)))
	// The real pipeline persists requests and usage after its drain is joined.
	require.Equal(t, 2, client.Request.Query().Where(request.ProjectIDEQ(proj.ID)).CountX(ctx))
	require.Equal(t, 2, client.UsageLog.Query().CountX(ctx))
	// Changing the active key profile is observed by the next frame on this socket.
	_, err = keyService.UpdateAPIKeyProfiles(ctx, key.ID, objects.APIKeyProfiles{ActiveProfile: "deny", Profiles: []objects.APIKeyProfile{{Name: "deny", ModelIDs: []string{"other"}}}})
	require.NoError(t, err)
	send(`{"type":"response.create","model":"m","input":"blocked"}`)
	rejected := wsRead(t, conn)
	require.Equal(t, "error", wsEventString(rejected, "type"))
	require.Equal(t, "not_executed", wsEventString(rejected, "execution"))
	require.Equal(t, int32(2), calls.Load())
	_, err = keyService.UpdateAPIKeyProfiles(ctx, key.ID, objects.APIKeyProfiles{ActiveProfile: "allow", Profiles: []objects.APIKeyProfile{{Name: "allow"}}})
	require.NoError(t, err)
	// An attempted provider failure is ambiguous and must not consume the
	// configured same-channel or cross-channel retry budgets.
	send(`{"type":"response.create","model":"m","input":"force_failure"}`)
	ambiguous := wsRead(t, conn)
	require.Equal(t, "error", wsEventString(ambiguous, "type"))
	require.NotContains(t, ambiguous, "execution")
	require.Equal(t, int32(3), calls.Load())
	<-received
	// New WS safety policy does not turn cross-format HTTP into an unsupported path.
	chat, err := openai.NewOutboundTransformer(upstream.URL, "upstream-key")
	require.NoError(t, err)
	selector.channel.Store(&biz.Channel{Channel: row, Outbound: chat})
	send(`{"type":"response.create","model":"m","input":"cross-format"}`)
	rejected = wsRead(t, conn)
	require.Equal(t, "unsupported_upstream_format", wsErrorCode(rejected))
	require.Equal(t, "not_executed", wsEventString(rejected, "execution"))
	require.Equal(t, int32(3), calls.Load())
	httpReq, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"ordinary HTTP"}`))
	require.NoError(t, err)
	httpReq.Header.Set("Authorization", "Bearer "+key.Key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	httpBody, err := io.ReadAll(httpResp.Body)
	require.NoError(t, err)
	require.NoError(t, httpResp.Body.Close())
	require.Equal(t, http.StatusOK, httpResp.StatusCode)
	require.Contains(t, string(httpBody), "HTTP answer")
	require.Equal(t, int32(4), calls.Load())
	<-received
	// Revoke after upgrade; the next frame must reauthenticate and avoid inference.
	_, err = keyService.UpdateAPIKeyStatus(ctx, key.ID, apikey.StatusDisabled)
	require.NoError(t, err)
	send(`{"type":"response.create","model":"m","input":"revoked"}`)
	require.Equal(t, "authentication_failed", wsErrorCode(wsRead(t, conn)))
	require.Equal(t, int32(4), calls.Load())
}
