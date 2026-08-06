package biz

import (
	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/objects"
)

var defaultStoragePolicy = StoragePolicy{
	StoreChunks:               false,
	LivePreview:               false,
	StoreRequestBody:          true,
	StoreExecutionRequestBody: lo.ToPtr(true),
	StoreResponseBody:         true,
	CleanupOptions: []CleanupOption{
		{
			ResourceType: "requests",
			Enabled:      false,
			CleanupDays:  3,
		},
		{
			ResourceType: "usage_logs",
			Enabled:      false,
			CleanupDays:  30,
		},
	},
}

var defaultRetryPolicy = RetryPolicy{
	MaxChannelRetries:       3,
	MaxSingleChannelRetries: 2,
	RetryDelayMs:            1000,
	LoadBalancerStrategy:    "adaptive",
	Enabled:                 true,
	EmptyResponseTextPatterns: []string{
		"The request was rejected because it was considered high risk",
	},
	UpstreamErrorPolicy: UpstreamErrorPolicy{
		Mode: UpstreamErrorModePassthrough,
	},
}

var defaultModelSettings = SystemModelSettings{
	FallbackToChannelsOnModelNotFound: true,
	QueryAllChannelModels:             true,
	DefaultModelAPIIncludeAll:         false,
	AutoReasoningEffort:               false,
	ModelBlacklistRegex:               "",
	DeveloperSettings:                 []*DeveloperModelSettings{},
}

var defaultChannelSetting = SystemChannelSettings{
	Probe: ChannelProbeSetting{
		Enabled:   true,
		Frequency: ProbeFrequency5Min,
	},
	AutoSync: ChannelModelAutoSyncSetting{
		Frequency: AutoSyncFrequencyOneHour,
	},
	ActionMenu: ChannelActionMenuSetting{
		AdvancedActionsMode: ChannelAdvancedActionMenuModeGrouped,
	},
	Routing: ChannelKeyRoutingSetting{
		Strategy: objects.ChannelKeySelectionStrategyTraceSticky,
	},
}

var defaultGeneralSettings = SystemGeneralSettings{
	CurrencyCode: "USD",
	Timezone:     "UTC",
}

var defaultAutoBackupSettings = AutoBackupSettings{
	Enabled:            false,
	Frequency:          BackupFrequencyDaily,
	IncludeChannels:    true,
	IncludeModels:      true,
	IncludeAPIKeys:     false,
	IncludeModelPrices: true,
	IncludeUsageStats:  false,
	IncludeRequestLogs: false,
	RetentionDays:      30,
}

var defaultVideoStorageSettings = VideoStorageSettings{
	Enabled:             false,
	DataStorageID:       0,
	ScanIntervalMinutes: 1,
	ScanLimit:           50,
}

var defaultRequestObservabilitySettings = RequestObservabilitySettings{
	ExposeSelectedChannelAPIKey: false,
}

var defaultQuotaEnforcementSettings = QuotaEnforcementSettings{
	Enabled: false,
	Mode:    QuotaEnforcementModeExhaustedOnly,
}

var defaultSecuritySettings = SecuritySettings{
	BlockedIPs:              []string{},
	ShowRequestLogIPBanIcon: true,
}

var defaultMonitoringSettings = MonitoringSettings{
	Enabled:              false,
	HistoryRetentionDays: 30,
	Rules:                []MonitoringRule{},
}

var defaultMonitoringRuleSchedule = MonitoringRuleSchedule{
	IntervalMinutes:   60,
	HistoryLimit:      100,
	MaxChannels:       4,
	MaxKeysPerChannel: 8,
	KeySpacingMs:      1000,
	JitterMs:          250,
}

var defaultMonitoringRuleTargets = MonitoringRuleTargets{
	ChannelStatuses: []string{"enabled"},
	KeyStatuses:     []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive},
	ChannelIDs:      []int{},
	IncludeBackoff:  false,
}
