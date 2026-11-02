package objects

import "time"

// Portal DTOs contain no Ent edges, routing credentials or upstream evidence.
type PortalIdentity struct {
	ID             int    `json:"id"`
	Email          string `json:"email"`
	FirstName      string `json:"firstName"`
	LastName       string `json:"lastName"`
	PreferLanguage string `json:"preferLanguage"`
	AccessMode     string `json:"accessMode"`
	Avatar         string `json:"avatar"`
	HasPassword    bool   `json:"hasPassword"`
}
type PortalGroup struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Callable bool   `json:"callable"`
	Member   bool   `json:"member"`
}
type PortalDocument struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Markdown  string `json:"markdown"`
	Published bool   `json:"published"`
	Pinned    bool   `json:"pinned"`
	Order     int    `json:"order"`
}
type PortalSettings struct {
	Announcements []*PortalDocument `json:"announcements"`
	Guides        []*PortalDocument `json:"guides"`
}
type PortalKey struct {
	ID         int            `json:"id"`
	ProjectID  int            `json:"projectID"`
	Name       string         `json:"name"`
	MaskedKey  string         `json:"maskedKey"`
	Status     string         `json:"status"`
	ModelIDs   []string       `json:"modelIDs"`
	AllowedIPs []string       `json:"allowedIPs"`
	CreatedAt  time.Time      `json:"createdAt"`
	Quotas     []*PortalQuota `json:"quotas"`
}
type PortalQuota struct {
	ProfileName  string     `json:"profileName"`
	Active       bool       `json:"active"`
	Requests     *int64     `json:"requests"`
	TotalTokens  *int64     `json:"totalTokens"`
	Cost         *string    `json:"cost"`
	UsedRequests int64      `json:"usedRequests"`
	UsedTokens   int64      `json:"usedTokens"`
	UsedCost     string     `json:"usedCost"`
	Start        *time.Time `json:"start"`
	End          *time.Time `json:"end"`
}
type PortalKeyInput struct {
	Name       string   `json:"name"`
	ModelIDs   []string `json:"modelIDs"`
	AllowedIPs []string `json:"allowedIPs"`
}
type PortalKeyPage struct {
	Items      []*PortalKey `json:"items"`
	TotalCount int          `json:"totalCount"`
	NextCursor *int         `json:"nextCursor"`
}
type PortalModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Developer       string   `json:"developer"`
	Icon            string   `json:"icon"`
	Remark          *string  `json:"remark"`
	Vision          bool     `json:"vision"`
	ToolCall        bool     `json:"toolCall"`
	Reasoning       bool     `json:"reasoning"`
	ContextLength   *int     `json:"contextLength"`
	MaxOutputTokens *int     `json:"maxOutputTokens"`
	InputPrice      *float64 `json:"inputPrice"`
	OutputPrice     *float64 `json:"outputPrice"`
	APIFormats      []string `json:"apiFormats"`
}
type PortalModelUsage struct {
	ModelID      string   `json:"modelID"`
	UsageCount   int      `json:"usageCount"`
	InputTokens  int64    `json:"inputTokens"`
	OutputTokens int64    `json:"outputTokens"`
	CachedTokens int64    `json:"cachedTokens"`
	TotalTokens  int64    `json:"totalTokens"`
	PricedCount  int      `json:"pricedCount"`
	Cost         *float64 `json:"cost"`
}
type PortalDayUsage struct {
	Day         string `json:"day"`
	TotalTokens int64  `json:"totalTokens"`
}
type PortalRequest struct {
	ID            int       `json:"id"`
	CreatedAt     time.Time `json:"createdAt"`
	Status        string    `json:"status"`
	ModelID       string    `json:"modelID"`
	UsageReported bool      `json:"usageReported"`
	TotalTokens   *int64    `json:"totalTokens"`
	Cost          *float64  `json:"cost"`
}
type PortalDashboard struct {
	Requests           int                 `json:"requests"`
	SuccessfulRequests int                 `json:"successfulRequests"`
	UsageCount         int                 `json:"usageCount"`
	InputTokens        int64               `json:"inputTokens"`
	OutputTokens       int64               `json:"outputTokens"`
	CachedTokens       int64               `json:"cachedTokens"`
	TotalTokens        int64               `json:"totalTokens"`
	PricedCount        int                 `json:"pricedCount"`
	Cost               *float64            `json:"cost"`
	Models             []*PortalModelUsage `json:"models"`
	Days               []*PortalDayUsage   `json:"days"`
	RecentRequests     []*PortalRequest    `json:"recentRequests"`
}
