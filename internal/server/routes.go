package server

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
	"net/http"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/gql/openapi"
	"github.com/looplj/axonhub/internal/server/maintenance"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/portalgql"
	"github.com/looplj/axonhub/internal/server/static"
)

type Handlers struct {
	fx.In

	Graphql        *gql.GraphqlHandler
	OpenAPIGraphql *openapi.GraphqlHandler
	PortalGraphql  *portalgql.GraphqlHandler
	OpenAI         *api.OpenAIHandlers
	Doubao         *api.DoubaoHandlers
	Anthropic      *api.AnthropicHandlers
	Gemini         *api.GeminiHandlers
	AiSDK          *api.AiSDKHandlers
	Playground     *api.PlaygroundHandlers
	System         *api.SystemHandlers
	Auth           *api.AuthHandlers
	Jina           *api.JinaHandlers
	Codex          *api.CodexHandlers
	ClaudeCode     *api.ClaudeCodeHandlers
	Antigravity    *api.AntigravityHandlers
	Copilot        *api.CopilotHandlers
	RequestContent *api.RequestContentHandlers
	OIDC           *api.OIDCHandlers
	RequestPreview *api.RequestPreviewHandlers
	Management     *api.ManagementHandlers
	Diagnostics    *api.DiagnosticsHandlers
	RawHTTP        *api.RawHTTPHandlers
	Cluster        *api.ClusterHandlers `optional:"true"`
}

type Services struct {
	fx.In

	TraceService  *biz.TraceService
	ThreadService *biz.ThreadService
	AuthService   *biz.AuthService
	SystemService *biz.SystemService
	Node          cluster.Config    `optional:"true"`
	Maintenance   *maintenance.Gate `optional:"true"`
}

func SetupRoutes(server *Server, handlers Handlers, client *ent.Client, services Services, ipAccessControl *middleware.IPAccessControlConfig) {
	// IP 访问控制 - 全局最优先，拦截所有请求包括静态文件
	server.Use(middleware.WithIPAccessControl(ipAccessControl))
	if services.Maintenance != nil {
		server.Use(services.Maintenance.Middleware())
	}
	if services.Node.Enabled {
		server.Use(api.DataNodeRoutes(services.Node), api.WithClusterCallerAuthorization())
	}

	// Serve static frontend files
	server.NoRoute(static.Handler())

	server.Use(middleware.AccessLog())
	server.Use(middleware.WithEntClient(client))
	server.Use(middleware.WithLoggingTracing(server.Config.Trace))
	server.Use(middleware.WithMetrics())

	// Setup CORS middleware at server level if enabled
	if server.Config.CORS.Enabled {
		corsConfig := cors.DefaultConfig()
		corsConfig.AllowOrigins = server.Config.CORS.AllowedOrigins
		corsConfig.AllowMethods = server.Config.CORS.AllowedMethods
		corsConfig.AllowHeaders = server.Config.CORS.AllowedHeaders
		corsConfig.ExposeHeaders = server.Config.CORS.ExposedHeaders
		corsConfig.AllowCredentials = server.Config.CORS.AllowCredentials
		corsConfig.MaxAge = server.Config.CORS.MaxAge

		corsHandler := cors.New(corsConfig)
		server.Use(corsHandler)
		server.OPTIONS("*any", corsHandler)
	}

	if services.Node.Enabled {
		ownerAdmin := server.Group("/__axon/admin", middleware.WithJWTAuth(services.AuthService), middleware.WithLegacyUser(), middleware.WithProjectID(), middleware.WithTimeout(server.Config.LLMRequestTimeout))
		ownerOpenAPI := server.Group("/__axon/openapi", middleware.WithIPBlocklist(services.SystemService), middleware.WithOpenAPIAuth(services.AuthService), middleware.WithTimeout(server.Config.LLMRequestTimeout))
		for _, group := range []*gin.RouterGroup{ownerAdmin, ownerOpenAPI} {
			group.POST("/channel-test", handlers.Cluster.ChannelTest)
			group.POST("/channel-health", handlers.Cluster.ChannelHealth)
			group.POST("/channel-quota-reset", handlers.Cluster.ChannelQuotaReset)
			group.POST("/channel-models", handlers.Cluster.ChannelModels)
			group.POST("/channel-sync", handlers.Cluster.ChannelSync)
		}
		placementNode := server.Group("/__axon/admin/placement", middleware.WithJWTAuth(services.AuthService), middleware.WithLegacyUser(), middleware.WithTimeout(server.Config.RequestTimeout))
		placementNode.POST("/prepare", handlers.Cluster.PrepareChannelPlacement)
		placementNode.POST("/readback", handlers.Cluster.ReadChannelPlacementOnNode)
		ownerAdmin.POST("/playground/chat", middleware.WithSource(request.SourcePlayground), handlers.Playground.ChatCompletion)
		server.GET("/v1/axon/route", middleware.WithIPBlocklist(services.SystemService), middleware.WithAPIKeyConfig(services.AuthService, nil), middleware.WithTimeout(server.Config.RequestTimeout), handlers.Cluster.Resolve)
		server.GET("/v1/axon/task-route", middleware.WithIPBlocklist(services.SystemService), middleware.WithAPIKeyConfig(services.AuthService, nil), middleware.WithTimeout(server.Config.RequestTimeout), handlers.Cluster.TaskRoute)
	}
	requestBodyTransport := middleware.NewRequestBodyTransport()
	server.GET("/__portway/capabilities", middleware.WithIPBlocklist(services.SystemService), requestBodyTransport.Capabilities)

	publicGroup := server.Group("", middleware.WithTimeout(server.Config.RequestTimeout))
	{
		// Favicon API - DO NOT AUTH
		publicGroup.GET("/favicon", handlers.System.GetFavicon)
		// Health check endpoint - no authentication required
		publicGroup.GET("/health", handlers.System.Health)
	}

	unSecureAdminGroup := server.Group("/admin", middleware.WithTimeout(server.Config.RequestTimeout))
	{
		// System Status and Initialize - DO NOT AUTH
		unSecureAdminGroup.GET("/system/status", handlers.System.GetSystemStatus)
		unSecureAdminGroup.POST("/system/initialize", handlers.System.InitializeSystem)
		// User Login - DO NOT AUTH
		unSecureAdminGroup.POST("/auth/signin", handlers.Auth.SignIn)
		unSecureAdminGroup.GET("/auth/registration", handlers.Auth.RegistrationStatus)
		unSecureAdminGroup.POST("/auth/signup", handlers.Auth.SignUp)
	}

	oauthGroup := server.Group("/oauth", middleware.WithTimeout(server.Config.RequestTimeout))
	{
		handlers.OIDC.RegisterRoutes(oauthGroup)
	}

	server.GET("/portal/oidc/link/:provider", middleware.WithJWTAuth(services.AuthService), handlers.OIDC.GetLinkAuthorizeURL)
	server.POST("/portal/graphql", middleware.WithJWTAuth(services.AuthService), middleware.WithTimeout(server.Config.RequestTimeout), func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024)
		c.Header("Cache-Control", "no-store")
		handlers.PortalGraphql.Graphql.ServeHTTP(c.Writer, c.Request)
	})

	adminGroup := server.Group("/admin", middleware.WithJWTAuth(services.AuthService), middleware.WithLegacyUser(), middleware.WithProjectID())
	// 管理员路由 - 使用 JWT 认证
	{
		if services.Node.Enabled {
			adminGroup.GET("/cluster/manifest", middleware.WithTimeout(server.Config.RequestTimeout), handlers.Cluster.Manifest)
			adminGroup.GET("/cluster/model-routes", middleware.WithTimeout(server.Config.RequestTimeout), handlers.Cluster.ModelRoutes)
			adminGroup.POST("/cluster/model-routes/preview", middleware.WithTimeout(server.Config.RequestTimeout), handlers.Cluster.PreviewModelRoutes)
			placementAdmin := server.Group("/admin/cluster/placement", middleware.WithJWTAuth(services.AuthService), middleware.WithLegacyUser(), middleware.WithTimeout(server.Config.RequestTimeout))
			placementAdmin.GET("/:id", handlers.Cluster.ObserveChannelPlacement)
			placementAdmin.GET("/:id/preview", handlers.Cluster.PreviewChannelPlacement)
			placementAdmin.POST("", handlers.Cluster.SaveChannelPlacement)
			placementAdmin.POST("/worker-ack", handlers.Cluster.AcknowledgePlacementWorker)
		}
		if handlers.Cluster != nil && (!services.Node.Enabled || services.Node.IsControl()) {
			infraAdmin := server.Group("/admin/cluster/infrastructure", middleware.WithJWTAuth(services.AuthService), middleware.WithLegacyUser(), middleware.WithTimeout(server.Config.RequestTimeout))
			infraAdmin.GET("/state", handlers.Cluster.InfrastructureState)
			infraAdmin.GET("/topology", handlers.Cluster.InfrastructureTopology)
			infraAdmin.GET("/nodes/:id", handlers.Cluster.InfrastructureGetNode)
			infraAdmin.PUT("/nodes/:id", handlers.Cluster.InfrastructurePutNode)
			infraAdmin.POST("/nodes/:id/actions/:step", handlers.Cluster.InfrastructureNodeAction)
			infraAdmin.PUT("/rules/:id", handlers.Cluster.InfrastructurePutRule)
			infraAdmin.DELETE("/rules/:id", handlers.Cluster.InfrastructureDeleteRule)
			infraAdmin.PUT("/draft", handlers.Cluster.InfrastructurePutDraft)
			infraAdmin.POST("/validate", handlers.Cluster.InfrastructureValidate)
			infraAdmin.POST("/explain", handlers.Cluster.InfrastructureExplain)
			infraAdmin.GET("/releases", handlers.Cluster.InfrastructureReleases)
			infraAdmin.GET("/releases/:id", handlers.Cluster.InfrastructureGetRelease)
			infraAdmin.POST("/releases/preview", handlers.Cluster.InfrastructurePreviewRelease)
			infraAdmin.POST("/releases", handlers.Cluster.InfrastructureCreateRelease)
			infraAdmin.POST("/releases/:id/activate", handlers.Cluster.InfrastructureActivateRelease)
		}
		adminGroup.GET("/playground", middleware.WithTimeout(server.Config.RequestTimeout), func(c *gin.Context) {
			handlers.Graphql.Playground.ServeHTTP(c.Writer, c.Request)
		})
		adminGroup.POST("/graphql", middleware.WithTimeout(server.Config.RequestTimeout), func(c *gin.Context) {
			c.Header("Cache-Control", "no-store")
			handlers.Graphql.Graphql.ServeHTTP(c.Writer, c.Request)
		})

		adminGroup.POST("/codex/oauth/start", handlers.Codex.StartOAuth)
		adminGroup.POST("/codex/oauth/exchange", handlers.Codex.Exchange)
		adminGroup.POST("/codex/auth/decode", handlers.Codex.DecodeAuthJSON)

		adminGroup.POST("/claudecode/oauth/start", handlers.ClaudeCode.StartOAuth)
		adminGroup.POST("/claudecode/oauth/exchange", handlers.ClaudeCode.Exchange)

		adminGroup.POST("/antigravity/oauth/start", handlers.Antigravity.StartOAuth)
		adminGroup.POST("/antigravity/oauth/exchange", handlers.Antigravity.Exchange)

		adminGroup.POST("/copilot/oauth/start", handlers.Copilot.StartOAuth)
		adminGroup.POST("/copilot/oauth/poll", handlers.Copilot.PollOAuth)

		// OIDC Manual Linking
		adminGroup.GET("/oidc/link/:provider", handlers.OIDC.GetLinkAuthorizeURL)

		// Playground API with channel specification support
		adminGroup.POST(
			"/playground/chat",
			middleware.WithTimeout(server.Config.LLMRequestTimeout),
			middleware.WithSource(request.SourcePlayground),
			handlers.Playground.ChatCompletion,
		)

		adminGroup.GET(
			"/requests/:request_id/content",
			middleware.WithTimeout(server.Config.RequestTimeout),
			handlers.RequestContent.DownloadRequestContent,
		)
		adminGroup.GET(
			"/requests/:request_id/preview",
			middleware.WithTimeout(server.Config.RequestTimeout),
			handlers.RequestPreview.PreviewRequest,
		)

		handlers.Management.RegisterAdminRoutes(adminGroup)
	}
	server.POST(
		"/admin/diagnostics/v1/pull",
		middleware.WithJWTAuthResponder(services.AuthService, handlers.Diagnostics.MiddlewareError),
		middleware.WithLegacyUserResponder(handlers.Diagnostics.MiddlewareError),
		middleware.WithProjectIDResponder(handlers.Diagnostics.MiddlewareError),
		middleware.WithTimeout(server.Config.RequestTimeout),
		handlers.Diagnostics.Pull,
	)

	openAPIGroup := server.Group(
		"/openapi",
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithOpenAPIAuth(services.AuthService),
		middleware.WithTimeout(server.Config.RequestTimeout),
	)
	{
		openAPIGroup.POST("/v1/graphql", func(c *gin.Context) {
			handlers.OpenAPIGraphql.Graphql.ServeHTTP(c.Writer, c.Request)
		})
		openAPIGroup.GET("/v1/playground", func(c *gin.Context) {
			handlers.OpenAPIGraphql.Playground.ServeHTTP(c.Writer, c.Request)
		})

		openAPIGroup.POST("/webhook/echo", handlers.System.WebhookEcho)

		handlers.Management.RegisterOpenAPIRoutes(openAPIGroup)
	}
	server.POST(
		"/openapi/v1/diagnostics/pull",
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithOpenAPIAuthResponder(services.AuthService, handlers.Diagnostics.MiddlewareError),
		middleware.WithTimeout(server.Config.RequestTimeout),
		handlers.Diagnostics.Pull,
	)

	rawGroup := server.Group("/raw",
		middleware.WithTimeout(server.Config.LLMRequestTimeout),
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithAPIKeyConfig(services.AuthService, nil),
	)
	rawMethods := []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodHead, http.MethodDelete, http.MethodConnect, http.MethodTrace}
	// CORS owns the global OPTIONS catch-all when enabled. Without CORS, OPTIONS
	// remains an authenticated, opaque upstream request like the other raw methods.
	if !server.Config.CORS.Enabled {
		rawMethods = append(rawMethods, http.MethodOptions)
	}
	rawGroup.Match(rawMethods, "/:channelID/*path", handlers.RawHTTP.Proxy)

	responsesWebSocketGroup := server.Group("/",
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithAPIKeyConfig(services.AuthService, nil),
		middleware.WithSource(request.SourceAPI),
		middleware.WithThread(server.Config.Trace, services.ThreadService),
		middleware.WithTrace(server.Config.Trace, services.TraceService),
	)
	responsesWebSocketGroup.GET("/v1/responses", handlers.OpenAI.ResponsesWebSocketWithTimeout(server.Config.LLMRequestTimeout, services.Maintenance))

	apiGroup := server.Group("/",
		middleware.WithTimeout(server.Config.LLMRequestTimeout),
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithAPIKeyConfig(services.AuthService, nil),
		middleware.WithSource(request.SourceAPI),
		middleware.WithThread(server.Config.Trace, services.ThreadService),
		middleware.WithDecodedRequestBody(requestBodyTransport),
		middleware.WithTrace(server.Config.Trace, services.TraceService),
	)

	{
		openaiGroup := apiGroup.Group("/v1")
		openaiGroup.POST("/chat/completions", handlers.OpenAI.ChatCompletion)
		openaiGroup.POST("/completions", handlers.OpenAI.Completion)
		openaiGroup.POST("/responses/compact", handlers.OpenAI.CompactResponse)
		openaiGroup.POST("/responses", handlers.OpenAI.CreateResponse)
		openaiGroup.GET("/models", handlers.OpenAI.ListModels)
		openaiGroup.GET("/models/*model", handlers.OpenAI.RetrieveModel)
		openaiGroup.POST("/embeddings", handlers.OpenAI.CreateEmbedding)
		openaiGroup.POST("/moderations", handlers.OpenAI.CreateModeration)
		openaiGroup.POST("/systemone", handlers.OpenAI.CreateSystemOne)
		openaiGroup.POST("/decisions", handlers.OpenAI.CreateDecision)
		openaiGroup.POST("/images/generations", handlers.OpenAI.CreateImage)
		openaiGroup.POST("/images/edits", handlers.OpenAI.CreateImageEdit)
		openaiGroup.POST("/videos", handlers.OpenAI.CreateVideo)
		openaiGroup.GET("/videos/:id", handlers.OpenAI.GetVideo)
		openaiGroup.DELETE("/videos/:id", handlers.OpenAI.DeleteVideo)
		openaiGroup.POST("/audio/speech", handlers.OpenAI.CreateSpeech)
		openaiGroup.POST("/audio/transcriptions", handlers.OpenAI.CreateTranscription)
		openaiGroup.POST("/audio/translations", handlers.OpenAI.CreateTranslation)
		// DO NOT SUPPORT IMAGE VARIATION
		// openaiGroup.POST("/images/variations", handlers.OpenAI.CreateImageVariation)

		// OpenAI-compatible Anthropic endpoint
		openaiGroup.POST("/messages", handlers.Anthropic.CreateMessage)

		// Compatible with OpenAI API
		openaiGroup.POST("/rerank", handlers.Jina.Rerank)
	}

	{
		jinaGroup := apiGroup.Group("/jina/v1")
		jinaGroup.POST("/embeddings", handlers.Jina.CreateEmbedding)
		jinaGroup.POST("/rerank", handlers.Jina.Rerank)
	}

	{
		anthropicGroup := apiGroup.Group("/anthropic/v1")
		anthropicGroup.POST("/messages", handlers.Anthropic.CreateMessage)
		anthropicGroup.GET("/models", handlers.Anthropic.ListModels)
	}

	{
		doubaoGroup := apiGroup.Group("/doubao/v3")
		doubaoGroup.POST("/contents/generations/tasks", handlers.Doubao.CreateTask)
		doubaoGroup.GET("/contents/generations/tasks/:id", handlers.Doubao.GetTask)
		doubaoGroup.DELETE("/contents/generations/tasks/:id", handlers.Doubao.DeleteTask)
	}

	{
		registerGeminiRoutes := func(group *gin.RouterGroup) {
			group.POST("/models/*action", handlers.Gemini.GenerateContent)
			group.GET("/models", handlers.Gemini.ListModels)
		}

		geminiGroup := server.Group("/gemini/:gemini-api-version",
			middleware.WithTimeout(server.Config.LLMRequestTimeout),
			middleware.WithIPBlocklist(services.SystemService),
			middleware.WithGeminiKeyAuth(services.AuthService),
			middleware.WithSource(request.SourceAPI),
			middleware.WithThread(server.Config.Trace, services.ThreadService),
			middleware.WithDecodedRequestBody(requestBodyTransport),
			middleware.WithTrace(server.Config.Trace, services.TraceService),
		)

		registerGeminiRoutes(geminiGroup)

		// Alias for Gemini API
		geminiAliasGroup := server.Group("/v1beta",
			middleware.WithTimeout(server.Config.LLMRequestTimeout),
			middleware.WithIPBlocklist(services.SystemService),
			middleware.WithGeminiKeyAuth(services.AuthService),
			middleware.WithSource(request.SourceAPI),
			middleware.WithThread(server.Config.Trace, services.ThreadService),
			middleware.WithDecodedRequestBody(requestBodyTransport),
			middleware.WithTrace(server.Config.Trace, services.TraceService),
		)

		registerGeminiRoutes(geminiAliasGroup)
	}
}
