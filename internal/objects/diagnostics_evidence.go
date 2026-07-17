package objects

import "time"

// RoutingContext is an immutable snapshot of the policy inputs used to route a
// request. It is intentionally stored on Request instead of reconstructed from
// mutable API-key or access-group rows.
type RoutingContext struct {
	Version                 int             `json:"version"`
	APIKeyID                int             `json:"apiKeyId"`
	APIKeyType              string          `json:"apiKeyType"`
	ProvisioningSource      string          `json:"provisioningSource"`
	ProfileMode             string          `json:"profileMode"`
	AccessGroupID           *int            `json:"accessGroupId"`
	AccessGroupRevision     *int64          `json:"accessGroupRevision"`
	EffectiveProfiles       *APIKeyProfiles `json:"effectiveProfiles"`
	EffectiveProfilesSHA256 string          `json:"effectiveProfilesSha256"`
	RequestedModelID        string          `json:"requestedModelId"`
}

// EvidenceDisposition records why a body/chunk field is or is not available.
// It contains metadata only and never duplicates evidence bytes.
type EvidenceDisposition struct {
	Version        int         `json:"version"`
	RequestBody    Disposition `json:"requestBody"`
	ResponseBody   Disposition `json:"responseBody"`
	ResponseChunks Disposition `json:"responseChunks"`
}

type Disposition struct {
	Intent       string    `json:"intent"`
	Location     string    `json:"location"`
	Outcome      string    `json:"outcome"`
	StorageID    *int      `json:"storageId"`
	StorageKey   *string   `json:"storageKey"`
	CapturedAt   time.Time `json:"capturedAt"`
	FailureClass *string   `json:"failureClass"`
}
