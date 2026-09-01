package diagnostics

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/fx"
	"golang.org/x/crypto/hkdf"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/build"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/apikeyprofiletemplate"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/request"
	requestexecution "github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/thread"
	"github.com/looplj/axonhub/internal/ent/trace"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

type Params struct {
	fx.In
	Ent      *ent.Client
	Requests *biz.RequestService
	Systems  *biz.SystemService
	Storage  *biz.DataStorageService
}
type Service struct {
	ent      *ent.Client
	requests *biz.RequestService
	systems  *biz.SystemService
	storage  *biz.DataStorageService
}

func NewService(p Params) *Service {
	return &Service{ent: p.Ent, requests: p.Requests, systems: p.Systems, storage: p.Storage}
}

type ServiceError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
	Supported bool
}

type cursorPayload struct {
	BundleID      string    `json:"bundleId"`
	PageIndex     int       `json:"pageIndex"`
	Binding       string    `json:"binding"`
	AsOf          time.Time `json:"asOf"`
	LastCreatedAt time.Time `json:"lastCreatedAt"`
	LastID        int       `json:"lastId"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

func (e *ServiceError) Error() string { return e.Code + ": " + e.Message }
func serviceError(status int, code, message string) error {
	return &ServiceError{Status: status, Code: code, Message: message}
}

var canonicalSections = ContractSectionNames
var sectionScopes = map[string][]scopes.ScopeSlug{
	"requests": {scopes.ScopeReadRequests}, "executions": {scopes.ScopeReadRequests}, "usage": {scopes.ScopeReadRequests}, "traces": {scopes.ScopeReadRequests}, "threads": {scopes.ScopeReadRequests},
	"channels": {scopes.ScopeReadChannels}, "apiKeys": {scopes.ScopeReadAPIKeys}, "accessGroups": {scopes.ScopeReadAPIKeys},
	"configuration": {scopes.ScopeReadSettings, scopes.ScopeReadDataStorages},
}

var requestMetadataFields = []string{
	request.FieldID, request.FieldCreatedAt, request.FieldUpdatedAt,
	request.FieldAPIKeyID, request.FieldProjectID, request.FieldTraceID, request.FieldDataStorageID,
	request.FieldSource, request.FieldModelID, request.FieldReasoningEffort, request.FieldFormat,
	request.FieldChannelID, request.FieldExternalID, request.FieldStatus, request.FieldStream, request.FieldClientIP,
	request.FieldMetricsLatencyMs, request.FieldMetricsFirstTokenLatencyMs, request.FieldMetricsReasoningDurationMs,
	request.FieldSelectedChannelAPIKeyMasked, request.FieldContentSaved, request.FieldContentStorageID,
	request.FieldContentStorageKey, request.FieldContentSavedAt, request.FieldRoutingContext, request.FieldEvidenceDisposition,
	request.FieldRequestBodyPayloadID,
}

var requestExecutionMetadataFields = []string{
	requestexecution.FieldID, requestexecution.FieldCreatedAt, requestexecution.FieldUpdatedAt,
	requestexecution.FieldProjectID, requestexecution.FieldRequestID, requestexecution.FieldChannelID,
	requestexecution.FieldDataStorageID, requestexecution.FieldExternalID, requestexecution.FieldModelID,
	requestexecution.FieldFormat, requestexecution.FieldErrorMessage, requestexecution.FieldResponseStatusCode,
	requestexecution.FieldStatus, requestexecution.FieldStream, requestexecution.FieldMetricsLatencyMs,
	requestexecution.FieldMetricsFirstTokenLatencyMs, requestexecution.FieldMetricsReasoningDurationMs,
	requestexecution.FieldSelectedChannelAPIKeyMasked, requestexecution.FieldRequestURL,
	requestexecution.FieldPassThroughApplied, requestexecution.FieldEvidenceDisposition,
	requestexecution.FieldRequestBodyPayloadID,
}

func (s *Service) Pull(ctx context.Context, req PullRequest) (*PullResponse, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	projectID, ok := contexts.GetProjectID(ctx)
	if !ok || projectID != req.Scope.ProjectID {
		return nil, serviceError(403, "PROJECT_FORBIDDEN", "requested project does not match the authenticated project")
	}
	if !authz.HasScope(ctx, scopes.ScopeReadDiagnostics) {
		return nil, serviceError(403, "SCOPE_FORBIDDEN", "read_diagnostics is required")
	}
	for _, section := range req.Include.Sections {
		for _, scope := range sectionScopes[section] {
			if !authz.HasScope(ctx, scope) {
				return nil, serviceError(403, "SCOPE_FORBIDDEN", "requested section requires additional scope")
			}
		}
	}

	principal, ok := authz.GetPrincipal(ctx)
	if !ok {
		return nil, serviceError(401, "UNAUTHENTICATED", "authentication is required")
	}
	user, hasUser := contexts.GetUser(ctx)
	key, hasKey := contexts.GetAPIKey(ctx)
	principalType, principalID := "serviceAccount", 0
	if principal.Type == authz.PrincipalTypeUser && hasUser {
		principalType, principalID = "user", user.ID
	} else if principal.Type == authz.PrincipalTypeAPIKey && hasKey {
		principalID = key.ID
	} else {
		return nil, serviceError(401, "UNAUTHENTICATED", "unsupported principal")
	}
	if req.Include.Credentials && (principalType != "user" || !user.IsOwner) {
		return nil, serviceError(403, "CREDENTIAL_EXPORT_FORBIDDEN", "credential export requires a system-owner user")
	}
	if req.Scope.SubjectUserID != nil {
		if principalType == "serviceAccount" || *req.Scope.SubjectUserID != principalID {
			if !authz.HasScope(ctx, scopes.ScopeReadUsers) {
				return nil, serviceError(403, "SCOPE_FORBIDDEN", "selecting another subject requires read_users")
			}
		}
	}

	release, err := acquirePull(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, serverPullTimeout)
	defer cancel()

	limits := effectiveLimits(req.Limits)
	materialized := newResponseBudget(limits.MaxResponseBytes)
	allowExternalEvidence := req.Selector.Kind == "requestIds"
	if req.Selector.Kind == "requestIds" {
		ids, _ := decodeRequestIDs(req.Selector.IDs)
		if len(ids) > limits.MaxRequests {
			return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "requestIds exceeds the server diagnostics work budget")
		}
	}
	asOf := time.Now().UTC()
	bundleID := uuid.NewString()
	pageIndex := 1
	var cursor *cursorPayload
	if req.Page != nil && req.Page.Cursor != nil {
		var cursorErr error
		cursor, cursorErr = s.openCursor(ctx, *req.Page.Cursor, req)
		if cursorErr != nil {
			return nil, cursorErr
		}
		asOf, bundleID, pageIndex = cursor.AsOf, cursor.BundleID, cursor.PageIndex
	}
	bypassCtx := ent.NewContext(authz.WithSystemBypass(ctx, "diagnostics-pull"), s.ent)
	selected, refs, hasMore, err := s.selectRequests(bypassCtx, req, limits, asOf, principalType, principalID, cursor)
	if err != nil {
		return nil, err
	}

	requested := make(map[string]bool, len(req.Include.Sections))
	for _, section := range req.Include.Sections {
		requested[section] = true
	}
	sections := emptySections(requested)
	issues := []Issue{}
	requestRecords := []any{}
	executionRecords := []any{}
	usageRecords := []any{}
	traceRecords := []any{}
	threadRecords := []any{}
	channelIDs, keyIDs, groupIDs := map[int]struct{}{}, map[int]struct{}{}, map[int]struct{}{}
	storageIDs := map[int]struct{}{}
	referencedGroupRevisions := map[int]map[int64]struct{}{}
	traceIDs, threadIDs := map[int]struct{}{}, map[int]struct{}{}

	// Selection also drives metadata-only pulls. A caller may request only
	// channels/keys/groups/configuration for a request selector and still expects
	// metadata referenced by those selected requests and executions.
	if requested["requests"] || requested["executions"] || requested["usage"] || requested["traces"] || requested["threads"] || requested["channels"] || requested["apiKeys"] || requested["accessGroups"] || requested["configuration"] {
		for _, item := range selected {
			if err := checkPullContext(bypassCtx); err != nil {
				return nil, err
			}
			if item.DataStorageID != 0 {
				storageIDs[item.DataStorageID] = struct{}{}
			}
			if item.APIKeyID != 0 {
				keyIDs[item.APIKeyID] = struct{}{}
			}
			if item.ChannelID != 0 {
				channelIDs[item.ChannelID] = struct{}{}
			}
			if item.TraceID != 0 {
				traceIDs[item.TraceID] = struct{}{}
			}
			if item.RoutingContext != nil && item.RoutingContext.AccessGroupID != nil && item.RoutingContext.AccessGroupRevision != nil {
				groupID, revision := *item.RoutingContext.AccessGroupID, *item.RoutingContext.AccessGroupRevision
				groupIDs[groupID] = struct{}{}
				if referencedGroupRevisions[groupID] == nil {
					referencedGroupRevisions[groupID] = map[int64]struct{}{}
				}
				referencedGroupRevisions[groupID][revision] = struct{}{}
			}
			if requested["requests"] {
				if hydrateErr := s.hydrateRequestEvidence(bypassCtx, item); hydrateErr != nil {
					return nil, hydrateErr
				}
				record, itemIssues, recordErr := s.requestRecord(bypassCtx, item, allowExternalEvidence)
				item.RequestHeaders = nil
				item.RequestBody = nil
				item.ResponseBody = nil
				item.ResponseChunks = nil
				if recordErr != nil {
					return nil, recordErr
				}
				if err := materialized.add(bypassCtx, record); err != nil {
					return nil, err
				}
				requestRecords = append(requestRecords, record)
				issues = append(issues, itemIssues...)
			}
		}
		ids := requestIDs(selected)
		if len(ids) > 0 {
			if requested["executions"] || requested["channels"] || requested["configuration"] {
				execQuery := s.ent.RequestExecution.Query().Where(requestexecution.RequestIDIn(ids...), requestexecution.CreatedAtLTE(asOf)).Order(ent.Asc(requestexecution.FieldCreatedAt), ent.Asc(requestexecution.FieldID)).Limit(limits.MaxExecutions + 1)
				execQuery.Select(requestExecutionMetadataFields...)
				execs, qerr := execQuery.All(bypassCtx)
				if qerr != nil {
					return nil, queryError(bypassCtx, qerr)
				}
				if len(execs) > limits.MaxExecutions {
					return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "execution selection exceeds the server diagnostics work budget")
				}
				for _, exec := range execs {
					if err := checkPullContext(bypassCtx); err != nil {
						return nil, err
					}
					if exec.DataStorageID != 0 {
						storageIDs[exec.DataStorageID] = struct{}{}
					}
					if exec.ChannelID != 0 {
						channelIDs[exec.ChannelID] = struct{}{}
					}
					if requested["executions"] {
						if hydrateErr := s.hydrateExecutionEvidence(bypassCtx, exec); hydrateErr != nil {
							return nil, hydrateErr
						}
						record, itemIssues, recordErr := s.executionRecord(bypassCtx, exec, allowExternalEvidence)
						exec.RequestHeaders = nil
						exec.RequestBody = nil
						exec.ResponseBody = nil
						exec.ResponseChunks = nil
						if recordErr != nil {
							return nil, recordErr
						}
						if err := materialized.add(bypassCtx, record); err != nil {
							return nil, err
						}
						executionRecords = append(executionRecords, record)
						issues = append(issues, itemIssues...)
					}
				}
			}
			if requested["usage"] {
				rows, qerr := s.ent.UsageLog.Query().Where(usagelog.RequestIDIn(ids...), usagelog.CreatedAtLTE(asOf)).Order(ent.Asc(usagelog.FieldCreatedAt), ent.Asc(usagelog.FieldID)).Limit(limits.MaxRelatedRecords + 1).All(bypassCtx)
				if qerr != nil {
					return nil, queryError(bypassCtx, qerr)
				}
				if len(rows) > limits.MaxRelatedRecords {
					return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "related selection exceeds the server diagnostics work budget")
				}
				for _, row := range rows {
					if err := checkPullContext(bypassCtx); err != nil {
						return nil, err
					}
					if row.ChannelID != 0 {
						channelIDs[row.ChannelID] = struct{}{}
					}
					record := usageRecord(row)
					if err := materialized.add(bypassCtx, record); err != nil {
						return nil, err
					}
					usageRecords = append(usageRecords, record)
				}
			}
		}
		if (requested["traces"] || requested["threads"]) && len(traceIDs) > 0 {
			rows, qerr := s.ent.Trace.Query().Where(trace.IDIn(mapKeys(traceIDs)...), trace.CreatedAtLTE(asOf)).Order(ent.Asc(trace.FieldCreatedAt), ent.Asc(trace.FieldID)).All(bypassCtx)
			if qerr != nil {
				return nil, queryError(bypassCtx, qerr)
			}
			for _, row := range rows {
				if err := checkPullContext(bypassCtx); err != nil {
					return nil, err
				}
				if row.ThreadID != 0 {
					threadIDs[row.ThreadID] = struct{}{}
				}
				if requested["traces"] {
					record := traceRecord(row)
					if err := materialized.add(bypassCtx, record); err != nil {
						return nil, err
					}
					traceRecords = append(traceRecords, record)
				}
			}
		}
		if requested["threads"] && len(threadIDs) > 0 {
			rows, qerr := s.ent.Thread.Query().Where(thread.IDIn(mapKeys(threadIDs)...), thread.CreatedAtLTE(asOf)).Order(ent.Asc(thread.FieldCreatedAt), ent.Asc(thread.FieldID)).All(bypassCtx)
			if qerr != nil {
				return nil, queryError(bypassCtx, qerr)
			}
			for _, row := range rows {
				if err := checkPullContext(bypassCtx); err != nil {
					return nil, err
				}
				record := threadRecord(row)
				if err := materialized.add(bypassCtx, record); err != nil {
					return nil, err
				}
				threadRecords = append(threadRecords, record)
			}
		}
	}
	if requested["requests"] {
		sections.Requests = sectionWithStatus(requestRecords, issuesFor(issues, "requests"))
	}
	if requested["executions"] {
		sections.Executions = sectionWithStatus(executionRecords, issuesFor(issues, "executions"))
	}
	if requested["usage"] {
		sections.Usage = sectionWithStatus(usageRecords, nil)
	}
	if requested["traces"] {
		sections.Traces = sectionWithStatus(traceRecords, nil)
	}
	if requested["threads"] {
		sections.Threads = sectionWithStatus(threadRecords, nil)
	}
	if requested["health"] {
		if err := checkPullContext(bypassCtx); err != nil {
			return nil, err
		}
		sections.Health = s.healthSection(bypassCtx)
		if err := checkPullContext(bypassCtx); err != nil {
			return nil, err
		}
		if err := materialized.add(bypassCtx, sections.Health.Data); err != nil {
			return nil, err
		}
	}

	channelRecords := []any{}
	keyRecords := []any{}
	groupRecords := []any{}
	if requested["channels"] {
		rows, qerr := s.loadChannels(bypassCtx, req.Selector.Kind, projectID, channelIDs, principalType, principalID)
		if qerr != nil {
			return nil, queryError(bypassCtx, qerr)
		}
		for _, row := range rows {
			record := channelRecord(row, req.Include.Credentials)
			if err := materialized.add(bypassCtx, record); err != nil {
				return nil, err
			}
			channelRecords = append(channelRecords, record)
		}
		sections.Channels = sectionWithStatus(channelRecords, nil)
	}
	if requested["apiKeys"] {
		rows, qerr := s.loadAPIKeys(bypassCtx, req.Selector.Kind, projectID, keyIDs, principalType, principalID)
		if qerr != nil {
			return nil, queryError(bypassCtx, qerr)
		}
		for _, row := range rows {
			record := apiKeyRecord(row, req.Include.Credentials)
			if err := materialized.add(bypassCtx, record); err != nil {
				return nil, err
			}
			keyRecords = append(keyRecords, record)
		}
		sections.APIKeys = sectionWithStatus(keyRecords, nil)
	}
	if requested["accessGroups"] {
		rows, qerr := s.loadGroups(bypassCtx, req.Selector.Kind, projectID, groupIDs)
		if qerr != nil {
			return nil, queryError(bypassCtx, qerr)
		}
		for _, row := range rows {
			record, qerr := s.accessGroupRecord(bypassCtx, row, referencedGroupRevisions[row.ID])
			if qerr != nil {
				return nil, queryError(bypassCtx, qerr)
			}
			if err := materialized.add(bypassCtx, record); err != nil {
				return nil, err
			}
			groupRecords = append(groupRecords, record)
		}
		sections.AccessGroups = sectionWithStatus(groupRecords, nil)
	}
	emittedStorageCount := 0
	if requested["configuration"] {
		configuration, qerr := s.configurationRecord(bypassCtx, req.Include.Credentials, storageIDs)
		if qerr != nil {
			return nil, queryError(bypassCtx, qerr)
		}
		if err := materialized.add(bypassCtx, configuration); err != nil {
			return nil, err
		}
		sections.Configuration = Section{Status: "available", Data: configuration, Issues: []Issue{}}
		emittedStorageCount = len(storageIDs)
	}
	if len(usageRecords)+len(traceRecords)+len(threadRecords)+len(channelRecords)+len(keyRecords)+len(groupRecords)+emittedStorageCount > limits.MaxRelatedRecords {
		return nil, serviceError(413, "RELATED_LIMIT_EXCEEDED", "related record limit exceeded")
	}

	partial := len(issues) > 0
	for _, section := range []Section{sections.Health, sections.Configuration, sections.Requests, sections.Executions, sections.Usage, sections.Traces, sections.Threads, sections.Channels, sections.APIKeys, sections.AccessGroups} {
		if section.Status == "partial" || section.Status == "unavailable" {
			partial = true
		}
	}
	for _, ref := range refs {
		if ref.Status != "matched" {
			partial = true
		}
	}
	status := "complete"
	if partial {
		status = "partial"
	}
	generated := asOf.Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var nextCursor *string
	if hasMore && len(selected) > 0 {
		boundary := selected[0]
		expiresAt := time.Now().UTC().Add(time.Hour)
		if cursor != nil {
			expiresAt = cursor.ExpiresAt
		}
		token, cursorErr := s.sealCursor(ctx, cursorPayload{BundleID: bundleID, PageIndex: pageIndex + 1, Binding: requestBinding(req), AsOf: asOf, LastCreatedAt: boundary.CreatedAt, LastID: boundary.ID, ExpiresAt: expiresAt})
		if cursorErr != nil {
			return nil, coreDBError()
		}
		nextCursor = &token
	}
	response := &PullResponse{Contract: ContractResponse{Name: ContractName, Major: ContractMajor, Minor: ContractMinor, SchemaSHA256: SchemaSHA256}, Bundle: Bundle{ID: bundleID, GeneratedAt: generated, PageIndex: pageIndex, PageGeneratedAt: now, Status: status}, Server: ServerInfo{Version: build.Version, Commit: build.Commit, BuildTime: nullableBuildTime(), UptimeSeconds: int64(time.Since(build.StartTime).Seconds())}, Authorization: Authorization{PrincipalType: principalType, PrincipalID: principalID, ProjectID: projectID, SubjectUserID: req.Scope.SubjectUserID, CredentialsIncluded: req.Include.Credentials, PersonalDataExcluded: principalType == "serviceAccount" || req.Scope.SubjectUserID == nil || (req.Scope.SubjectUserID != nil && *req.Scope.SubjectUserID != principalID)}, Selection: Selection{Selector: req.Selector, AsOf: generated, Order: "createdAtAsc,idAsc", RequestRefs: refs, Counts: Counts{Requests: len(requestRecords), Executions: len(executionRecords), Usage: len(usageRecords), Traces: len(traceRecords), Threads: len(threadRecords), Channels: len(channelRecords), APIKeys: len(keyRecords), AccessGroups: len(groupRecords)}, HasMore: hasMore, NextCursor: nextCursor}, Sections: sections, Issues: issues}
	if err := checkPullContext(bypassCtx); err != nil {
		return nil, err
	}
	encoded, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return nil, serviceError(500, "SERIALIZATION_FAILED", "diagnostics response could not be serialized")
	}
	if err := checkPullContext(bypassCtx); err != nil {
		return nil, err
	}
	if len(encoded) > limits.MaxResponseBytes {
		return nil, serviceError(413, "RESPONSE_TOO_LARGE", "use the next cursor or request fewer sections")
	}
	return response, nil
}

func (s *Service) healthSection(ctx context.Context) Section {
	data := map[string]any{
		"processStatus":     "available",
		"databaseStatus":    "unknown",
		"databaseLatencyMs": nil,
		"uptimeSeconds":     int64(time.Since(build.StartTime).Seconds()),
		"version":           build.Version,
		"commit":            build.Commit,
		"buildTime":         nullableBuildTime(),
	}
	if s.ent == nil {
		issue := Issue{Code: "DATABASE_HEALTH_UNKNOWN", Section: "health", Retryable: true, Message: "database health could not be observed"}
		return Section{Status: "partial", Data: data, Issues: []Issue{issue}}
	}
	started := time.Now()
	_, err := s.ent.Project.Query().Limit(1).IDs(ctx)
	latency := time.Since(started).Milliseconds()
	data["databaseLatencyMs"] = latency
	if err != nil {
		data["databaseStatus"] = "unavailable"
		issue := Issue{Code: "DATABASE_HEALTH_UNAVAILABLE", Section: "health", Retryable: true, Message: "database health observation failed"}
		return Section{Status: "partial", Data: data, Issues: []Issue{issue}}
	}
	data["databaseStatus"] = "available"
	managed := map[string]any{
		"status":        "available",
		"chargedBytes":  int64(0),
		"underPressure": false,
	}
	state, stateErr := s.ent.ManagedObservabilityState.Get(ctx, 1)
	if stateErr == nil {
		managed["chargedBytes"] = state.ChargedBytes
		managed["underPressure"] = state.UnderPressure
		managed["lastError"] = state.LastError
	} else if !ent.IsNotFound(stateErr) {
		managed["status"] = "unknown"
	}
	data["managedObservability"] = managed
	return Section{Status: "available", Data: data, Issues: []Issue{}}
}

func validateRequest(req PullRequest) error {
	if req.Contract.MinMinor < ContractMinorMinimum || req.Contract.MaxMinor < ContractMinorMinimum {
		return serviceError(400, "VALIDATION_FAILED", "contract minor ranges must be non-negative")
	}
	if req.Contract.MinMinor > req.Contract.MaxMinor {
		return serviceError(400, "VALIDATION_FAILED", "minMinor must not exceed maxMinor")
	}
	if req.Scope.SubjectUserID != nil && *req.Scope.SubjectUserID < ContractSubjectUserIDMinimum {
		return serviceError(400, "VALIDATION_FAILED", "scope.subjectUserId must be positive")
	}
	if req.Contract.Name != ContractName || req.Contract.Major != ContractMajor || req.Contract.MinMinor > ContractMinor || req.Contract.MaxMinor < ContractMinor {
		e := &ServiceError{Status: 426, Code: "VERSION_NOT_SUPPORTED", Message: "requested diagnostics contract is not supported", Supported: true}
		return e
	}
	if req.Scope.ProjectID <= 0 {
		return serviceError(400, "VALIDATION_FAILED", "scope.projectId must be positive")
	}
	seen := map[string]bool{}
	for _, section := range req.Include.Sections {
		if !contains(canonicalSections, section) || seen[section] {
			return serviceError(400, "VALIDATION_FAILED", "sections must be known and unique")
		}
		seen[section] = true
	}
	if len(seen) == 0 {
		return serviceError(400, "VALIDATION_FAILED", "at least one section is required")
	}
	l := defaultLimits(req.Limits)
	if l.MaxRequests < 1 || l.MaxRequests > ContractMaximumMaxRequests || l.MaxExecutions < 1 || l.MaxExecutions > ContractMaximumMaxExecutions || l.MaxRelatedRecords < 1 || l.MaxRelatedRecords > ContractMaximumMaxRelatedRecords || l.MaxResponseBytes < ContractMinimumMaxResponseBytes || l.MaxResponseBytes > ContractMaximumMaxResponseBytes {
		return serviceError(400, "VALIDATION_FAILED", "limits are outside the supported bounds")
	}
	hasIDs := len(req.Selector.IDs) > 0
	hasTimeFields := req.Selector.From != "" || req.Selector.To != "" || len(req.Selector.Statuses) > 0 || len(req.Selector.ModelIDs) > 0 || len(req.Selector.APIKeyIDs) > 0
	if req.Page != nil && req.Page.Cursor != nil && contains([]string{"requestIds", "snapshot"}, req.Selector.Kind) {
		return serviceError(400, "VALIDATION_FAILED", "this selector does not support pagination cursors")
	}
	switch req.Selector.Kind {
	case "requestIds":
		ids, e := decodeRequestIDs(req.Selector.IDs)
		if hasTimeFields || e != nil || len(ids) < 1 || len(ids) > 100 || !uniqueComparable(ids) || !allPositive(ids) {
			return serviceError(400, "VALIDATION_FAILED", "requestIds selector is invalid")
		}
		if len(ids) > l.MaxRequests {
			return serviceError(413, "SELECTOR_LIMIT_EXCEEDED", "requestIds exceeds maxRequests")
		}
	case "externalIds", "traceIds", "threadIds":
		ids, e := decodeIDs[string](req.Selector.IDs)
		max := 100
		if req.Selector.Kind != "externalIds" {
			max = 50
		}
		if hasTimeFields || e != nil || len(ids) < 1 || len(ids) > max || !validStrings(ids) {
			return serviceError(400, "VALIDATION_FAILED", "string selector is invalid")
		}
	case "timeRange":
		if hasIDs {
			return serviceError(400, "VALIDATION_FAILED", "timeRange does not accept ids")
		}
		from, e1 := time.Parse(time.RFC3339Nano, req.Selector.From)
		to, e2 := time.Parse(time.RFC3339Nano, req.Selector.To)
		if e1 != nil || e2 != nil || !from.Before(to) || to.Sub(from) > 7*24*time.Hour || from.Location() != time.UTC || to.Location() != time.UTC {
			return serviceError(400, "VALIDATION_FAILED", "timeRange must be UTC, increasing, and at most seven days")
		}
		if len(req.Selector.Statuses) > 100 || !uniqueComparable(req.Selector.Statuses) || !allRequestStatuses(req.Selector.Statuses) || len(req.Selector.ModelIDs) > 100 || !validStrings(req.Selector.ModelIDs) || len(req.Selector.APIKeyIDs) > 100 || !uniqueComparable(req.Selector.APIKeyIDs) || !allPositive(req.Selector.APIKeyIDs) {
			return serviceError(400, "VALIDATION_FAILED", "timeRange filters are invalid")
		}
	case "snapshot":
		if hasIDs || hasTimeFields {
			return serviceError(400, "VALIDATION_FAILED", "snapshot does not accept selector filters")
		}
		for section := range seen {
			if !contains([]string{"health", "configuration", "channels", "apiKeys", "accessGroups"}, section) {
				return serviceError(400, "VALIDATION_FAILED", "snapshot selector does not support request evidence sections")
			}
		}
	default:
		return serviceError(400, "VALIDATION_FAILED", "unknown selector kind")
	}
	return nil
}

func (s *Service) selectRequests(ctx context.Context, req PullRequest, limits Limits, asOf time.Time, principalType string, principalID int, cursor *cursorPayload) ([]*ent.Request, []RequestRef, bool, error) {
	if req.Selector.Kind == "snapshot" {
		return []*ent.Request{}, []RequestRef{}, false, nil
	}
	q := s.ent.Request.Query().Where(request.ProjectIDEQ(req.Scope.ProjectID), request.CreatedAtLTE(asOf))
	if cursor != nil {
		q = q.Where(request.Or(request.CreatedAtLT(cursor.LastCreatedAt), request.And(request.CreatedAtEQ(cursor.LastCreatedAt), request.IDLT(cursor.LastID))))
	}
	privacy := request.Or(
		request.APIKeyIDIsNil(),
		request.HasAPIKeyWith(apikey.TypeNEQ(apikey.TypePersonal), apikey.TypeNEQ(apikey.TypeUser)),
	)
	if principalType == "user" {
		privacy = request.Or(
			privacy,
			request.HasAPIKeyWith(apikey.TypeEQ(apikey.TypeUser)),
			request.HasAPIKeyWith(apikey.TypeEQ(apikey.TypePersonal), apikey.UserIDEQ(principalID)),
		)
	}
	q = q.Where(privacy)
	if req.Scope.SubjectUserID != nil {
		q = q.Where(request.HasAPIKeyWith(apikey.UserIDEQ(*req.Scope.SubjectUserID)))
	}
	refs := []RequestRef{}
	switch req.Selector.Kind {
	case "requestIds":
		ids, _ := decodeRequestIDs(req.Selector.IDs)
		q = q.Where(request.IDIn(ids...))
		q.Select(requestMetadataFields...)
		rows, err := q.All(ctx)
		if err != nil {
			return nil, nil, false, queryError(ctx, err)
		}
		byID := map[int]*ent.Request{}
		for _, row := range rows {
			byID[row.ID] = row
		}
		for _, id := range ids {
			ref := RequestRef{Kind: "requestId", Value: strconv.Itoa(id), Status: "notFoundOrNotAuthorized"}
			if row := byID[id]; row != nil {
				ref.Status = "matched"
				ref.MatchedRequestIDs = []int{id}
			}
			refs = append(refs, ref)
		}
		sortRequests(rows)
		return rows, refs, false, nil
	case "externalIds":
		ids, _ := decodeIDs[string](req.Selector.IDs)
		q = q.Where(request.ExternalIDIn(ids...))
		refs = makeRefs("externalId", ids)
	case "traceIds":
		ids, _ := decodeIDs[string](req.Selector.IDs)
		q = q.Where(request.HasTraceWith(trace.TraceIDIn(ids...)))
		refs = makeRefs("traceId", ids)
	case "threadIds":
		ids, _ := decodeIDs[string](req.Selector.IDs)
		q = q.Where(request.HasTraceWith(trace.HasThreadWith(thread.ThreadIDIn(ids...))))
		refs = makeRefs("threadId", ids)
	case "timeRange":
		from, _ := time.Parse(time.RFC3339Nano, req.Selector.From)
		to, _ := time.Parse(time.RFC3339Nano, req.Selector.To)
		q = q.Where(request.CreatedAtGTE(from), request.CreatedAtLT(to))
		if len(req.Selector.Statuses) > 0 {
			statuses := make([]request.Status, len(req.Selector.Statuses))
			for i, v := range req.Selector.Statuses {
				statuses[i] = request.Status(v)
			}
			q = q.Where(request.StatusIn(statuses...))
		}
		if len(req.Selector.ModelIDs) > 0 {
			q = q.Where(request.ModelIDIn(req.Selector.ModelIDs...))
		}
		if len(req.Selector.APIKeyIDs) > 0 {
			q = q.Where(request.APIKeyIDIn(req.Selector.APIKeyIDs...))
		}
	}
	q.Select(requestMetadataFields...)
	rows, err := q.Order(ent.Desc(request.FieldCreatedAt), ent.Desc(request.FieldID)).Limit(limits.MaxRequests + 1).All(ctx)
	if err != nil {
		return nil, nil, false, queryError(ctx, err)
	}
	hasMore := len(rows) > limits.MaxRequests
	if hasMore {
		rows = rows[:limits.MaxRequests]
	}
	sortRequests(rows)
	if len(refs) > 0 {
		for i := range refs {
			matched := []int{}
			for _, row := range rows {
				if err := checkPullContext(ctx); err != nil {
					return nil, nil, false, err
				}
				switch refs[i].Kind {
				case "externalId":
					if row.ExternalID == refs[i].Value {
						matched = append(matched, row.ID)
					}
				case "traceId":
					tr, _ := row.QueryTrace().Only(ctx)
					if tr != nil && tr.TraceID == refs[i].Value {
						matched = append(matched, row.ID)
					}
				case "threadId":
					tr, _ := row.QueryTrace().WithThread().Only(ctx)
					if tr != nil && tr.Edges.Thread != nil && tr.Edges.Thread.ThreadID == refs[i].Value {
						matched = append(matched, row.ID)
					}
				}
			}
			if len(matched) > 0 {
				sort.Ints(matched)
				refs[i].Status = "matched"
				refs[i].MatchedRequestIDs = matched
			}
		}
	}
	return rows, refs, hasMore, nil
}

func (s *Service) hydrateRequestEvidence(ctx context.Context, row *ent.Request) error {
	type requestEvidenceSizes struct {
		RequestHeaders int `json:"request_headers_bytes"`
		RequestBody    int `json:"request_body_bytes"`
		ResponseBody   int `json:"response_body_bytes"`
		ResponseChunks int `json:"response_chunks_bytes"`
	}
	var results []requestEvidenceSizes
	dialectName := s.ent.Driver().Dialect()
	err := s.ent.Request.Query().Where(request.IDEQ(row.ID)).Modify(func(selector *sql.Selector) {
		selector.Select(
			sql.As(evidenceLengthExpression(dialectName, selector.C(request.FieldRequestHeaders)), "request_headers_bytes"),
			sql.As(evidenceLengthExpression(dialectName, selector.C(request.FieldRequestBody)), "request_body_bytes"),
			sql.As(evidenceLengthExpression(dialectName, selector.C(request.FieldResponseBody)), "response_body_bytes"),
			sql.As(evidenceLengthExpression(dialectName, selector.C(request.FieldResponseChunks)), "response_chunks_bytes"),
		)
	}).Scan(ctx, &results)
	if err != nil {
		return queryError(ctx, err)
	}
	if len(results) != 1 {
		return coreDBError()
	}
	sizes := results[0]
	for _, size := range []int{sizes.RequestHeaders, sizes.RequestBody, sizes.ResponseBody, sizes.ResponseChunks} {
		if size > serverMaxEvidenceBytes {
			return serviceError(413, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
		}
	}
	q := s.ent.Request.Query().Where(request.IDEQ(row.ID))
	q.Select(request.FieldRequestHeaders, request.FieldRequestBody, request.FieldResponseBody, request.FieldResponseChunks, request.FieldRequestBodyPayloadID)
	evidence, err := q.Only(ctx)
	if err != nil {
		return queryError(ctx, err)
	}
	row.RequestHeaders = evidence.RequestHeaders
	row.RequestBody = evidence.RequestBody
	row.ResponseBody = evidence.ResponseBody
	row.ResponseChunks = evidence.ResponseChunks
	row.RequestBodyPayloadID = evidence.RequestBodyPayloadID
	return nil
}

func (s *Service) hydrateExecutionEvidence(ctx context.Context, row *ent.RequestExecution) error {
	type executionEvidenceSizes struct {
		RequestHeaders int `json:"request_headers_bytes"`
		RequestBody    int `json:"request_body_bytes"`
		ResponseBody   int `json:"response_body_bytes"`
		ResponseChunks int `json:"response_chunks_bytes"`
	}
	var results []executionEvidenceSizes
	dialectName := s.ent.Driver().Dialect()
	err := s.ent.RequestExecution.Query().Where(requestexecution.IDEQ(row.ID)).Modify(func(selector *sql.Selector) {
		selector.Select(
			sql.As(evidenceLengthExpression(dialectName, selector.C(requestexecution.FieldRequestHeaders)), "request_headers_bytes"),
			sql.As(evidenceLengthExpression(dialectName, selector.C(requestexecution.FieldRequestBody)), "request_body_bytes"),
			sql.As(evidenceLengthExpression(dialectName, selector.C(requestexecution.FieldResponseBody)), "response_body_bytes"),
			sql.As(evidenceLengthExpression(dialectName, selector.C(requestexecution.FieldResponseChunks)), "response_chunks_bytes"),
		)
	}).Scan(ctx, &results)
	if err != nil {
		return queryError(ctx, err)
	}
	if len(results) != 1 {
		return coreDBError()
	}
	sizes := results[0]
	for _, size := range []int{sizes.RequestHeaders, sizes.RequestBody, sizes.ResponseBody, sizes.ResponseChunks} {
		if size > serverMaxEvidenceBytes {
			return serviceError(413, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
		}
	}
	q := s.ent.RequestExecution.Query().Where(requestexecution.IDEQ(row.ID))
	q.Select(requestexecution.FieldRequestHeaders, requestexecution.FieldRequestBody, requestexecution.FieldResponseBody, requestexecution.FieldResponseChunks, requestexecution.FieldRequestBodyPayloadID)
	evidence, err := q.Only(ctx)
	if err != nil {
		return queryError(ctx, err)
	}
	row.RequestHeaders = evidence.RequestHeaders
	row.RequestBody = evidence.RequestBody
	row.ResponseBody = evidence.ResponseBody
	row.ResponseChunks = evidence.ResponseChunks
	row.RequestBodyPayloadID = evidence.RequestBodyPayloadID
	return nil
}

func evidenceLengthExpression(dialectName, column string) string {
	switch dialectName {
	case dialect.Postgres:
		// PostgreSQL JSON columns are jsonb. Cast to text before measuring and
		// use octet_length so the preflight matches the bytes the driver returns.
		return fmt.Sprintf("COALESCE(OCTET_LENGTH(CAST(%s AS TEXT)), 0)", column)
	case dialect.SQLite:
		// SQLite LENGTH(text) counts characters, not bytes. Casting to BLOB keeps
		// the preflight conservative for non-ASCII JSON.
		return fmt.Sprintf("COALESCE(LENGTH(CAST(%s AS BLOB)), 0)", column)
	case dialect.MySQL:
		// MySQL and TiDB expose Ent's mysql dialect. JSON can be rendered as
		// CHAR, and OCTET_LENGTH measures encoded bytes rather than characters.
		return fmt.Sprintf("COALESCE(OCTET_LENGTH(CAST(%s AS CHAR)), 0)", column)
	default:
		// Unknown dialects must fail conservatively instead of emitting a cast
		// syntax that may be invalid or under-count evidence bytes.
		return "NULL"
	}
}

func shouldSkipExternalEvidence(disposition *objects.EvidenceDisposition, field string, allowExternal bool) bool {
	if allowExternal || disposition == nil {
		return false
	}
	var fieldDisposition objects.Disposition
	switch field {
	case "requestBody":
		fieldDisposition = disposition.RequestBody
	case "responseBody":
		fieldDisposition = disposition.ResponseBody
	case "responseChunks":
		fieldDisposition = disposition.ResponseChunks
	default:
		return false
	}
	return fieldDisposition.Location == "external"
}

func skippedExternalEvidence() Evidence {
	return Evidence{
		State:     "storageUnavailable",
		Source:    "external",
		MediaType: "application/json",
		Reason:    "explicit_request_ids_required",
	}
}

func unsupportedBoundedEvidence() Evidence {
	return Evidence{
		State:     "storageUnavailable",
		Source:    "external",
		MediaType: "application/json",
		Reason:    "cancelable_bounded_read_unsupported",
	}
}

func externalSelectionIssue(section, recordType string, recordID int, field string) Issue {
	return Issue{
		Code:       "EXTERNAL_EVIDENCE_REQUIRES_EXPLICIT_REQUEST_IDS",
		Section:    section,
		RecordType: recordType,
		RecordID:   strconv.Itoa(recordID),
		Retryable:  false,
		Message:    field + " was not loaded for a broad diagnostics selector; use explicit requestIds",
	}
}

func unsupportedBoundedReadIssue(section, recordType string, recordID int, field string) Issue {
	return Issue{
		Code:       "CANCELABLE_BOUNDED_READ_UNSUPPORTED",
		Section:    section,
		RecordType: recordType,
		RecordID:   strconv.Itoa(recordID),
		Retryable:  false,
		Message:    field + " uses a storage backend that cannot honor diagnostics cancellation",
	}
}

func loadBudgetedJSONEvidence(
	ctx context.Context,
	disposition *objects.EvidenceDisposition,
	field string,
	allowExternal bool,
	inline objects.JSONRawMessage,
	load func(context.Context) (objects.JSONRawMessage, error),
) (Evidence, bool, error) {
	if shouldSkipExternalEvidence(disposition, field, allowExternal) {
		return skippedExternalEvidence(), true, nil
	}
	if !allowExternal {
		if err := ensureEvidenceBudget(inline); err != nil {
			return Evidence{}, false, err
		}
		return evidenceFromRaw(inline, disposition, nil, field), false, nil
	}
	raw, loadErr := withStorageReadDeadline(ctx, load)
	if err := checkPullContext(ctx); err != nil {
		return Evidence{}, false, err
	}
	if errors.Is(loadErr, biz.ErrBoundedReadUnsupported) {
		return unsupportedBoundedEvidence(), false, nil
	}
	if errors.Is(loadErr, biz.ErrDataTooLarge) {
		return Evidence{}, false, serviceError(413, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
	}
	if err := ensureEvidenceBudget(raw); err != nil {
		return Evidence{}, false, err
	}
	return evidenceFromRaw(raw, disposition, loadErr, field), false, nil
}

func loadBudgetedChunkEvidence(
	ctx context.Context,
	disposition *objects.EvidenceDisposition,
	allowExternal bool,
	source string,
	inline []objects.JSONRawMessage,
	load func(context.Context) ([]objects.JSONRawMessage, error),
) (Evidence, bool, error) {
	if source != "live" && shouldSkipExternalEvidence(disposition, "responseChunks", allowExternal) {
		return skippedExternalEvidence(), true, nil
	}
	chunks := inline
	var loadErr error
	if allowExternal || source == "live" {
		chunks, loadErr = withStorageReadDeadline(ctx, load)
		if err := checkPullContext(ctx); err != nil {
			return Evidence{}, false, err
		}
		if errors.Is(loadErr, biz.ErrBoundedReadUnsupported) {
			return unsupportedBoundedEvidence(), false, nil
		}
		if errors.Is(loadErr, biz.ErrDataTooLarge) {
			return Evidence{}, false, serviceError(413, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
		}
	}
	rawChunks := make([]json.RawMessage, len(chunks))
	for index := range chunks {
		rawChunks[index] = json.RawMessage(chunks[index])
	}
	if err := ensureChunkBudget(rawChunks); err != nil {
		return Evidence{}, false, err
	}
	if source == "live" && chunks == nil {
		chunks = []objects.JSONRawMessage{}
	}
	raw, err := json.Marshal(chunks)
	if err != nil {
		return Evidence{}, false, serviceError(500, "SERIALIZATION_FAILED", "diagnostics chunks could not be serialized")
	}
	return evidenceFromRawWithSource(raw, disposition, loadErr, "responseChunks", source), false, nil
}

func (s *Service) requestRecord(ctx context.Context, row *ent.Request, allowExternalEvidence bool) (map[string]any, []Issue, error) {
	if err := checkPullContext(ctx); err != nil {
		return nil, nil, err
	}
	m := map[string]any{
		"id": row.ID, "projectId": row.ProjectID, "apiKeyId": nullablePositiveInt(row.APIKeyID),
		"traceDatabaseId": nullablePositiveInt(row.TraceID), "dataStorageId": nullablePositiveInt(row.DataStorageID),
		"source": row.Source, "modelId": row.ModelID, "reasoningEffort": row.ReasoningEffort, "format": row.Format,
		"channelId": nullablePositiveInt(row.ChannelID), "externalId": row.ExternalID, "status": row.Status,
		"stream": row.Stream, "clientIp": row.ClientIP, "metricsLatencyMs": row.MetricsLatencyMs,
		"metricsFirstTokenLatencyMs": row.MetricsFirstTokenLatencyMs, "metricsReasoningDurationMs": row.MetricsReasoningDurationMs,
		"createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt,
	}
	issues := []Issue{}
	if err := ensureEvidenceBudget(row.RequestHeaders); err != nil {
		return nil, nil, err
	}
	requestHeaders := evidenceFromRaw(row.RequestHeaders, row.EvidenceDisposition, nil, "requestHeaders")
	m["requestHeaders"] = requestHeaders
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "requestHeaders", requestHeaders)
	requestEvidence, requestSkipped, err := loadBudgetedJSONEvidence(ctx, row.EvidenceDisposition, "requestBody", allowExternalEvidence, row.RequestBody, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadRequestBodyBounded(readCtx, row, serverMaxEvidenceBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	requestEvidence = normalizeManagedRequestBodyEvidence(requestEvidence, row.RequestBodyPayloadID)
	m["requestBody"] = requestEvidence
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "requestBody", requestEvidence)
	if requestEvidence.Reason == "cancelable_bounded_read_unsupported" {
		issues = append(issues, unsupportedBoundedReadIssue("requests", "request", row.ID, "requestBody"))
	} else if requestSkipped {
		issues = append(issues, externalSelectionIssue("requests", "request", row.ID, "requestBody"))
	} else if requestEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "REQUEST_BODY_STORAGE_UNAVAILABLE"))
	} else if requestEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "REQUEST_BODY_LEGACY_UNKNOWN"))
	}
	responseEvidence, responseSkipped, err := loadBudgetedJSONEvidence(ctx, row.EvidenceDisposition, "responseBody", allowExternalEvidence, row.ResponseBody, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadResponseBodyEvidenceBounded(readCtx, row, serverMaxEvidenceBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	m["responseBody"] = responseEvidence
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "responseBody", responseEvidence)
	if responseEvidence.Reason == "cancelable_bounded_read_unsupported" {
		issues = append(issues, unsupportedBoundedReadIssue("requests", "request", row.ID, "responseBody"))
	} else if responseSkipped {
		issues = append(issues, externalSelectionIssue("requests", "request", row.ID, "responseBody"))
	} else if responseEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "RESPONSE_BODY_STORAGE_UNAVAILABLE"))
	} else if responseEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "RESPONSE_BODY_LEGACY_UNKNOWN"))
	}
	chunkSource := ""
	if row.Stream && row.Status == request.StatusProcessing {
		chunkSource = "live"
	}
	chunkEvidence, chunksSkipped, err := loadBudgetedChunkEvidence(ctx, row.EvidenceDisposition, allowExternalEvidence, chunkSource, row.ResponseChunks, func(readCtx context.Context) ([]objects.JSONRawMessage, error) {
		return s.requests.LoadResponseChunksBounded(readCtx, row, serverMaxEvidenceBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	m["responseChunks"] = chunkEvidence
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "responseChunks", chunkEvidence)
	if chunkEvidence.Reason == "cancelable_bounded_read_unsupported" {
		issues = append(issues, unsupportedBoundedReadIssue("requests", "request", row.ID, "responseChunks"))
	} else if chunksSkipped {
		issues = append(issues, externalSelectionIssue("requests", "request", row.ID, "responseChunks"))
	} else if chunkEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "RESPONSE_CHUNKS_STORAGE_UNAVAILABLE"))
	} else if chunkEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "RESPONSE_CHUNKS_LEGACY_UNKNOWN"))
	}
	if row.RoutingContext == nil {
		m["routingContext"] = map[string]any{"state": "legacyUnknown", "value": nil}
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "ROUTING_CONTEXT_LEGACY_UNKNOWN"))
	} else {
		m["routingContext"] = map[string]any{"state": "available", "value": routingContextForContract(row.RoutingContext)}
	}
	artifactEvidence := Evidence{State: "notApplicable", Source: "none", MediaType: "application/octet-stream"}
	if row.ContentSaved && row.ContentStorageID != nil && row.ContentStorageKey != nil {
		if !allowExternalEvidence {
			artifactEvidence = Evidence{State: "storageUnavailable", Source: "external", MediaType: "application/octet-stream", Reason: "explicit_request_ids_required"}
			issues = append(issues, externalSelectionIssue("requests", "request", row.ID, "contentArtifact"))
		} else {
			ds, loadErr := s.storage.GetDataStorageByID(ctx, *row.ContentStorageID)
			if loadErr == nil {
				var content []byte
				content, loadErr = withStorageReadDeadline(ctx, func(readCtx context.Context) ([]byte, error) {
					return s.storage.LoadDataBounded(readCtx, ds, *row.ContentStorageKey, serverMaxEvidenceBytes)
				})
				if err := checkPullContext(ctx); err != nil {
					return nil, nil, err
				}
				if errors.Is(loadErr, biz.ErrBoundedReadUnsupported) {
					artifactEvidence = Evidence{State: "storageUnavailable", Source: "external", MediaType: "application/octet-stream", Reason: "cancelable_bounded_read_unsupported"}
					issues = append(issues, unsupportedBoundedReadIssue("requests", "request", row.ID, "contentArtifact"))
					loadErr = nil
				} else if errors.Is(loadErr, biz.ErrDataTooLarge) {
					return nil, nil, serviceError(413, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
				}
				if loadErr == nil && artifactEvidence.Reason == "" {
					if err := ensureEvidenceBudget(content); err != nil {
						return nil, nil, err
					}
					sum := sha256.Sum256(content)
					mediaType := mime.TypeByExtension(filepath.Ext(*row.ContentStorageKey))
					if mediaType == "" {
						mediaType = "application/octet-stream"
					}
					artifactEvidence = Evidence{State: "available", Source: "external", MediaType: mediaType, Encoding: "base64", ByteLength: len(content), SHA256: hex.EncodeToString(sum[:]), Value: base64.StdEncoding.EncodeToString(content)}
				}
			}
			if loadErr != nil {
				artifactEvidence = Evidence{State: "storageUnavailable", Source: "external", MediaType: "application/octet-stream"}
				issues = append(issues, availabilityIssue("requests", "request", row.ID, "CONTENT_ARTIFACT_STORAGE_UNAVAILABLE"))
			}
		}
	}
	m["contentArtifact"] = map[string]any{"saved": row.ContentSaved, "storageId": row.ContentStorageID, "key": row.ContentStorageKey, "savedAt": row.ContentSavedAt, "content": artifactEvidence}
	return m, issues, nil
}
func (s *Service) executionRecord(ctx context.Context, row *ent.RequestExecution, allowExternalEvidence bool) (map[string]any, []Issue, error) {
	if err := checkPullContext(ctx); err != nil {
		return nil, nil, err
	}
	m := map[string]any{
		"id": row.ID, "projectId": row.ProjectID, "requestId": row.RequestID,
		"channelId": nullablePositiveInt(row.ChannelID), "dataStorageId": nullablePositiveInt(row.DataStorageID),
		"externalId": row.ExternalID, "modelId": row.ModelID, "format": row.Format,
		"errorMessage": row.ErrorMessage, "responseStatusCode": row.ResponseStatusCode, "status": row.Status,
		"stream": row.Stream, "metricsLatencyMs": row.MetricsLatencyMs,
		"metricsFirstTokenLatencyMs": row.MetricsFirstTokenLatencyMs, "metricsReasoningDurationMs": row.MetricsReasoningDurationMs,
		"requestUrl": row.RequestURL, "passThroughApplied": row.PassThroughApplied,
		"createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt,
	}
	issues := []Issue{}
	if err := ensureEvidenceBudget(row.RequestHeaders); err != nil {
		return nil, nil, err
	}
	requestHeaders := evidenceFromRaw(row.RequestHeaders, row.EvidenceDisposition, nil, "requestHeaders")
	m["requestHeaders"] = requestHeaders
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "requestHeaders", requestHeaders)
	requestEvidence, requestSkipped, err := loadBudgetedJSONEvidence(ctx, row.EvidenceDisposition, "requestBody", allowExternalEvidence, row.RequestBody, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadRequestExecutionRequestBodyBounded(readCtx, row, serverMaxEvidenceBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	requestEvidence = normalizeManagedRequestBodyEvidence(requestEvidence, row.RequestBodyPayloadID)
	m["requestBody"] = requestEvidence
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "requestBody", requestEvidence)
	if requestEvidence.Reason == "cancelable_bounded_read_unsupported" {
		issues = append(issues, unsupportedBoundedReadIssue("executions", "execution", row.ID, "requestBody"))
	} else if requestSkipped {
		issues = append(issues, externalSelectionIssue("executions", "execution", row.ID, "requestBody"))
	} else if requestEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "REQUEST_BODY_STORAGE_UNAVAILABLE"))
	} else if requestEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "REQUEST_BODY_LEGACY_UNKNOWN"))
	}
	responseEvidence, responseSkipped, err := loadBudgetedJSONEvidence(ctx, row.EvidenceDisposition, "responseBody", allowExternalEvidence, row.ResponseBody, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadRequestExecutionResponseBodyEvidenceBounded(readCtx, row, serverMaxEvidenceBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	m["responseBody"] = responseEvidence
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "responseBody", responseEvidence)
	if responseEvidence.Reason == "cancelable_bounded_read_unsupported" {
		issues = append(issues, unsupportedBoundedReadIssue("executions", "execution", row.ID, "responseBody"))
	} else if responseSkipped {
		issues = append(issues, externalSelectionIssue("executions", "execution", row.ID, "responseBody"))
	} else if responseEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_BODY_STORAGE_UNAVAILABLE"))
	} else if responseEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_BODY_LEGACY_UNKNOWN"))
	}
	chunkSource := ""
	if row.Stream && row.Status == requestexecution.StatusProcessing {
		chunkSource = "live"
	}
	chunkEvidence, chunksSkipped, err := loadBudgetedChunkEvidence(ctx, row.EvidenceDisposition, allowExternalEvidence, chunkSource, row.ResponseChunks, func(readCtx context.Context) ([]objects.JSONRawMessage, error) {
		return s.requests.LoadRequestExecutionResponseChunksBounded(readCtx, row, serverMaxEvidenceBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	m["responseChunks"] = chunkEvidence
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "responseChunks", chunkEvidence)
	if chunkEvidence.Reason == "cancelable_bounded_read_unsupported" {
		issues = append(issues, unsupportedBoundedReadIssue("executions", "execution", row.ID, "responseChunks"))
	} else if chunksSkipped {
		issues = append(issues, externalSelectionIssue("executions", "execution", row.ID, "responseChunks"))
	} else if chunkEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_CHUNKS_STORAGE_UNAVAILABLE"))
	} else if chunkEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_CHUNKS_LEGACY_UNKNOWN"))
	}
	return m, issues, nil
}

func evidenceFromRaw(raw objects.JSONRawMessage, d *objects.EvidenceDisposition, loadErr error, field string) Evidence {
	return evidenceFromRawWithSource(raw, d, loadErr, field, "")
}

func evidenceFromRawWithSource(raw objects.JSONRawMessage, d *objects.EvidenceDisposition, loadErr error, field, sourceOverride string) Evidence {
	if loadErr != nil {
		return Evidence{State: "storageUnavailable", Source: "external", MediaType: "application/json"}
	}
	var disp *objects.Disposition
	if d != nil {
		switch field {
		case "requestBody":
			disp = &d.RequestBody
		case "responseBody":
			disp = &d.ResponseBody
		case "responseChunks":
			disp = &d.ResponseChunks
		}
	}
	missing := len(raw) == 0 || string(raw) == "null" || (string(raw) == "{}" && field != "requestHeaders" && (disp == nil || disp.Outcome != "stored"))
	if missing {
		if disp == nil {
			return Evidence{State: "legacyUnknown", Source: "unknown", MediaType: "application/json"}
		}
		if disp.Intent == "omit" || disp.Outcome == "omitted" {
			return Evidence{State: "notPersisted", Source: "none", MediaType: "application/json"}
		}
		if disp.Intent == "notApplicable" {
			return Evidence{State: "notApplicable", Source: "none", MediaType: "application/json"}
		}
		if disp.Outcome == "unavailable" || disp.Outcome == "writeFailed" {
			return Evidence{State: "storageUnavailable", Source: dispositionSource(disp), MediaType: "application/json"}
		}
		if disp.Location == "external" && (disp.Outcome == "stored" || disp.Outcome == "writeFailed") {
			return Evidence{State: "storageUnavailable", Source: "external", MediaType: "application/json"}
		}
		return Evidence{State: "legacyUnknown", Source: "unknown", MediaType: "application/json"}
	}
	if !json.Valid(raw) {
		return Evidence{State: "legacyUnknown", Source: "unknown", MediaType: "application/json"}
	}
	source := "database"
	if disp != nil {
		source = dispositionSource(disp)
	}
	if sourceOverride != "" {
		source = sourceOverride
	}
	rawCopy := append(json.RawMessage(nil), raw...)
	rawSum := sha256.Sum256(rawCopy)
	evidence := Evidence{
		State:                  "available",
		Source:                 source,
		MediaType:              "application/json",
		Encoding:               "json",
		Canonicalization:       "rfc8785",
		CanonicalizationStatus: "available",
		ByteLength:             len(rawCopy),
		RawSHA256:              hex.EncodeToString(rawSum[:]),
		Value:                  rawCopy,
	}
	encoded, err := canonicalizeRFC8785(rawCopy)
	if err != nil {
		reason := "stored JSON cannot be represented by RFC 8785"
		if errors.Is(err, ErrRFC8785NumberOutOfDomain) {
			reason = "RFC 8785 requires integral values to be exactly representable as IEEE-754 binary64"
		}
		evidence.CanonicalizationStatus = "unavailable"
		evidence.CanonicalizationReason = reason
		return evidence
	}
	sum := sha256.Sum256(encoded)
	evidence.CanonicalSHA256 = hex.EncodeToString(sum[:])
	return evidence
}

func dispositionSource(disposition *objects.Disposition) string {
	switch disposition.Location {
	case "external":
		return disposition.Location
	case "managed":
		// Managed payloads live in the primary database. The diagnostics v1
		// source enum intentionally describes the storage boundary, not the
		// internal payload table.
		return "database"
	case "none":
		return "none"
	default:
		return "database"
	}
}

func normalizeManagedRequestBodyEvidence(evidence Evidence, payloadID *int) Evidence {
	if payloadID != nil {
		evidence.Source = "database"
	}
	return evidence
}

func appendCanonicalizationIssue(issues []Issue, section, recordType string, recordID int, field string, evidence Evidence) []Issue {
	if evidence.CanonicalizationStatus != "unavailable" {
		return issues
	}
	return append(issues, Issue{
		Code:       "EVIDENCE_CANONICALIZATION_UNAVAILABLE",
		Section:    section,
		RecordType: recordType,
		RecordID:   strconv.Itoa(recordID),
		Retryable:  false,
		Message:    field + " is available, but its RFC 8785 canonical hash is unavailable",
	})
}

func withStorageReadDeadline[T any](ctx context.Context, load func(context.Context) (T, error)) (T, error) {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return load(readCtx)
}

func (s *Service) loadChannels(ctx context.Context, kind string, projectID int, ids map[int]struct{}, principalType string, principalID int) ([]*ent.Channel, error) {
	q := s.ent.Channel.Query().Where(channel.StatusNEQ(channel.StatusArchived))
	if kind != "snapshot" {
		if len(ids) == 0 {
			return []*ent.Channel{}, nil
		}
		q = q.Where(channel.IDIn(mapKeys(ids)...))
		return q.Order(ent.Asc(channel.FieldID)).Limit(serverMaxRelatedRecords).All(ctx)
	}

	// Channel is a global entity, so a snapshot must derive project relevance
	// rather than exporting the global table. A channel is relevant when retained
	// project evidence references it or a currently visible key/access group could
	// select it under the project's active profile.
	rows, err := q.Order(ent.Asc(channel.FieldID)).Limit(serverMaxRelatedRecords + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "channel snapshot exceeds the server diagnostics work budget")
	}
	projectRow, err := s.ent.Project.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	keysQuery := s.ent.APIKey.Query().Where(apikey.ProjectIDEQ(projectID), apikey.StatusNEQ(apikey.StatusArchived))
	if principalType == "serviceAccount" {
		keysQuery = keysQuery.Where(apikey.TypeNEQ(apikey.TypePersonal), apikey.TypeNEQ(apikey.TypeUser))
	} else {
		keysQuery = keysQuery.Where(apikey.Or(
			apikey.TypeNEQ(apikey.TypePersonal),
			apikey.And(apikey.UserIDEQ(principalID), apikey.TypeEQ(apikey.TypePersonal)),
		))
	}
	keys, err := keysQuery.Limit(serverMaxRelatedRecords + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(keys) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "API key snapshot exceeds the server diagnostics work budget")
	}
	groups, err := s.ent.APIKeyProfileTemplate.Query().Where(apikeyprofiletemplate.ProjectIDEQ(projectID)).Limit(serverMaxRelatedRecords + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(groups) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "access group snapshot exceeds the server diagnostics work budget")
	}
	requestChannelIDs, err := s.ent.Request.Query().Where(request.ProjectIDEQ(projectID), request.ChannelIDNotNil()).Limit(serverMaxRelatedRecords + 1).Select(request.FieldChannelID).Ints(ctx)
	if err != nil {
		return nil, err
	}
	if len(requestChannelIDs) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "request references exceed the server diagnostics work budget")
	}
	executionChannelIDs, err := s.ent.RequestExecution.Query().Where(requestexecution.ProjectIDEQ(projectID), requestexecution.ChannelIDNotNil()).Limit(serverMaxRelatedRecords + 1).Select(requestexecution.FieldChannelID).Ints(ctx)
	if err != nil {
		return nil, err
	}
	if len(executionChannelIDs) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "execution references exceed the server diagnostics work budget")
	}
	referenced := map[int]struct{}{}
	for _, id := range append(requestChannelIDs, executionChannelIDs...) {
		referenced[id] = struct{}{}
	}
	projectProfile := projectRow.GetActiveProfile()
	result := make([]*ent.Channel, 0, len(rows))
	for _, row := range rows {
		if _, ok := referenced[row.ID]; ok {
			result = append(result, row)
			continue
		}
		if !projectProfileAllowsChannel(projectProfile, row) {
			continue
		}
		allowed := false
		for _, key := range keys {
			if apiKeyProfileAllowsChannel(key.GetActiveProfile(), row) {
				allowed = true
				break
			}
		}
		if !allowed {
			for _, group := range groups {
				if apiKeyProfileAllowsChannel(group.Profile, row) {
					allowed = true
					break
				}
			}
		}
		if allowed {
			result = append(result, row)
		}
	}
	return result, nil
}

func projectProfileAllowsChannel(profile *objects.ProjectProfile, row *ent.Channel) bool {
	if profile == nil {
		return true
	}
	if len(profile.ChannelIDs) > 0 && !containsInt(profile.ChannelIDs, row.ID) {
		return false
	}
	return len(profile.ChannelTags) == 0 || profile.MatchChannelTags(row.Tags)
}

func apiKeyProfileAllowsChannel(profile *objects.APIKeyProfile, row *ent.Channel) bool {
	if profile == nil {
		return true
	}
	if len(profile.ChannelIDs) > 0 && !containsInt(profile.ChannelIDs, row.ID) {
		return false
	}
	return len(profile.ChannelTags) == 0 || profile.MatchChannelTags(row.Tags)
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func (s *Service) loadAPIKeys(ctx context.Context, kind string, projectID int, ids map[int]struct{}, principalType string, principalID int) ([]*ent.APIKey, error) {
	q := s.ent.APIKey.Query().Where(apikey.ProjectIDEQ(projectID))
	if kind != "snapshot" {
		if len(ids) == 0 {
			return []*ent.APIKey{}, nil
		}
		q = q.Where(apikey.IDIn(mapKeys(ids)...))
	}
	if principalType == "serviceAccount" {
		q = q.Where(apikey.TypeNEQ(apikey.TypePersonal), apikey.TypeNEQ(apikey.TypeUser))
	} else {
		q = q.Where(apikey.Or(
			apikey.TypeNEQ(apikey.TypePersonal),
			apikey.And(apikey.UserIDEQ(principalID), apikey.TypeEQ(apikey.TypePersonal)),
		))
	}
	rows, err := q.Order(ent.Asc(apikey.FieldID)).Limit(serverMaxRelatedRecords + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "API key selection exceeds the server diagnostics work budget")
	}
	return rows, nil
}
func (s *Service) loadGroups(ctx context.Context, kind string, projectID int, ids map[int]struct{}) ([]*ent.APIKeyProfileTemplate, error) {
	q := s.ent.APIKeyProfileTemplate.Query().Where(apikeyprofiletemplate.ProjectIDEQ(projectID))
	if kind != "snapshot" {
		if len(ids) == 0 {
			return []*ent.APIKeyProfileTemplate{}, nil
		}
		q = q.Where(apikeyprofiletemplate.IDIn(mapKeys(ids)...))
	}
	rows, err := q.Order(ent.Asc(apikeyprofiletemplate.FieldID)).Limit(serverMaxRelatedRecords + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) > serverMaxRelatedRecords {
		return nil, serviceError(413, "SERVER_WORK_BUDGET_EXCEEDED", "access group selection exceeds the server diagnostics work budget")
	}
	return rows, nil
}
func channelRecord(row *ent.Channel, credentials bool) map[string]any {
	m := map[string]any{
		"id": row.ID, "type": row.Type, "baseUrl": credentialAwareURL(row.BaseURL, credentials),
		"name": row.Name, "status": row.Status, "supportedModels": nonNilSlice(row.SupportedModels), "manualModels": nonNilSlice(row.ManualModels),
		"autoSyncSupportedModels": row.AutoSyncSupportedModels, "autoSyncModelPattern": row.AutoSyncModelPattern,
		"tags": nonNilSlice(row.Tags), "defaultTestModel": row.DefaultTestModel, "policies": row.Policies,
		"settings": channelSettingsRecord(row.Settings, credentials), "orderingWeight": row.OrderingWeight,
		"errorMessage": row.ErrorMessage, "remark": row.Remark, "createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt,
	}
	endpoints := make([]any, 0, len(row.Endpoints))
	for _, endpoint := range row.Endpoints {
		endpoints = append(endpoints, map[string]any{
			"apiFormat": endpoint.APIFormat,
			"path":      endpoint.Path,
			"baseUrl":   credentialAwareURL(endpoint.BaseURL, credentials),
			"transport": endpoint.Transport,
		})
	}
	m["endpoints"] = endpoints
	if credentials {
		m["credentials"] = Credential{Status: "included", Value: row.Credentials}
		m["disabledApiKeys"] = Credential{Status: "included", Value: row.DisabledAPIKeys}
	} else {
		m["credentials"] = Credential{Status: "excluded"}
		m["disabledApiKeys"] = Credential{Status: "excluded"}
	}
	return m
}

func channelSettingsRecord(settings *objects.ChannelSettings, include bool) any {
	if settings == nil {
		return nil
	}
	raw, _ := json.Marshal(settings)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	wrap := func(value any) Credential {
		if include {
			return Credential{Status: "included", Value: value}
		}
		return Credential{Status: "excluded"}
	}
	if value, ok := m["overrideParameters"]; ok {
		m["overrideParameters"] = wrap(value)
	}
	for _, field := range []string{"autoTrimedModelPrefixes", "modelMappings", "overrideHeaders"} {
		if m[field] == nil {
			m[field] = []any{}
		}
	}
	if entries, ok := m["overrideHeaders"].([]any); ok {
		for _, item := range entries {
			if entry, ok := item.(map[string]any); ok {
				entry["value"] = wrap(entry["value"])
			}
		}
	}
	for _, field := range []string{"bodyOverrideOperations", "headerOverrideOperations"} {
		if operations, ok := m[field].([]any); ok {
			for _, item := range operations {
				if operation, ok := item.(map[string]any); ok {
					if value, exists := operation["value"]; exists {
						operation["value"] = wrap(value)
					}
					if match, ok := operation["match"].(map[string]any); ok {
						if value, exists := match["eq"]; exists {
							match["eq"] = wrap(value)
						}
					}
				}
			}
		}
	}
	if proxy, ok := m["proxy"].(map[string]any); ok {
		proxy["url"] = credentialAwareURL(stringValue(proxy["url"]), include)
		proxy["username"] = wrap(proxy["username"])
		proxy["password"] = wrap(proxy["password"])
	}
	if quota, ok := m["providerQuota"].(map[string]any); ok {
		if opencode, ok := quota["opencodeGo"].(map[string]any); ok {
			opencode["authCookie"] = wrap(opencode["authCookie"])
		}
	}
	return m
}

func credentialAwareURL(raw string, include bool) map[string]any {
	wrap := func(value any) Credential {
		if include {
			return Credential{Status: "included", Value: value}
		}
		return Credential{Status: "excluded"}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		base := raw
		if index := strings.IndexByte(base, '?'); index >= 0 {
			base = base[:index]
		}
		return map[string]any{"base": base, "userInfo": Credential{Status: "unavailable", Reason: "invalid_url"}, "query": wrap("")}
	}
	userInfo := ""
	if parsed.User != nil {
		userInfo = parsed.User.String()
		parsed.User = nil
	}
	query := parsed.RawQuery
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	return map[string]any{"base": parsed.String(), "userInfo": wrap(userInfo), "query": wrap(query)}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
func apiKeyRecord(row *ent.APIKey, credentials bool) map[string]any {
	var userID any
	if row.UserID != 0 {
		userID = row.UserID
	}
	m := map[string]any{
		"id": row.ID, "userId": userID, "projectId": row.ProjectID, "name": row.Name,
		"type": row.Type, "status": row.Status, "scopes": nonNilSlice(row.Scopes), "profiles": profilesForContract(row.Profiles),
		"provisioningSource": "native", "profileMode": "inline",
		"accessGroupId": nil, "accessGroupRevision": nil,
		"createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt,
	}
	if credentials {
		m["key"] = Credential{Status: "included", Value: row.Key}
	} else {
		m["key"] = Credential{Status: "excluded"}
	}
	return m
}

func usageRecord(row *ent.UsageLog) map[string]any {
	m := map[string]any{
		"id": row.ID, "requestId": row.RequestID, "apiKeyId": nullablePositiveInt(row.APIKeyID),
		"projectId": row.ProjectID, "channelId": nullablePositiveInt(row.ChannelID), "modelId": row.ModelID,
		"promptTokens": row.PromptTokens, "completionTokens": row.CompletionTokens, "totalTokens": row.TotalTokens,
		"promptAudioTokens": row.PromptAudioTokens, "promptCachedTokens": row.PromptCachedTokens,
		"promptWriteCachedTokens": row.PromptWriteCachedTokens, "promptWriteCachedTokens5m": row.PromptWriteCachedTokens5m,
		"promptWriteCachedTokens1h": row.PromptWriteCachedTokens1h, "completionAudioTokens": row.CompletionAudioTokens,
		"completionReasoningTokens": row.CompletionReasoningTokens, "completionAcceptedPredictionTokens": row.CompletionAcceptedPredictionTokens,
		"completionRejectedPredictionTokens": row.CompletionRejectedPredictionTokens, "source": row.Source, "format": row.Format,
		"costItems": nonNilSlice(row.CostItems), "costPriceReferenceId": row.CostPriceReferenceID,
		"createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt,
	}
	if row.TotalCost == nil {
		m["totalCost"] = nil
	} else {
		m["totalCost"] = strconv.FormatFloat(*row.TotalCost, 'f', -1, 64)
	}
	return m
}

func traceRecord(row *ent.Trace) map[string]any {
	return map[string]any{"id": row.ID, "projectId": row.ProjectID, "traceId": row.TraceID, "threadDatabaseId": nullablePositiveInt(row.ThreadID), "status": row.Status, "createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt}
}

func threadRecord(row *ent.Thread) map[string]any {
	return map[string]any{"id": row.ID, "projectId": row.ProjectID, "threadId": row.ThreadID, "status": row.Status, "createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt}
}

func (s *Service) accessGroupRecord(_ context.Context, row *ent.APIKeyProfileTemplate, _ map[int64]struct{}) (map[string]any, error) {
	return map[string]any{
		"id": row.ID, "projectId": row.ProjectID, "name": row.Name, "description": row.Description,
		"profile": profileForContract(row.Profile), "revision": 0, "selfServiceVisible": false,
		"attachedKeyCount": 0, "createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt, "referencedRevisions": []any{},
	}, nil
}
func (s *Service) configurationRecord(ctx context.Context, credentials bool, storageIDs map[int]struct{}) (map[string]any, error) {
	invite := Credential{Status: "unavailable", Reason: "registration_not_supported"}
	storagePolicy := *s.systems.StoragePolicyOrDefault(ctx)
	storagePolicy.CleanupOptions = nonNilSlice(storagePolicy.CleanupOptions)
	retryPolicy := *s.systems.RetryPolicyOrDefault(ctx)
	retryPolicy.AutoDisableChannel.Statuses = nonNilSlice(retryPolicy.AutoDisableChannel.Statuses)
	retryPolicy.EmptyResponseTextPatterns = nonNilSlice(retryPolicy.EmptyResponseTextPatterns)
	modelSettings := *s.systems.ModelSettingsOrDefault(ctx)
	modelSettings.DeveloperSettings = nonNilSlice(modelSettings.DeveloperSettings)
	userAgentPassThrough, _ := s.systems.UserAgentPassThrough(ctx)
	bodyPassThrough, _ := s.systems.PassThrough(ctx)
	defaultStorageID, _ := s.systems.DefaultDataStorageID(ctx)
	if defaultStorageID != 0 {
		storageIDs[defaultStorageID] = struct{}{}
	}
	dataStorages := []any{}
	if len(storageIDs) > 0 {
		rows, err := s.ent.DataStorage.Query().Where(datastorage.IDIn(mapKeys(storageIDs)...)).Order(ent.Asc(datastorage.FieldID)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			dataStorages = append(dataStorages, dataStorageRecord(row, credentials))
		}
	}
	return map[string]any{
		"storagePolicy": storagePolicy, "retryPolicy": retryPolicy, "modelSettings": modelSettings,
		"userAgentPassThrough": userAgentPassThrough, "bodyPassThrough": bodyPassThrough,
		"defaultDataStorageId": nullablePositiveInt(defaultStorageID), "dataStorages": dataStorages,
		"registration": map[string]any{"enabled": false, "oidcEnabled": false, "defaultProjectId": 0, "autoJoinFirstProject": false, "defaultProjectScopes": []string{}, "selfServiceEnabled": false, "allowRequestDetails": false, "inviteCode": invite},
	}, nil
}

func dataStorageRecord(row *ent.DataStorage, include bool) map[string]any {
	wrap := func(value any) Credential {
		if include {
			return Credential{Status: "included", Value: value}
		}
		return Credential{Status: "excluded"}
	}
	settings := map[string]any{"dsn": wrap(nil), "directory": nil, "s3": nil, "gcs": nil, "webdav": nil}
	if row.Settings != nil {
		settings["dsn"] = wrap(row.Settings.DSN)
		settings["directory"] = row.Settings.Directory
		if value := row.Settings.S3; value != nil {
			settings["s3"] = map[string]any{"bucketName": value.BucketName, "endpoint": credentialAwareURL(value.Endpoint, include), "region": value.Region, "accessKey": wrap(value.AccessKey), "secretKey": wrap(value.SecretKey), "pathStyle": value.PathStyle}
		}
		if value := row.Settings.GCS; value != nil {
			settings["gcs"] = map[string]any{"bucketName": value.BucketName, "credential": wrap(value.Credential)}
		}
		if value := row.Settings.WebDAV; value != nil {
			settings["webdav"] = map[string]any{"url": credentialAwareURL(value.URL, include), "username": wrap(value.Username), "password": wrap(value.Password), "insecureSkipTls": value.InsecureSkipTLS, "path": value.Path}
		}
	}
	return map[string]any{"id": row.ID, "name": row.Name, "description": row.Description, "primary": row.Primary, "type": row.Type, "status": row.Status, "settings": settings, "createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt}
}

func nullablePositiveInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func emptySections(requested map[string]bool) Sections {
	mk := func(name string) Section {
		if requested[name] {
			return Section{Status: "available", Data: []any{}, Issues: []Issue{}}
		}
		return Section{Status: "notRequested", Data: nil, Issues: []Issue{}}
	}
	return Sections{mk("health"), mk("configuration"), mk("requests"), mk("executions"), mk("usage"), mk("traces"), mk("threads"), mk("channels"), mk("apiKeys"), mk("accessGroups")}
}
func sectionWithStatus(data any, issues []Issue) Section {
	if issues == nil {
		issues = []Issue{}
	}
	status := "available"
	if len(issues) > 0 {
		status = "partial"
	}
	return Section{Status: status, Data: data, Issues: issues}
}
func requestIDs(rows []*ent.Request) []int {
	out := make([]int, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}
func mapKeys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}
func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
func profileForContract(profile *objects.APIKeyProfile) *objects.APIKeyProfile {
	if profile == nil {
		return nil
	}
	clone := profile.Clone()
	clone.ModelMappings = nonNilSlice(clone.ModelMappings)
	return clone
}
func profilesForContract(profiles *objects.APIKeyProfiles) *objects.APIKeyProfiles {
	if profiles == nil {
		return nil
	}
	clone := &objects.APIKeyProfiles{ActiveProfile: profiles.ActiveProfile, Profiles: make([]objects.APIKeyProfile, 0, len(profiles.Profiles))}
	for i := range profiles.Profiles {
		clone.Profiles = append(clone.Profiles, *profileForContract(&profiles.Profiles[i]))
	}
	return clone
}
func routingContextForContract(value *objects.RoutingContext) *objects.RoutingContext {
	if value == nil {
		return nil
	}
	clone := *value
	clone.EffectiveProfiles = profilesForContract(value.EffectiveProfiles)
	return &clone
}
func sortRequests(rows []*ent.Request) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
}
func makeRefs(kind string, ids []string) []RequestRef {
	out := make([]RequestRef, len(ids))
	for i, id := range ids {
		out[i] = RequestRef{Kind: kind, Value: id, Status: "notFoundOrNotAuthorized"}
	}
	return out
}
func uniqueComparable[T comparable](values []T) bool {
	seen := map[T]struct{}{}
	for _, v := range values {
		if _, ok := seen[v]; ok {
			return false
		}
		seen[v] = struct{}{}
	}
	return true
}
func allPositive(values []int) bool {
	for _, value := range values {
		if value <= 0 {
			return false
		}
	}
	return true
}
func allRequestStatuses(values []string) bool {
	for _, value := range values {
		if !contains([]string{"pending", "processing", "completed", "failed", "canceled"}, value) {
			return false
		}
	}
	return true
}
func validStrings(values []string) bool {
	if !uniqueComparable(values) {
		return false
	}
	for _, v := range values {
		if strings.TrimSpace(v) == "" || len([]byte(v)) > 512 {
			return false
		}
	}
	return true
}
func contains(values []string, v string) bool {
	for _, item := range values {
		if item == v {
			return true
		}
	}
	return false
}
func nullableBuildTime() any {
	if build.BuildTime == "" {
		return nil
	}
	return build.BuildTime
}
func coreDBError() error {
	return serviceError(503, "CORE_DATABASE_UNAVAILABLE", "diagnostic selection is temporarily unavailable")
}
func availabilityIssue(section, typ string, id int, code string) Issue {
	return Issue{Code: code, Section: section, RecordType: typ, RecordID: strconv.Itoa(id), Retryable: true, Message: "persisted evidence could not be loaded"}
}
func issuesFor(all []Issue, section string) []Issue {
	out := []Issue{}
	for _, issue := range all {
		if issue.Section == section {
			out = append(out, issue)
		}
	}
	return out
}

func requestBinding(req PullRequest) string {
	req.Page = nil
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) cursorKey(ctx context.Context) ([]byte, error) {
	secret, err := authz.RunWithSystemBypass(ctx, "diagnostics-cursor-key", func(bypassCtx context.Context) (string, error) { return s.systems.SecretKey(bypassCtx) })
	if err != nil {
		return nil, err
	}
	reader := hkdf.New(sha256.New, []byte(secret), nil, []byte("axonhub-diagnostics-cursor-v1"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, err
	}
	return key, nil
}
func (s *Service) sealCursor(ctx context.Context, payload cursorPayload) (string, error) {
	key, err := s.cursorKey(ctx)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	signed := append(raw, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(signed), nil
}
func (s *Service) openCursor(ctx context.Context, token string, req PullRequest) (*cursorPayload, error) {
	signed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(signed) <= sha256.Size {
		return nil, serviceError(409, "CURSOR_EXPIRED", "cursor is invalid or expired")
	}
	raw, sig := signed[:len(signed)-sha256.Size], signed[len(signed)-sha256.Size:]
	key, err := s.cursorKey(ctx)
	if err != nil {
		return nil, coreDBError()
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, serviceError(409, "CURSOR_EXPIRED", "cursor is invalid or expired")
	}
	var payload cursorPayload
	if json.Unmarshal(raw, &payload) != nil || time.Now().UTC().After(payload.ExpiresAt) {
		return nil, serviceError(409, "CURSOR_EXPIRED", "cursor is invalid or expired")
	}
	if payload.Binding != requestBinding(req) {
		return nil, serviceError(409, "CURSOR_MISMATCH", "cursor does not match the diagnostics request")
	}
	return &payload, nil
}

var _ = errors.Is
var _ = fmt.Sprintf
