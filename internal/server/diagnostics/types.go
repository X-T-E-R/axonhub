package diagnostics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	contractv1 "github.com/looplj/axonhub/internal/server/diagnostics/contract/v1"
)

type ContractRequest struct {
	Name     string `json:"name"`
	Major    int    `json:"major"`
	MinMinor int    `json:"minMinor"`
	MaxMinor int    `json:"maxMinor"`
}
type Scope struct {
	ProjectID     int  `json:"projectId"`
	SubjectUserID *int `json:"subjectUserId,omitempty"`
}
type Selector struct {
	Kind      string          `json:"kind"`
	IDs       json.RawMessage `json:"ids,omitempty"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	Statuses  []string        `json:"statuses,omitempty"`
	ModelIDs  []string        `json:"modelIds,omitempty"`
	APIKeyIDs []int           `json:"apiKeyIds,omitempty"`
}
type Include struct {
	Sections    []string `json:"sections"`
	Credentials bool     `json:"credentials,omitempty"`
}
type Limits struct {
	MaxRequests       int `json:"maxRequests,omitempty"`
	MaxExecutions     int `json:"maxExecutions,omitempty"`
	MaxRelatedRecords int `json:"maxRelatedRecords,omitempty"`
	MaxResponseBytes  int `json:"maxResponseBytes,omitempty"`
}
type Page struct {
	Cursor *string `json:"cursor"`
}
type PullRequest struct {
	Contract ContractRequest `json:"contract"`
	Scope    Scope           `json:"scope"`
	Selector Selector        `json:"selector"`
	Include  Include         `json:"include"`
	Limits   Limits          `json:"limits,omitempty"`
	Page     *Page           `json:"page,omitempty"`
}

// DecodeContractPullRequest validates and decodes the protocol boundary into
// the Go type generated from the authoritative JSON Schema.
func DecodeContractPullRequest(r io.Reader) (contractv1.PullRequest, error) {
	var req contractv1.PullRequest
	raw, err := io.ReadAll(r)
	if err != nil {
		return req, fmt.Errorf("invalid diagnostics request: %w", err)
	}
	if err := ValidatePullRequestJSON(raw); err != nil {
		return req, fmt.Errorf("invalid diagnostics request: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Selector is a generated union represented as interface{}. UseNumber keeps
	// every numeric lexeme exact until the adapter stores selector IDs as raw JSON.
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid diagnostics request: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return req, fmt.Errorf("diagnostics request must contain one JSON object")
	}
	return req, nil
}

// PullRequestFromContract adapts the generated protocol projection to the
// internal service model after boundary schema validation.
func PullRequestFromContract(req contractv1.PullRequest) (PullRequest, error) {
	var internal PullRequest
	raw, err := json.Marshal(req)
	if err != nil {
		return internal, fmt.Errorf("adapt diagnostics request: %w", err)
	}
	if err := json.Unmarshal(raw, &internal); err != nil {
		return internal, fmt.Errorf("adapt diagnostics request: %w", err)
	}
	return internal, nil
}

// DecodePullRequest is retained for internal callers. Protocol decoding still
// occurs into the schema-generated type before adaptation to the service model.
func DecodePullRequest(r io.Reader) (PullRequest, error) {
	boundary, err := DecodeContractPullRequest(r)
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequestFromContract(boundary)
}

// PullResponseToContract validates the internal response and adapts it to the
// generated protocol type used by the HTTP boundary.
func PullResponseToContract(response *PullResponse) (contractv1.PullResponse, error) {
	var boundary contractv1.PullResponse
	raw, err := json.Marshal(response)
	if err != nil {
		return boundary, fmt.Errorf("adapt diagnostics response: %w", err)
	}
	if err := ValidatePullResponseJSON(raw); err != nil {
		return boundary, fmt.Errorf("adapt diagnostics response: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&boundary); err != nil {
		return boundary, fmt.Errorf("adapt diagnostics response: %w", err)
	}
	// Nullable section data is generated as interface{} by the pinned generator.
	// Reattach it as RawMessage so evidence values keep exact numeric, escape,
	// whitespace, and object-order lexemes when the generated response is emitted.
	var envelope struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return boundary, fmt.Errorf("preserve diagnostics section data: %w", err)
	}
	sectionData := func(name string) (json.RawMessage, error) {
		var section struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(envelope.Sections[name], &section); err != nil {
			return nil, err
		}
		return section.Data, nil
	}
	dataTargets := []struct {
		name string
		set  func(json.RawMessage)
	}{
		{"health", func(data json.RawMessage) { boundary.Sections.Health.Data = data }},
		{"configuration", func(data json.RawMessage) { boundary.Sections.Configuration.Data = data }},
		{"requests", func(data json.RawMessage) { boundary.Sections.Requests.Data = data }},
		{"executions", func(data json.RawMessage) { boundary.Sections.Executions.Data = data }},
		{"usage", func(data json.RawMessage) { boundary.Sections.Usage.Data = data }},
		{"traces", func(data json.RawMessage) { boundary.Sections.Traces.Data = data }},
		{"threads", func(data json.RawMessage) { boundary.Sections.Threads.Data = data }},
		{"channels", func(data json.RawMessage) { boundary.Sections.Channels.Data = data }},
		{"apiKeys", func(data json.RawMessage) { boundary.Sections.APIKeys.Data = data }},
		{"accessGroups", func(data json.RawMessage) { boundary.Sections.AccessGroups.Data = data }},
	}
	for _, target := range dataTargets {
		data, err := sectionData(target.name)
		if err != nil {
			return boundary, fmt.Errorf("preserve diagnostics %s section data: %w", target.name, err)
		}
		target.set(data)
	}
	return boundary, nil
}

type Issue struct {
	Code       string `json:"code"`
	Section    string `json:"section,omitempty"`
	RecordType string `json:"recordType,omitempty"`
	RecordID   string `json:"recordId,omitempty"`
	Retryable  bool   `json:"retryable"`
	Message    string `json:"message"`
}
type Section struct {
	Status string  `json:"status"`
	Data   any     `json:"data"`
	Issues []Issue `json:"issues"`
}
type Sections struct {
	Health        Section `json:"health"`
	Configuration Section `json:"configuration"`
	Requests      Section `json:"requests"`
	Executions    Section `json:"executions"`
	Usage         Section `json:"usage"`
	Traces        Section `json:"traces"`
	Threads       Section `json:"threads"`
	Channels      Section `json:"channels"`
	APIKeys       Section `json:"apiKeys"`
	AccessGroups  Section `json:"accessGroups"`
}
type ContractResponse struct {
	Name         string `json:"name"`
	Major        int    `json:"major"`
	Minor        int    `json:"minor"`
	SchemaSHA256 string `json:"schemaSha256"`
}
type Bundle struct {
	ID              string `json:"id"`
	GeneratedAt     string `json:"generatedAt"`
	PageIndex       int    `json:"pageIndex"`
	PageGeneratedAt string `json:"pageGeneratedAt"`
	Status          string `json:"status"`
}
type ServerInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuildTime     any    `json:"buildTime"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}
type Authorization struct {
	PrincipalType        string `json:"principalType"`
	PrincipalID          int    `json:"principalId"`
	ProjectID            int    `json:"projectId"`
	SubjectUserID        *int   `json:"subjectUserId"`
	CredentialsIncluded  bool   `json:"credentialsIncluded"`
	PersonalDataExcluded bool   `json:"personalDataExcluded"`
}
type RequestRef struct {
	Kind              string `json:"kind"`
	Value             string `json:"value"`
	Status            string `json:"status"`
	MatchedRequestIDs []int  `json:"matchedRequestIds,omitempty"`
}
type Counts struct {
	Requests     int `json:"requests"`
	Executions   int `json:"executions"`
	Usage        int `json:"usage"`
	Traces       int `json:"traces"`
	Threads      int `json:"threads"`
	Channels     int `json:"channels"`
	APIKeys      int `json:"apiKeys"`
	AccessGroups int `json:"accessGroups"`
}
type Selection struct {
	Selector    Selector     `json:"selector"`
	AsOf        string       `json:"asOf"`
	Order       string       `json:"order"`
	RequestRefs []RequestRef `json:"requestRefs"`
	Counts      Counts       `json:"counts"`
	HasMore     bool         `json:"hasMore"`
	NextCursor  *string      `json:"nextCursor"`
}
type PullResponse struct {
	Contract      ContractResponse `json:"contract"`
	Bundle        Bundle           `json:"bundle"`
	Server        ServerInfo       `json:"server"`
	Authorization Authorization    `json:"authorization"`
	Selection     Selection        `json:"selection"`
	Sections      Sections         `json:"sections"`
	Issues        []Issue          `json:"issues"`
}

type ErrorDetail struct {
	Code          string           `json:"code"`
	Message       string           `json:"message"`
	CorrelationID string           `json:"correlationId"`
	Retryable     bool             `json:"retryable"`
	Supported     []SupportedRange `json:"supported,omitempty"`
}
type SupportedRange struct {
	Major    int `json:"major"`
	MinMinor int `json:"minMinor"`
	MaxMinor int `json:"maxMinor"`
}
type ErrorResponse struct {
	Contract ContractResponse `json:"contract"`
	Error    ErrorDetail      `json:"error"`
}

type Evidence struct {
	State                  string `json:"state"`
	Source                 string `json:"source"`
	MediaType              string `json:"mediaType"`
	Encoding               string `json:"encoding,omitempty"`
	Canonicalization       string `json:"canonicalization,omitempty"`
	CanonicalizationStatus string `json:"canonicalizationStatus,omitempty"`
	CanonicalizationReason string `json:"canonicalizationReason,omitempty"`
	ByteLength             int    `json:"byteLength,omitempty"`
	SHA256                 string `json:"sha256,omitempty"`
	CanonicalSHA256        string `json:"canonicalSha256,omitempty"`
	RawSHA256              string `json:"rawSha256,omitempty"`
	Value                  any    `json:"value,omitempty"`
	Reason                 string `json:"reason,omitempty"`
}
type Credential struct {
	Status string `json:"status"`
	Value  any    `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func defaultLimits(l Limits) Limits {
	if l.MaxRequests == 0 {
		l.MaxRequests = ContractDefaultMaxRequests
	}
	if l.MaxExecutions == 0 {
		l.MaxExecutions = ContractDefaultMaxExecutions
	}
	if l.MaxRelatedRecords == 0 {
		l.MaxRelatedRecords = ContractDefaultMaxRelatedRecords
	}
	if l.MaxResponseBytes == 0 {
		l.MaxResponseBytes = ContractDefaultMaxResponseBytes
	}
	return l
}

func decodeIDs[T any](raw json.RawMessage) ([]T, error) {
	var ids []T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("selector ids are required")
	}
	if err := dec.Decode(&ids); err != nil {
		return nil, fmt.Errorf("invalid selector ids: %w", err)
	}
	return ids, nil
}

func decodeRequestIDs(raw json.RawMessage) ([]int, error) {
	ids64, err := decodeIDs[int64](raw)
	if err != nil {
		return nil, err
	}
	ids := make([]int, len(ids64))
	for index, id := range ids64 {
		ids[index] = int(id)
		if int64(ids[index]) != id {
			return nil, fmt.Errorf("request id %d is outside the platform integer range", id)
		}
	}
	return ids, nil
}
