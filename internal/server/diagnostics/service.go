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

	"github.com/google/uuid"
	"go.uber.org/fx"
	"golang.org/x/crypto/hkdf"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/build"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/apikeyprofiletemplate"
	"github.com/looplj/axonhub/internal/ent/apikeyprofiletemplaterevision"
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
	credentialsAuthorized := req.Include.Credentials && principalType == "user" && hasUser && user.IsOwner
	if req.Include.Credentials && !credentialsAuthorized {
		return nil, serviceError(403, "CREDENTIAL_EXPORT_FORBIDDEN", "credential export requires a system-owner user")
	}
	if req.Scope.SubjectUserID != nil {
		if principalType == "serviceAccount" || *req.Scope.SubjectUserID != principalID {
			if !authz.HasScope(ctx, scopes.ScopeReadUsers) {
				return nil, serviceError(403, "SCOPE_FORBIDDEN", "selecting another subject requires read_users")
			}
		}
	}

	limits := defaultLimits(req.Limits)
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
				record, itemIssues := s.requestRecord(bypassCtx, item)
				requestRecords = append(requestRecords, record)
				issues = append(issues, itemIssues...)
			}
		}
		ids := requestIDs(selected)
		if len(ids) > 0 {
			if requested["executions"] || requested["channels"] || requested["configuration"] {
				execs, qerr := s.ent.RequestExecution.Query().Where(requestexecution.RequestIDIn(ids...), requestexecution.CreatedAtLTE(asOf)).Order(ent.Asc(requestexecution.FieldCreatedAt), ent.Asc(requestexecution.FieldID)).All(bypassCtx)
				if qerr != nil {
					return nil, coreDBError()
				}
				if requested["executions"] && len(execs) > limits.MaxExecutions {
					return nil, serviceError(413, "RELATED_LIMIT_EXCEEDED", "execution limit exceeded")
				}
				for _, exec := range execs {
					if exec.DataStorageID != 0 {
						storageIDs[exec.DataStorageID] = struct{}{}
					}
					if exec.ChannelID != 0 {
						channelIDs[exec.ChannelID] = struct{}{}
					}
					if requested["executions"] {
						record, itemIssues := s.executionRecord(bypassCtx, exec)
						executionRecords = append(executionRecords, record)
						issues = append(issues, itemIssues...)
					}
				}
			}
			if requested["usage"] {
				rows, qerr := s.ent.UsageLog.Query().Where(usagelog.RequestIDIn(ids...), usagelog.CreatedAtLTE(asOf)).Order(ent.Asc(usagelog.FieldCreatedAt), ent.Asc(usagelog.FieldID)).All(bypassCtx)
				if qerr != nil {
					return nil, coreDBError()
				}
				for _, row := range rows {
					if row.ChannelID != 0 {
						channelIDs[row.ChannelID] = struct{}{}
					}
					usageRecords = append(usageRecords, usageRecord(row))
				}
			}
		}
		if (requested["traces"] || requested["threads"]) && len(traceIDs) > 0 {
			rows, qerr := s.ent.Trace.Query().Where(trace.IDIn(mapKeys(traceIDs)...), trace.CreatedAtLTE(asOf)).Order(ent.Asc(trace.FieldCreatedAt), ent.Asc(trace.FieldID)).All(bypassCtx)
			if qerr != nil {
				return nil, coreDBError()
			}
			for _, row := range rows {
				if row.ThreadID != 0 {
					threadIDs[row.ThreadID] = struct{}{}
				}
				if requested["traces"] {
					traceRecords = append(traceRecords, traceRecord(row))
				}
			}
		}
		if requested["threads"] && len(threadIDs) > 0 {
			rows, qerr := s.ent.Thread.Query().Where(thread.IDIn(mapKeys(threadIDs)...), thread.CreatedAtLTE(asOf)).Order(ent.Asc(thread.FieldCreatedAt), ent.Asc(thread.FieldID)).All(bypassCtx)
			if qerr != nil {
				return nil, coreDBError()
			}
			for _, row := range rows {
				threadRecords = append(threadRecords, threadRecord(row))
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
		sections.Health = s.healthSection(bypassCtx)
	}

	channelRecords := []any{}
	keyRecords := []any{}
	groupRecords := []any{}
	if requested["channels"] {
		rows, qerr := s.loadChannels(bypassCtx, req.Selector.Kind, projectID, channelIDs, principalType, principalID)
		if qerr != nil {
			return nil, coreDBError()
		}
		for _, row := range rows {
			channelRecords = append(channelRecords, channelRecord(row, credentialsAuthorized))
		}
		sections.Channels = sectionWithStatus(channelRecords, nil)
	}
	if requested["apiKeys"] {
		rows, qerr := s.loadAPIKeys(bypassCtx, req.Selector.Kind, projectID, keyIDs, principalType, principalID)
		if qerr != nil {
			return nil, coreDBError()
		}
		for _, row := range rows {
			if row.AccessGroupID != nil {
				groupIDs[*row.AccessGroupID] = struct{}{}
			}
			keyRecords = append(keyRecords, apiKeyRecord(row, credentialsAuthorized))
		}
		sections.APIKeys = sectionWithStatus(keyRecords, nil)
	}
	if requested["accessGroups"] {
		if req.Selector.Kind != "snapshot" && len(groupIDs) == 0 && len(keyIDs) > 0 {
			keys, qerr := s.loadAPIKeys(bypassCtx, req.Selector.Kind, projectID, keyIDs, principalType, principalID)
			if qerr != nil {
				return nil, coreDBError()
			}
			for _, row := range keys {
				if row.AccessGroupID != nil {
					groupIDs[*row.AccessGroupID] = struct{}{}
				}
			}
		}
		rows, qerr := s.loadGroups(bypassCtx, req.Selector.Kind, projectID, groupIDs)
		if qerr != nil {
			return nil, coreDBError()
		}
		for _, row := range rows {
			record, qerr := s.accessGroupRecord(bypassCtx, row, referencedGroupRevisions[row.ID])
			if qerr != nil {
				return nil, coreDBError()
			}
			groupRecords = append(groupRecords, record)
		}
		sections.AccessGroups = sectionWithStatus(groupRecords, nil)
	}
	emittedStorageCount := 0
	if requested["configuration"] {
		configuration, qerr := s.configurationRecord(bypassCtx, credentialsAuthorized, storageIDs)
		if qerr != nil {
			return nil, coreDBError()
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
	response := &PullResponse{Contract: ContractResponse{Name: ContractName, Major: ContractMajor, Minor: ContractMinor, SchemaSHA256: SchemaSHA256}, Bundle: Bundle{ID: bundleID, GeneratedAt: generated, PageIndex: pageIndex, PageGeneratedAt: now, Status: status}, Server: ServerInfo{Version: build.Version, Commit: build.Commit, BuildTime: nullableBuildTime(), UptimeSeconds: int64(time.Since(build.StartTime).Seconds())}, Authorization: Authorization{PrincipalType: principalType, PrincipalID: principalID, ProjectID: projectID, SubjectUserID: req.Scope.SubjectUserID, CredentialsIncluded: credentialsAuthorized, PersonalDataExcluded: principalType == "serviceAccount" || req.Scope.SubjectUserID == nil || (req.Scope.SubjectUserID != nil && *req.Scope.SubjectUserID != principalID)}, Selection: Selection{Selector: req.Selector, AsOf: generated, Order: "createdAtAsc,idAsc", RequestRefs: refs, Counts: Counts{Requests: len(requestRecords), Executions: len(executionRecords), Usage: len(usageRecords), Traces: len(traceRecords), Threads: len(threadRecords), Channels: len(channelRecords), APIKeys: len(keyRecords), AccessGroups: len(groupRecords)}, HasMore: hasMore, NextCursor: nextCursor}, Sections: sections, Issues: issues}
	encoded, _ := json.Marshal(response)
	if len(encoded) > limits.MaxResponseBytes {
		return nil, serviceError(413, "RESPONSE_TOO_LARGE", "reduce maxRequests or increase maxResponseBytes within the hard limit")
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
	nonPersonalKey := apikey.And(
		apikey.TypeNEQ(apikey.TypePersonal),
		apikey.Or(apikey.TypeNEQ(apikey.TypeUser), apikey.ProvisioningSourceNEQ(apikey.ProvisioningSourceLegacyUnknown)),
	)
	personalKey := apikey.Or(
		apikey.TypeEQ(apikey.TypePersonal),
		apikey.And(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)),
	)
	privacy := request.Or(request.APIKeyIDIsNil(), request.HasAPIKeyWith(nonPersonalKey))
	if principalType == "user" {
		privacy = request.Or(privacy, request.HasAPIKeyWith(personalKey, apikey.UserIDEQ(principalID)))
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
		rows, err := q.All(ctx)
		if err != nil {
			return nil, nil, false, coreDBError()
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
	rows, err := q.Order(ent.Desc(request.FieldCreatedAt), ent.Desc(request.FieldID)).Limit(limits.MaxRequests + 1).All(ctx)
	if err != nil {
		return nil, nil, false, coreDBError()
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

func (s *Service) requestRecord(ctx context.Context, row *ent.Request) (map[string]any, []Issue) {
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
	requestHeaders := evidenceFromRaw(row.RequestHeaders, row.EvidenceDisposition, nil, "requestHeaders")
	m["requestHeaders"] = requestHeaders
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "requestHeaders", requestHeaders)
	body, e := withStorageReadDeadline(ctx, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadRequestBody(readCtx, row)
	})
	requestEvidence := evidenceFromRaw(body, row.EvidenceDisposition, e, "requestBody")
	m["requestBody"] = requestEvidence
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "requestBody", requestEvidence)
	if requestEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "REQUEST_BODY_STORAGE_UNAVAILABLE"))
	} else if requestEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "REQUEST_BODY_LEGACY_UNKNOWN"))
	}
	body, e = withStorageReadDeadline(ctx, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadResponseBody(readCtx, row)
	})
	responseEvidence := evidenceFromRaw(body, row.EvidenceDisposition, e, "responseBody")
	m["responseBody"] = responseEvidence
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "responseBody", responseEvidence)
	if responseEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "RESPONSE_BODY_STORAGE_UNAVAILABLE"))
	} else if responseEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("requests", "request", row.ID, "RESPONSE_BODY_LEGACY_UNKNOWN"))
	}
	chunks, e := withStorageReadDeadline(ctx, func(readCtx context.Context) ([]objects.JSONRawMessage, error) {
		return s.requests.LoadResponseChunks(readCtx, row)
	})
	chunkSource := ""
	if row.Stream && row.Status == request.StatusProcessing {
		chunkSource = "live"
		if chunks == nil {
			chunks = []objects.JSONRawMessage{}
		}
	}
	raw, _ := json.Marshal(chunks)
	chunkEvidence := evidenceFromRawWithSource(raw, row.EvidenceDisposition, e, "responseChunks", chunkSource)
	m["responseChunks"] = chunkEvidence
	issues = appendCanonicalizationIssue(issues, "requests", "request", row.ID, "responseChunks", chunkEvidence)
	if chunkEvidence.State == "storageUnavailable" {
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
		ds, loadErr := s.storage.GetDataStorageByID(ctx, *row.ContentStorageID)
		if loadErr == nil {
			var content []byte
			content, loadErr = withStorageReadDeadline(ctx, func(readCtx context.Context) ([]byte, error) {
				return s.storage.LoadData(readCtx, ds, *row.ContentStorageKey)
			})
			if loadErr == nil {
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
	m["contentArtifact"] = map[string]any{"saved": row.ContentSaved, "storageId": row.ContentStorageID, "key": row.ContentStorageKey, "savedAt": row.ContentSavedAt, "content": artifactEvidence}
	return m, issues
}
func (s *Service) executionRecord(ctx context.Context, row *ent.RequestExecution) (map[string]any, []Issue) {
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
	requestHeaders := evidenceFromRaw(row.RequestHeaders, row.EvidenceDisposition, nil, "requestHeaders")
	m["requestHeaders"] = requestHeaders
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "requestHeaders", requestHeaders)
	body, e := withStorageReadDeadline(ctx, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadRequestExecutionRequestBody(readCtx, row)
	})
	requestEvidence := evidenceFromRaw(body, row.EvidenceDisposition, e, "requestBody")
	m["requestBody"] = requestEvidence
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "requestBody", requestEvidence)
	if requestEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "REQUEST_BODY_STORAGE_UNAVAILABLE"))
	} else if requestEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "REQUEST_BODY_LEGACY_UNKNOWN"))
	}
	body, e = withStorageReadDeadline(ctx, func(readCtx context.Context) (objects.JSONRawMessage, error) {
		return s.requests.LoadRequestExecutionResponseBody(readCtx, row)
	})
	responseEvidence := evidenceFromRaw(body, row.EvidenceDisposition, e, "responseBody")
	m["responseBody"] = responseEvidence
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "responseBody", responseEvidence)
	if responseEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_BODY_STORAGE_UNAVAILABLE"))
	} else if responseEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_BODY_LEGACY_UNKNOWN"))
	}
	chunks, e := withStorageReadDeadline(ctx, func(readCtx context.Context) ([]objects.JSONRawMessage, error) {
		return s.requests.LoadRequestExecutionResponseChunks(readCtx, row)
	})
	chunkSource := ""
	if row.Stream && row.Status == requestexecution.StatusProcessing {
		chunkSource = "live"
		if chunks == nil {
			chunks = []objects.JSONRawMessage{}
		}
	}
	raw, _ := json.Marshal(chunks)
	chunkEvidence := evidenceFromRawWithSource(raw, row.EvidenceDisposition, e, "responseChunks", chunkSource)
	m["responseChunks"] = chunkEvidence
	issues = appendCanonicalizationIssue(issues, "executions", "execution", row.ID, "responseChunks", chunkEvidence)
	if chunkEvidence.State == "storageUnavailable" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_CHUNKS_STORAGE_UNAVAILABLE"))
	} else if chunkEvidence.State == "legacyUnknown" {
		issues = append(issues, availabilityIssue("executions", "execution", row.ID, "RESPONSE_CHUNKS_LEGACY_UNKNOWN"))
	}
	return m, issues
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
		if disp.Location == "external" {
			source = "external"
		}
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
		return q.Order(ent.Asc(channel.FieldID)).All(ctx)
	}

	// Channel is a global entity, so a snapshot must derive project relevance
	// rather than exporting the global table. A channel is relevant when retained
	// project evidence references it or a currently visible key/access group could
	// select it under the project's active profile.
	rows, err := q.Order(ent.Asc(channel.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	projectRow, err := s.ent.Project.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	keysQuery := s.ent.APIKey.Query().Where(apikey.ProjectIDEQ(projectID), apikey.StatusNEQ(apikey.StatusArchived))
	if principalType == "serviceAccount" {
		keysQuery = keysQuery.Where(apikey.TypeNEQ(apikey.TypePersonal), apikey.Or(apikey.TypeNEQ(apikey.TypeUser), apikey.ProvisioningSourceNEQ(apikey.ProvisioningSourceLegacyUnknown)))
	} else {
		keysQuery = keysQuery.Where(apikey.Or(
			apikey.And(apikey.TypeNEQ(apikey.TypePersonal), apikey.Or(apikey.TypeNEQ(apikey.TypeUser), apikey.ProvisioningSourceNEQ(apikey.ProvisioningSourceLegacyUnknown))),
			apikey.And(apikey.UserIDEQ(principalID), apikey.Or(apikey.TypeEQ(apikey.TypePersonal), apikey.And(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)))),
		))
	}
	keys, err := keysQuery.All(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := s.ent.APIKeyProfileTemplate.Query().Where(apikeyprofiletemplate.ProjectIDEQ(projectID)).All(ctx)
	if err != nil {
		return nil, err
	}
	requestChannelIDs, err := s.ent.Request.Query().Where(request.ProjectIDEQ(projectID), request.ChannelIDNotNil()).Select(request.FieldChannelID).Ints(ctx)
	if err != nil {
		return nil, err
	}
	executionChannelIDs, err := s.ent.RequestExecution.Query().Where(requestexecution.ProjectIDEQ(projectID), requestexecution.ChannelIDNotNil()).Select(requestexecution.FieldChannelID).Ints(ctx)
	if err != nil {
		return nil, err
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
		q = q.Where(apikey.TypeNEQ(apikey.TypePersonal), apikey.Or(apikey.TypeNEQ(apikey.TypeUser), apikey.ProvisioningSourceNEQ(apikey.ProvisioningSourceLegacyUnknown)))
	} else {
		q = q.Where(apikey.Or(
			apikey.And(apikey.TypeNEQ(apikey.TypePersonal), apikey.Or(apikey.TypeNEQ(apikey.TypeUser), apikey.ProvisioningSourceNEQ(apikey.ProvisioningSourceLegacyUnknown))),
			apikey.And(apikey.UserIDEQ(principalID), apikey.Or(apikey.TypeEQ(apikey.TypePersonal), apikey.And(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)))),
		))
	}
	return q.Order(ent.Asc(apikey.FieldID)).All(ctx)
}
func (s *Service) loadGroups(ctx context.Context, kind string, projectID int, ids map[int]struct{}) ([]*ent.APIKeyProfileTemplate, error) {
	q := s.ent.APIKeyProfileTemplate.Query().Where(apikeyprofiletemplate.ProjectIDEQ(projectID))
	if kind != "snapshot" {
		if len(ids) == 0 {
			return []*ent.APIKeyProfileTemplate{}, nil
		}
		q = q.Where(apikeyprofiletemplate.IDIn(mapKeys(ids)...))
	}
	return q.Order(ent.Asc(apikeyprofiletemplate.FieldID)).All(ctx)
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
		"provisioningSource": row.ProvisioningSource, "profileMode": row.ProfileMode,
		"accessGroupId": row.AccessGroupID, "accessGroupRevision": row.AccessGroupRevision,
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

func (s *Service) accessGroupRecord(ctx context.Context, row *ent.APIKeyProfileTemplate, referenced map[int64]struct{}) (map[string]any, error) {
	attached, err := s.ent.APIKey.Query().Where(apikey.AccessGroupIDEQ(row.ID)).Count(ctx)
	if err != nil {
		return nil, err
	}
	revisions := []any{}
	if len(referenced) > 0 {
		values := make([]int64, 0, len(referenced))
		for revision := range referenced {
			if revision < row.Revision {
				values = append(values, revision)
			}
		}
		if len(values) > 0 {
			sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
			rows, qerr := s.ent.APIKeyProfileTemplateRevision.Query().Where(apikeyprofiletemplaterevision.TemplateIDEQ(row.ID), apikeyprofiletemplaterevision.RevisionIn(values...)).Order(ent.Asc(apikeyprofiletemplaterevision.FieldRevision)).All(ctx)
			if qerr != nil {
				return nil, qerr
			}
			for _, revision := range rows {
				revisions = append(revisions, map[string]any{"revision": revision.Revision, "name": revision.Name, "description": revision.Description, "profile": profileForContract(revision.Profile), "createdAt": revision.CreatedAt, "createdByUserId": revision.CreatedByUserID})
			}
		}
	}
	return map[string]any{
		"id": row.ID, "projectId": row.ProjectID, "name": row.Name, "description": row.Description,
		"profile": profileForContract(row.Profile), "revision": row.Revision, "selfServiceVisible": row.SelfServiceVisible,
		"attachedKeyCount": attached, "createdAt": row.CreatedAt, "updatedAt": row.UpdatedAt, "referencedRevisions": revisions,
	}, nil
}
func (s *Service) configurationRecord(ctx context.Context, credentials bool, storageIDs map[int]struct{}) (map[string]any, error) {
	registration, _ := s.systems.RegistrationConfig(ctx, biz.RegistrationConfig{})
	invite := Credential{Status: "excluded"}
	if credentials {
		invite = Credential{Status: "included", Value: registration.InviteCode}
	}
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
		"registration": map[string]any{"enabled": registration.Enabled, "oidcEnabled": registration.OIDCEnabled, "defaultProjectId": registration.DefaultProjectID, "autoJoinFirstProject": registration.AutoJoinFirstProject, "defaultProjectScopes": nonNilSlice(registration.DefaultProjectScopes), "selfServiceEnabled": registration.SelfServiceEnabled, "allowRequestDetails": registration.AllowRequestDetails, "inviteCode": invite},
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
