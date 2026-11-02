package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entmodel "github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/portalgql"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

// F-03: the alias deliberately has no association. Both directories must follow
// its mapped target, just like the real authenticated inference pipeline.
func TestPortalCatalogKeyMappingHTTPAndRevocation(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:portal-catalog-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	u := db.User.Create().SetEmail("catalog@example.test").SetPassword("synthetic").SetAccessMode(user.AccessModePortal).SaveX(ctx)
	var callsA, callsB atomic.Int32
	makeRoute := func(name, requestModel string, calls *atomic.Int32) (*ent.Channel, *biz.Channel) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "provider-"+name, body["model"])
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"resp_%s","object":"response","model":"provider-%s","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, name, name)
		}))
		t.Cleanup(upstream.Close)
		ch := db.Channel.Create().SetName(name).SetType(channel.TypeOpenaiResponses).SetBaseURL(upstream.URL).
			SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).SetSupportedModels([]string{"provider-" + name}).SetDefaultTestModel(requestModel).
			SetSettings(&objects.ChannelSettings{ModelMappings: []objects.ModelMapping{{From: requestModel, To: "provider-" + name}}}).SaveX(ctx)
		outbound, err := responses.NewOutboundTransformer(upstream.URL, "synthetic")
		require.NoError(t, err)
		return ch, &biz.Channel{Channel: ch, Outbound: outbound}
	}
	a, routeA := makeRoute("a", "target", &callsA)
	b, routeB := makeRoute("b", "alternate", &callsB)
	profile := func(ids []string, channelIDs []int) *objects.ProjectProfiles {
		return &objects.ProjectProfiles{ActiveProfile: "active", Profiles: []objects.ProjectProfile{{Name: "active", ModelIDs: &ids, ChannelIDs: channelIDs}}}
	}
	allowedModels, allowedChannels := []string{"alias", "target", "alternate"}, []int{a.ID, b.ID}
	g := db.Project.Create().SetName("Catalog group").SetProfiles(profile(allowedModels, allowedChannels)).SaveX(ctx)
	db.UserProject.Create().SetUserID(u.ID).SetProjectID(g.ID).SaveX(ctx)
	k := db.APIKey.Create().SetName("Catalog key").SetKey("ah-catalog-synthetic").SetType(apikey.TypePersonal).SetPortalManaged(true).SetUserID(u.ID).SetProjectID(g.ID).
		SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "p", Profiles: []objects.APIKeyProfile{{Name: "p", ModelIDs: []string{"alias"}, ModelMappings: []objects.ModelMapping{{From: "alias", To: "target"}}}}}).SaveX(ctx)
	createModel := func(id string, associations []*objects.ModelAssociation) *ent.Model {
		return db.Model.Create().SetModelID(id).SetName(id).SetType(entmodel.TypeChat).SetStatus(entmodel.StatusEnabled).SetDeveloper("synthetic").SetIcon("OpenAI").SetGroup("catalog").
			SetModelCard(&objects.ModelCard{Limit: objects.ModelCardLimit{Context: 1024}}).SetSettings(&objects.ModelSettings{Associations: associations}).SaveX(ctx)
	}
	alias := createModel("alias", nil)
	db.Model.UpdateOne(alias).SetName("Public alias").SetType(entmodel.TypeEmbedding).SetModelCard(&objects.ModelCard{Vision: true, Limit: objects.ModelCardLimit{Context: 999}}).SaveX(ctx)
	createModel("target", []*objects.ModelAssociation{
		{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: a.ID, ModelID: "target"}},
		{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: b.ID, ModelID: "alternate"}},
	})
	channels := biz.NewChannelServiceForTest(db)
	defer channels.Stop()
	channels.SetEnabledChannelsForTest([]*biz.Channel{routeA, routeB})
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: db, CacheConfig: cache})
	db.System.Create().SetKey(biz.SystemKeySecretKey).SetValue("synthetic-catalog-jwt").SaveX(ctx)
	users := biz.NewUserService(biz.UserServiceParams{Ent: db, CacheConfig: cache})
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: db, SystemService: system, UserService: users})
	models := biz.NewModelService(biz.ModelServiceParams{Ent: db, SystemService: system, ChannelService: channels})
	portal := biz.NewPortalService(biz.PortalServiceParams{Ent: db, Models: models})
	db.DataStorage.Create().SetName("primary").SetDescription("synthetic").SetType(datastorage.TypeDatabase).SetPrimary(true).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)
	quota := biz.NewQuotaService(db, system)
	usage := biz.NewUsageLogService(db, system, channels)
	storage := &biz.DataStorageService{AbstractService: &biz.AbstractService{}, SystemService: system, Cache: xcache.NewFromConfig[ent.DataStorage](cache)}
	requests := biz.NewRequestService(db, system, usage, storage, biz.NewLiveStreamRegistry())
	selector := orchestrator.NewDefaultSelector(channels, models, system)
	processor := orchestrator.NewChatCompletionOrchestrator(channels, selector, requests, httpclient.NewHttpClient(), responses.NewInboundTransformer(), system, usage, nil, quota, nil, nil, orchestrator.NewChannelLimiterManager(), nil)
	processor.PromptProtecter = nil
	processor.PromptProvider = wsPromptProvider{}
	handler := &ChatCompletionHandlers{ChatCompletionOrchestrator: processor}
	pgql := portalgql.NewGraphqlHandlers(portal, users)
	openai := &OpenAIHandlers{ModelService: models, SystemService: system, EntClient: db}
	router := gin.New()
	router.Use(middleware.WithEntClient(db))
	router.POST("/v1/responses", middleware.WithAPIKeyConfig(auth, nil), handler.ChatCompletion)
	router.GET("/v1/models", middleware.WithAPIKeyConfig(auth, nil), openai.ListModels)
	router.POST("/portal/graphql", middleware.WithJWTAuth(auth), func(c *gin.Context) { pgql.Graphql.ServeHTTP(c.Writer, c.Request) })
	jwt, err := auth.GenerateJWTToken(ctx, u)
	require.NoError(t, err)
	serve := func(method, path, credential, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}
	checkedTemplates := false
	checkMetadata := func(m *objects.PortalModel) {
		t.Helper()
		require.Equal(t, "alias", m.ID)
		require.Equal(t, "Public alias", m.Name)
		require.Equal(t, "chat", m.Type)
		require.False(t, m.Vision)
		require.NotNil(t, m.ContextLength)
		require.Equal(t, 1024, *m.ContextLength)
		require.Equal(t, []string{"openai/responses"}, m.APIFormats)
	}
	checkDirectories := func(names ...string) {
		t.Helper()
		rec := serve("GET", "/v1/models", k.Key, "")
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var compatibility struct{ Data []OpenAIModel }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &compatibility))
		query := fmt.Sprintf(`{"query":"{models(projectID:%d,keyID:%d,type:\"chat\"){id name type vision contextLength apiFormats} modelChannels(projectID:%d,keyID:%d,type:\"chat\"){id name models{id name type vision contextLength apiFormats}}}"}`, g.ID, k.ID, g.ID, k.ID)
		rec = serve("POST", "/portal/graphql", jwt, query)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var body struct {
			Data struct {
				Models        []*objects.PortalModel
				ModelChannels []*objects.PortalModelChannel
			}
			Errors json.RawMessage
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Empty(t, body.Errors, rec.Body.String())
		var actualNames []string
		for _, ch := range body.Data.ModelChannels {
			actualNames = append(actualNames, ch.Name)
			require.True(t, strings.HasPrefix(ch.ID, "channel-"))
			require.Len(t, ch.Models, 1)
			checkMetadata(ch.Models[0])
		}
		require.ElementsMatch(t, names, actualNames, "one public model may appear under each authorized channel")
		if len(names) > 0 {
			require.Len(t, compatibility.Data, 1)
			require.Equal(t, "alias", compatibility.Data[0].ID)
			require.Len(t, body.Data.Models, 1)
			checkMetadata(body.Data.Models[0])
			if node := os.Getenv("AXON_PORTAL_NODE"); node != "" && !checkedTemplates {
				payload, err := json.Marshal(map[string]any{"model": body.Data.Models[0], "channels": body.Data.ModelChannels})
				require.NoError(t, err)
				command := exec.Command(node, "../../../integration_test/portal/mapped_model_client.mjs")
				command.Stdin = bytes.NewReader(payload)
				output, err := command.CombinedOutput()
				require.NoError(t, err, string(output))
				t.Log(string(output))
				checkedTemplates = true
			}
		} else {
			require.Empty(t, compatibility.Data)
			require.Empty(t, body.Data.Models)
		}
	}
	invoke := func(allowed bool) {
		t.Helper()
		before := callsA.Load() + callsB.Load()
		rec := serve("POST", "/v1/responses", k.Key, `{"model":"alias","input":"synthetic"}`)
		if allowed {
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"id":"resp_`)
			require.Contains(t, rec.Body.String(), `"model":"alias"`)
			require.Equal(t, before+1, callsA.Load()+callsB.Load())
		} else {
			require.NotEqual(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, before, callsA.Load()+callsB.Load(), "revoked routes must never reach upstream")
		}
	}
	// Warm the selector cache with both candidates, then change only the group.
	require.Contains(t, routeA.GetModelEntries(), "target")
	require.Contains(t, routeB.GetModelEntries(), "alternate")
	key, err := biz.RefreshAPIKeyAuthorization(ctx, db, k.Key)
	require.NoError(t, err)
	candidates, err := selector.Select(contexts.WithAPIKey(ctx, key), &llm.Request{Model: "target", RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIResponse})
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	checkDirectories("a", "b")
	invoke(true)
	// Per-entry upper boundary removes alternate but leaves target callable.
	db.Project.UpdateOne(g).SetProfiles(profile([]string{"alias", "target"}, allowedChannels)).SaveX(ctx)
	beforeB := callsB.Load()
	checkDirectories("a")
	invoke(true)
	require.Equal(t, beforeB, callsB.Load())
	// Removing the target itself rejects even though alternate is still allowed.
	db.Project.UpdateOne(g).SetProfiles(profile([]string{"alias", "alternate"}, allowedChannels)).SaveX(ctx)
	checkDirectories()
	invoke(false)
	// With only B allowed, inference and directories remain usable; removing B
	// too leaves a valid non-empty group profile but no eligible target route.
	db.Project.UpdateOne(g).SetProfiles(profile(allowedModels, []int{b.ID})).SaveX(ctx)
	beforeA := callsA.Load()
	checkDirectories("b")
	invoke(true)
	require.Equal(t, beforeA, callsA.Load())
	db.Project.UpdateOne(g).SetProfiles(profile([]string{"alias", "target"}, []int{b.ID})).SaveX(ctx)
	checkDirectories()
	invoke(false)
}
