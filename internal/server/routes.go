package server

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/gql/openapi"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/static"
)

type Handlers struct {
	fx.In

	Graphql        *gql.GraphqlHandler
	OpenAPIGraphql *openapi.GraphqlHandler
	OpenAI         *api.OpenAIHandlers
	Doubao         *api.DoubaoHandlers
	Anthropic      *api.AnthropicHandlers
	Gemini         *api.GeminiHandlers
	AiSDK          *api.AiSDKHandlers
	Playground     *api.PlaygroundHandlers
	System         *api.SystemHandlers
	Auth           *api.AuthHandlers
	Self           *api.SelfServiceHandlers
	Jina           *api.JinaHandlers
	Codex          *api.CodexHandlers
	ClaudeCode     *api.ClaudeCodeHandlers
	Antigravity    *api.AntigravityHandlers
	Copilot        *api.CopilotHandlers
	RequestContent *api.RequestContentHandlers
	OIDC           *api.OIDCHandlers
	RequestPreview *api.RequestPreviewHandlers
	Diagnostics    *api.DiagnosticsHandlers
}

type Services struct {
	fx.In

	TraceService  *biz.TraceService
	ThreadService *biz.ThreadService
	AuthService   *biz.AuthService
	SystemService *biz.SystemService
}

func SetupRoutes(server *Server, handlers Handlers, client *ent.Client, services Services, ipAccessControl *middleware.IPAccessControlConfig) {
	// IP 访问控制 - 全局最优先，拦截所有请求包括静态文件
	server.Use(middleware.WithIPAccessControl(ipAccessControl))

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
		unSecureAdminGroup.GET("/auth/signup-policy", handlers.Auth.SignUpPolicy)
		unSecureAdminGroup.POST("/auth/signup", handlers.Auth.SignUp)
	}

	oauthGroup := server.Group("/oauth", middleware.WithTimeout(server.Config.RequestTimeout))
	{
		handlers.OIDC.RegisterRoutes(oauthGroup)
	}

	adminGroup := server.Group("/admin", middleware.WithJWTAuth(services.AuthService), middleware.WithProjectID())
	// 管理员路由 - 使用 JWT 认证
	{
		adminGroup.GET("/playground", middleware.WithTimeout(server.Config.RequestTimeout), func(c *gin.Context) {
			handlers.Graphql.Playground.ServeHTTP(c.Writer, c.Request)
		})
		adminGroup.POST("/graphql", middleware.WithTimeout(server.Config.RequestTimeout), func(c *gin.Context) {
			handlers.Graphql.Graphql.ServeHTTP(c.Writer, c.Request)
		})
		selfGroup := adminGroup.Group("/self")
		{
			selfGroup.GET("/api-keys", handlers.Self.ListAPIKeys)
			selfGroup.POST("/api-keys", handlers.Self.CreateAPIKey)
			selfGroup.PATCH("/api-keys/:id", handlers.Self.UpdateAPIKey)
			selfGroup.PATCH("/api-keys/:id/status", handlers.Self.UpdateAPIKeyStatus)
			selfGroup.POST("/api-keys/:id/rotate", handlers.Self.RotateAPIKey)
			selfGroup.POST("/api-keys/:id/reveal", handlers.Self.RevealAPIKey)
			selfGroup.POST("/api-keys/:id/classify", handlers.Self.ClassifyMyLegacyAPIKey)
			selfGroup.GET("/routing-presets", handlers.Self.ListRoutingPresets)
			selfGroup.GET("/access-groups", handlers.Self.ListAccessGroups)
			selfGroup.POST("/access-groups/:id/api-keys", handlers.Self.CreateAPIKeyForAccessGroup)
			selfGroup.GET("/models", handlers.Self.ListModels)
			selfGroup.GET("/requests", handlers.Self.ListRequests)
			selfGroup.GET("/requests/:id", middleware.WithTimeout(server.Config.RequestTimeout), handlers.Self.GetRequest)
			selfGroup.GET("/usage", handlers.Self.Usage)
		}

		adminGroup.GET("/access-groups", handlers.Self.ListAdminAccessGroups)
		adminGroup.POST("/access-groups", handlers.Self.CreateAdminAccessGroup)
		adminGroup.GET("/access-groups/:id", handlers.Self.GetAdminAccessGroup)
		adminGroup.PATCH("/access-groups/:id", handlers.Self.UpdateAdminAccessGroup)
		adminGroup.PATCH("/access-groups/:id/channels", handlers.Self.AddChannelsToAccessGroup)
		adminGroup.POST("/api-keys/:id/classify", handlers.Self.ClassifyLegacyAPIKey)
		adminGroup.POST("/api-keys/:id/detach-access-group", handlers.Self.DetachAPIKeyAccessGroup)

		adminGroup.GET("/auth/registration-policy", handlers.Auth.AdminRegistrationPolicy)
		adminGroup.PUT("/auth/registration-policy", handlers.Auth.UpdateRegistrationPolicy)

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
	}
	server.POST(
		"/admin/diagnostics/v1/pull",
		middleware.WithJWTAuthResponder(services.AuthService, handlers.Diagnostics.MiddlewareError),
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
	}
	server.POST(
		"/openapi/v1/diagnostics/pull",
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithOpenAPIAuthResponder(services.AuthService, handlers.Diagnostics.MiddlewareError),
		middleware.WithTimeout(server.Config.RequestTimeout),
		handlers.Diagnostics.Pull,
	)

	apiGroup := server.Group("/",
		middleware.WithTimeout(server.Config.LLMRequestTimeout),
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithAPIKeyConfig(services.AuthService, nil),
		middleware.WithSource(request.SourceAPI),
		middleware.WithThread(server.Config.Trace, services.ThreadService),
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
			middleware.WithTrace(server.Config.Trace, services.TraceService),
		)

		registerGeminiRoutes(geminiAliasGroup)
	}
}
