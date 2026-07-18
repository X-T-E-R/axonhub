package diagnostics

import (
	"time"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

// These handwritten types are internal projection aids only. The shared JSON
// Schema is the protocol source of truth and validates all emitted responses at
// the HTTP boundary.

type HealthDataContract struct {
	ProcessStatus     string     `json:"processStatus"`
	DatabaseStatus    string     `json:"databaseStatus"`
	DatabaseLatencyMs *int64     `json:"databaseLatencyMs"`
	UptimeSeconds     int64      `json:"uptimeSeconds"`
	Version           string     `json:"version"`
	Commit            string     `json:"commit"`
	BuildTime         *time.Time `json:"buildTime"`
}

type RoutingContextStateContract struct {
	State string                  `json:"state"`
	Value *objects.RoutingContext `json:"value"`
}

type ContentArtifactContract struct {
	Saved     bool       `json:"saved"`
	StorageID *int       `json:"storageId"`
	Key       *string    `json:"key"`
	SavedAt   *time.Time `json:"savedAt"`
	Content   Evidence   `json:"content"`
}

type RequestRecordContract struct {
	ID                         int                         `json:"id"`
	ProjectID                  int                         `json:"projectId"`
	APIKeyID                   *int                        `json:"apiKeyId"`
	TraceDatabaseID            *int                        `json:"traceDatabaseId"`
	DataStorageID              *int                        `json:"dataStorageId"`
	Source                     string                      `json:"source"`
	ModelID                    string                      `json:"modelId"`
	ReasoningEffort            string                      `json:"reasoningEffort"`
	Format                     string                      `json:"format"`
	RequestHeaders             Evidence                    `json:"requestHeaders"`
	RequestBody                Evidence                    `json:"requestBody"`
	ResponseBody               Evidence                    `json:"responseBody"`
	ResponseChunks             Evidence                    `json:"responseChunks"`
	ChannelID                  *int                        `json:"channelId"`
	ExternalID                 string                      `json:"externalId"`
	Status                     string                      `json:"status"`
	Stream                     bool                        `json:"stream"`
	ClientIP                   string                      `json:"clientIp"`
	MetricsLatencyMs           *int64                      `json:"metricsLatencyMs"`
	MetricsFirstTokenLatencyMs *int64                      `json:"metricsFirstTokenLatencyMs"`
	MetricsReasoningDurationMs *int64                      `json:"metricsReasoningDurationMs"`
	ContentArtifact            ContentArtifactContract     `json:"contentArtifact"`
	RoutingContext             RoutingContextStateContract `json:"routingContext"`
	CreatedAt                  time.Time                   `json:"createdAt"`
	UpdatedAt                  time.Time                   `json:"updatedAt"`
}

type ExecutionRecordContract struct {
	ID                         int       `json:"id"`
	ProjectID                  int       `json:"projectId"`
	RequestID                  int       `json:"requestId"`
	ChannelID                  *int      `json:"channelId"`
	DataStorageID              *int      `json:"dataStorageId"`
	ExternalID                 string    `json:"externalId"`
	ModelID                    string    `json:"modelId"`
	Format                     string    `json:"format"`
	RequestHeaders             Evidence  `json:"requestHeaders"`
	RequestBody                Evidence  `json:"requestBody"`
	ResponseBody               Evidence  `json:"responseBody"`
	ResponseChunks             Evidence  `json:"responseChunks"`
	ErrorMessage               string    `json:"errorMessage"`
	ResponseStatusCode         *int      `json:"responseStatusCode"`
	Status                     string    `json:"status"`
	Stream                     bool      `json:"stream"`
	MetricsLatencyMs           *int64    `json:"metricsLatencyMs"`
	MetricsFirstTokenLatencyMs *int64    `json:"metricsFirstTokenLatencyMs"`
	MetricsReasoningDurationMs *int64    `json:"metricsReasoningDurationMs"`
	RequestURL                 string    `json:"requestUrl"`
	PassThroughApplied         bool      `json:"passThroughApplied"`
	CreatedAt                  time.Time `json:"createdAt"`
	UpdatedAt                  time.Time `json:"updatedAt"`
}

type UsageRecordContract struct {
	ID                                 int                `json:"id"`
	RequestID                          int                `json:"requestId"`
	APIKeyID                           *int               `json:"apiKeyId"`
	ProjectID                          int                `json:"projectId"`
	ChannelID                          *int               `json:"channelId"`
	ModelID                            string             `json:"modelId"`
	PromptTokens                       int64              `json:"promptTokens"`
	CompletionTokens                   int64              `json:"completionTokens"`
	TotalTokens                        int64              `json:"totalTokens"`
	PromptAudioTokens                  int64              `json:"promptAudioTokens"`
	PromptCachedTokens                 int64              `json:"promptCachedTokens"`
	PromptWriteCachedTokens            int64              `json:"promptWriteCachedTokens"`
	PromptWriteCachedTokens5m          int64              `json:"promptWriteCachedTokens5m"`
	PromptWriteCachedTokens1h          int64              `json:"promptWriteCachedTokens1h"`
	CompletionAudioTokens              int64              `json:"completionAudioTokens"`
	CompletionReasoningTokens          int64              `json:"completionReasoningTokens"`
	CompletionAcceptedPredictionTokens int64              `json:"completionAcceptedPredictionTokens"`
	CompletionRejectedPredictionTokens int64              `json:"completionRejectedPredictionTokens"`
	Source                             string             `json:"source"`
	Format                             string             `json:"format"`
	TotalCost                          *string            `json:"totalCost"`
	CostItems                          []objects.CostItem `json:"costItems"`
	CostPriceReferenceID               string             `json:"costPriceReferenceId"`
	CreatedAt                          time.Time          `json:"createdAt"`
	UpdatedAt                          time.Time          `json:"updatedAt"`
}

type TraceRecordContract struct {
	ID               int       `json:"id"`
	ProjectID        int       `json:"projectId"`
	TraceID          string    `json:"traceId"`
	ThreadDatabaseID *int      `json:"threadDatabaseId"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type ThreadRecordContract struct {
	ID        int       `json:"id"`
	ProjectID int       `json:"projectId"`
	ThreadID  string    `json:"threadId"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type CredentialAwareURLContract struct {
	Base     string     `json:"base"`
	UserInfo Credential `json:"userInfo"`
	Query    Credential `json:"query"`
}

type OverrideMatchContract struct {
	Path string     `json:"path"`
	Eq   Credential `json:"eq"`
}

type OverrideOperationContract struct {
	Op        string                 `json:"op"`
	Path      string                 `json:"path,omitempty"`
	From      string                 `json:"from,omitempty"`
	To        string                 `json:"to,omitempty"`
	Value     *Credential            `json:"value,omitempty"`
	Condition string                 `json:"condition,omitempty"`
	Match     *OverrideMatchContract `json:"match,omitempty"`
	Index     *int                   `json:"index,omitempty"`
	Splat     *bool                  `json:"splat,omitempty"`
}

type HeaderEntryContract struct {
	Key   string     `json:"key"`
	Value Credential `json:"value"`
}

type ProxyContract struct {
	Type     string                     `json:"type"`
	URL      CredentialAwareURLContract `json:"url"`
	Username Credential                 `json:"username"`
	Password Credential                 `json:"password"`
}

type OpenCodeGoQuotaContract struct {
	WorkspaceID string     `json:"workspaceId,omitempty"`
	AuthCookie  Credential `json:"authCookie"`
}

type ProviderQuotaContract struct {
	OpenCodeGo *OpenCodeGoQuotaContract `json:"opencodeGo,omitempty"`
}

type ChannelSettingsContract struct {
	ExtraModelPrefix           string                          `json:"extraModelPrefix"`
	AutoTrimedModelPrefixes    []string                        `json:"autoTrimedModelPrefixes"`
	ModelMappings              []objects.ModelMapping          `json:"modelMappings"`
	HideOriginalModels         bool                            `json:"hideOriginalModels"`
	HideMappedModels           bool                            `json:"hideMappedModels"`
	LowercaseModelID           bool                            `json:"lowercaseModelId"`
	OverrideParameters         Credential                      `json:"overrideParameters"`
	BodyOverrideOperations     []OverrideOperationContract     `json:"bodyOverrideOperations,omitempty"`
	OverrideHeaders            []HeaderEntryContract           `json:"overrideHeaders"`
	HeaderOverrideOperations   []OverrideOperationContract     `json:"headerOverrideOperations,omitempty"`
	Proxy                      *ProxyContract                  `json:"proxy,omitempty"`
	TransformOptions           objects.TransformOptions        `json:"transformOptions"`
	PassThroughUserAgent       *bool                           `json:"passThroughUserAgent,omitempty"`
	PassThroughBody            *bool                           `json:"passThroughBody,omitempty"`
	DisableRetries             bool                            `json:"disableRetries,omitempty"`
	FullPassThrough            bool                            `json:"fullPassThrough,omitempty"`
	StoreExecutionRequestBody  *bool                           `json:"storeExecutionRequestBody,omitempty"`
	StoreExecutionResponseBody *bool                           `json:"storeExecutionResponseBody,omitempty"`
	StoreExecutionStreamChunks *bool                           `json:"storeExecutionStreamChunks,omitempty"`
	RateLimit                  *objects.ChannelRateLimit       `json:"rateLimit,omitempty"`
	RetryableStatusCodes       []int                           `json:"retryableStatusCodes,omitempty"`
	RetryableErrorPatterns     []objects.RetryableErrorPattern `json:"retryableErrorPatterns,omitempty"`
	ProviderQuota              *ProviderQuotaContract          `json:"providerQuota,omitempty"`
}

type ChannelEndpointContract struct {
	APIFormat string                     `json:"apiFormat"`
	Path      string                     `json:"path"`
	BaseURL   CredentialAwareURLContract `json:"baseUrl"`
	Transport string                     `json:"transport"`
}

type ChannelRecordContract struct {
	ID                      int                        `json:"id"`
	Type                    string                     `json:"type"`
	BaseURL                 CredentialAwareURLContract `json:"baseUrl"`
	Name                    string                     `json:"name"`
	Status                  string                     `json:"status"`
	Credentials             Credential                 `json:"credentials"`
	DisabledAPIKeys         Credential                 `json:"disabledApiKeys"`
	SupportedModels         []string                   `json:"supportedModels"`
	ManualModels            []string                   `json:"manualModels"`
	AutoSyncSupportedModels bool                       `json:"autoSyncSupportedModels"`
	AutoSyncModelPattern    string                     `json:"autoSyncModelPattern"`
	Tags                    []string                   `json:"tags"`
	DefaultTestModel        string                     `json:"defaultTestModel"`
	Policies                objects.ChannelPolicies    `json:"policies"`
	Settings                *ChannelSettingsContract   `json:"settings"`
	OrderingWeight          int                        `json:"orderingWeight"`
	ErrorMessage            *string                    `json:"errorMessage"`
	Remark                  *string                    `json:"remark"`
	Endpoints               []ChannelEndpointContract  `json:"endpoints"`
	CreatedAt               time.Time                  `json:"createdAt"`
	UpdatedAt               time.Time                  `json:"updatedAt"`
}

type APIKeyRecordContract struct {
	ID                  int                     `json:"id"`
	UserID              *int                    `json:"userId"`
	ProjectID           int                     `json:"projectId"`
	Name                string                  `json:"name"`
	Type                string                  `json:"type"`
	Status              string                  `json:"status"`
	Scopes              []string                `json:"scopes"`
	Profiles            *objects.APIKeyProfiles `json:"profiles"`
	ProvisioningSource  string                  `json:"provisioningSource"`
	ProfileMode         string                  `json:"profileMode"`
	AccessGroupID       *int                    `json:"accessGroupId"`
	AccessGroupRevision *int64                  `json:"accessGroupRevision"`
	Key                 Credential              `json:"key"`
	CreatedAt           time.Time               `json:"createdAt"`
	UpdatedAt           time.Time               `json:"updatedAt"`
}

type AccessGroupRevisionContract struct {
	Revision        int64                  `json:"revision"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description"`
	Profile         *objects.APIKeyProfile `json:"profile"`
	CreatedAt       time.Time              `json:"createdAt"`
	CreatedByUserID *int                   `json:"createdByUserId"`
}

type AccessGroupRecordContract struct {
	ID                  int                           `json:"id"`
	ProjectID           int                           `json:"projectId"`
	Name                string                        `json:"name"`
	Description         string                        `json:"description"`
	Profile             *objects.APIKeyProfile        `json:"profile"`
	Revision            int64                         `json:"revision"`
	SelfServiceVisible  bool                          `json:"selfServiceVisible"`
	AttachedKeyCount    int                           `json:"attachedKeyCount"`
	CreatedAt           time.Time                     `json:"createdAt"`
	UpdatedAt           time.Time                     `json:"updatedAt"`
	ReferencedRevisions []AccessGroupRevisionContract `json:"referencedRevisions"`
}

type S3SettingsContract struct {
	BucketName string                     `json:"bucketName"`
	Endpoint   CredentialAwareURLContract `json:"endpoint"`
	Region     string                     `json:"region"`
	AccessKey  Credential                 `json:"accessKey"`
	SecretKey  Credential                 `json:"secretKey"`
	PathStyle  bool                       `json:"pathStyle"`
}

type GCSSettingsContract struct {
	BucketName string     `json:"bucketName"`
	Credential Credential `json:"credential"`
}

type WebDAVSettingsContract struct {
	URL             CredentialAwareURLContract `json:"url"`
	Username        Credential                 `json:"username"`
	Password        Credential                 `json:"password"`
	InsecureSkipTLS bool                       `json:"insecureSkipTls"`
	Path            string                     `json:"path"`
}

type DataStorageSettingsContract struct {
	DSN       Credential              `json:"dsn"`
	Directory *string                 `json:"directory"`
	S3        *S3SettingsContract     `json:"s3"`
	GCS       *GCSSettingsContract    `json:"gcs"`
	WebDAV    *WebDAVSettingsContract `json:"webdav"`
}

type DataStorageRecordContract struct {
	ID          int                         `json:"id"`
	Name        string                      `json:"name"`
	Description string                      `json:"description"`
	Primary     bool                        `json:"primary"`
	Type        string                      `json:"type"`
	Status      string                      `json:"status"`
	Settings    DataStorageSettingsContract `json:"settings"`
	CreatedAt   time.Time                   `json:"createdAt"`
	UpdatedAt   time.Time                   `json:"updatedAt"`
}

type RegistrationContract struct {
	Enabled              bool       `json:"enabled"`
	OIDCEnabled          bool       `json:"oidcEnabled"`
	DefaultProjectID     int        `json:"defaultProjectId"`
	AutoJoinFirstProject bool       `json:"autoJoinFirstProject"`
	DefaultProjectScopes []string   `json:"defaultProjectScopes"`
	SelfServiceEnabled   bool       `json:"selfServiceEnabled"`
	AllowRequestDetails  bool       `json:"allowRequestDetails"`
	InviteCode           Credential `json:"inviteCode"`
}

type ConfigurationDataContract struct {
	StoragePolicy        biz.StoragePolicy           `json:"storagePolicy"`
	RetryPolicy          biz.RetryPolicy             `json:"retryPolicy"`
	ModelSettings        biz.SystemModelSettings     `json:"modelSettings"`
	UserAgentPassThrough bool                        `json:"userAgentPassThrough"`
	BodyPassThrough      bool                        `json:"bodyPassThrough"`
	DefaultDataStorageID *int                        `json:"defaultDataStorageId"`
	DataStorages         []DataStorageRecordContract `json:"dataStorages"`
	Registration         RegistrationContract        `json:"registration"`
}
