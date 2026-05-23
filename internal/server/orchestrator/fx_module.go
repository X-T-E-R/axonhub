package orchestrator

import (
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"go.uber.org/fx"
)

var Module = fx.Module("orchestrator",
	fx.Provide(NewDefaultSelector),
	fx.Provide(NewCandidateSelectorDiagnostics),
	fx.Provide(NewChannelLimiterManager),
	fx.Provide(func(svc *biz.ProviderQuotaService) ProviderQuotaStatusProvider { return svc }),
	fx.Invoke(func(channelService *biz.ChannelService, requestService *biz.RequestService, systemService *biz.SystemService, usageLogService *biz.UsageLogService, promptProtectionRuleService *biz.PromptProtectionRuleService, httpClient *httpclient.HttpClient) {
		channelService.SetChannelKeyHealthCheckTester(NewTestChannelOrchestrator(channelService, requestService, systemService, usageLogService, promptProtectionRuleService, httpClient))
	}),
)
