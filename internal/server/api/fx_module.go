package api

import (
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/server/diagnostics"
)

var Module = fx.Module("api",
	fx.Provide(diagnostics.NewService),
	fx.Provide(NewOpenAIHandlers),
	fx.Provide(NewAnthropicHandlers),
	fx.Provide(NewGeminiHandlers),
	fx.Provide(NewAiSDKHandlers),
	fx.Provide(NewPlaygroundHandlers),
	fx.Provide(NewSystemHandlers),
	fx.Provide(NewAuthHandlers),
	fx.Provide(NewAPIKeyHandlers),
	fx.Provide(NewSelfServiceHandlers),
	fx.Provide(NewJinaHandlers),
	fx.Provide(NewDoubaoHandlers),
	fx.Provide(NewCodexHandlers),
	fx.Provide(NewClaudeCodeHandlers),
	fx.Provide(NewAntigravityHandlers),
	fx.Provide(NewCopilotHandlers),
	fx.Provide(NewRequestContentHandlers),
	fx.Provide(NewOIDCHandlers),
	fx.Provide(NewRequestPreviewHandlers),
	fx.Provide(NewDiagnosticsHandlers),
	fx.Invoke(initLogger),
)
