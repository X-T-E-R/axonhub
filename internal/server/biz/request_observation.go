package biz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xjson"
	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

const forwardingObservationBodyFailureClass = "forwarding_observation_unavailable"

type requestObservationCreatedAtKey struct{}

type requestObservationContextSnapshotKey struct{}

type requestObservationBodyInfoKey struct{}

type requestObservationResponseBodyInfoKey struct{}

type requestObservationIDKey struct{}

type requestObservationProfilesKey struct{}

type requestObservationBodyInfo struct {
	Unavailable bool
	ByteLength  int64
	SHA256      string
}

type requestObservationContextSnapshot struct {
	projectID        int
	hasProjectID     bool
	source           request.Source
	hasSource        bool
	apiKey           *ent.APIKey
	hasAPIKey        bool
	trace            *ent.Trace
	hasTrace         bool
	channelAPIKey    string
	hasChannelAPIKey bool
}

func withObservationCreatedAt(ctx context.Context, capturedAt time.Time) context.Context {
	return context.WithValue(ctx, requestObservationCreatedAtKey{}, capturedAt)
}

func observationCreatedAt(ctx context.Context) (time.Time, bool) {
	capturedAt, ok := ctx.Value(requestObservationCreatedAtKey{}).(time.Time)
	return capturedAt, ok && !capturedAt.IsZero()
}

func observationBodyInfoFromContext(ctx context.Context) *requestObservationBodyInfo {
	info, _ := ctx.Value(requestObservationBodyInfoKey{}).(*requestObservationBodyInfo)
	return info
}

func withObservationBodyInfo(ctx context.Context, info *requestObservationBodyInfo) context.Context {
	if info == nil {
		return ctx
	}
	clone := *info
	return context.WithValue(ctx, requestObservationBodyInfoKey{}, &clone)
}

func withObservationResponseBodyInfo(ctx context.Context, info *requestObservationBodyInfo) context.Context {
	if info == nil {
		return ctx
	}
	clone := *info
	return context.WithValue(ctx, requestObservationResponseBodyInfoKey{}, &clone)
}

func observationResponseBodyInfoFromContext(ctx context.Context) *requestObservationBodyInfo {
	info, _ := ctx.Value(requestObservationResponseBodyInfoKey{}).(*requestObservationBodyInfo)
	return info
}

func withObservationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestObservationIDKey{}, id)
}

func observationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestObservationIDKey{}).(string)
	return id
}

func observationContextSnapshotFromContext(ctx context.Context) *requestObservationContextSnapshot {
	snapshot, _ := ctx.Value(requestObservationContextSnapshotKey{}).(*requestObservationContextSnapshot)
	return snapshot
}

func captureObservationContext(ctx context.Context) context.Context {
	if existing := observationContextSnapshotFromContext(ctx); existing != nil {
		return context.WithValue(ctx, requestObservationContextSnapshotKey{}, cloneObservationContextSnapshot(existing))
	}
	snapshot := &requestObservationContextSnapshot{}

	if projectID, ok := contexts.GetProjectID(ctx); ok {
		snapshot.projectID = projectID
		snapshot.hasProjectID = true
	}
	if source, ok := contexts.GetSource(ctx); ok {
		snapshot.source = source
		snapshot.hasSource = true
	}
	if apiKey, ok := contexts.GetAPIKey(ctx); ok {
		snapshot.apiKey = cloneObservationAPIKey(apiKey)
		snapshot.hasAPIKey = true
	}
	if trace, ok := contexts.GetTrace(ctx); ok {
		snapshot.trace = cloneObservationTrace(trace)
		snapshot.hasTrace = true
	}
	if channelAPIKey, ok := contexts.GetChannelAPIKey(ctx); ok {
		snapshot.channelAPIKey = channelAPIKey
		snapshot.hasChannelAPIKey = true
	}

	return context.WithValue(ctx, requestObservationContextSnapshotKey{}, snapshot)
}

// A request context contains the pipeline container and its request/response
// buffers. Keeping its parent would bypass queue byte accounting even when the
// visible job snapshot was compact. Retain only persistence input values.
func detachedObservationContext(ctx context.Context) context.Context {
	captured := captureObservationContext(ctx)
	snapshot := observationContextSnapshotFromContext(captured)
	if keep, _ := ctx.Value(requestObservationProfilesKey{}).(bool); !keep && snapshot.apiKey != nil {
		snapshot.apiKey.Profiles = nil
	}
	base := context.Background()
	if client := ent.FromContext(ctx); client != nil {
		base = ent.NewContext(base, client)
	}
	base = context.WithValue(base, requestObservationContextSnapshotKey{}, snapshot)
	if snapshot.hasProjectID {
		base = contexts.WithProjectID(base, snapshot.projectID)
	}
	if snapshot.hasAPIKey {
		base = contexts.WithAPIKey(base, snapshot.apiKey)
	}
	if snapshot.hasSource {
		base = contexts.WithSource(base, snapshot.source)
	}
	if snapshot.hasTrace {
		base = contexts.WithTrace(base, snapshot.trace)
	}
	if snapshot.hasChannelAPIKey {
		base = contexts.WithChannelAPIKey(base, snapshot.channelAPIKey)
	}
	for _, key := range []any{requestObservationCreatedAtKey{}, requestObservationBodyInfoKey{}, requestObservationResponseBodyInfoKey{}, requestObservationIDKey{}, requestObservationProfilesKey{}, observationPendingUsageKey{}} {
		if value := ctx.Value(key); value != nil {
			base = context.WithValue(base, key, value)
		}
	}
	return base
}

func cloneObservationContextSnapshot(snapshot *requestObservationContextSnapshot) *requestObservationContextSnapshot {
	if snapshot == nil {
		return nil
	}
	clone := *snapshot
	clone.apiKey = cloneObservationAPIKey(snapshot.apiKey)
	clone.trace = cloneObservationTrace(snapshot.trace)
	return &clone
}

func observationProjectID(ctx context.Context) (int, bool) {
	if snapshot := observationContextSnapshotFromContext(ctx); snapshot != nil {
		return snapshot.projectID, snapshot.hasProjectID
	}
	return contexts.GetProjectID(ctx)
}

func observationSourceOrDefault(ctx context.Context, defaultSource request.Source) request.Source {
	if snapshot := observationContextSnapshotFromContext(ctx); snapshot != nil {
		if snapshot.hasSource {
			return snapshot.source
		}
		return defaultSource
	}
	return contexts.GetSourceOrDefault(ctx, defaultSource)
}

func observationAPIKey(ctx context.Context) (*ent.APIKey, bool) {
	if snapshot := observationContextSnapshotFromContext(ctx); snapshot != nil {
		return snapshot.apiKey, snapshot.hasAPIKey && snapshot.apiKey != nil
	}
	return contexts.GetAPIKey(ctx)
}

func observationTrace(ctx context.Context) (*ent.Trace, bool) {
	if snapshot := observationContextSnapshotFromContext(ctx); snapshot != nil {
		return snapshot.trace, snapshot.hasTrace && snapshot.trace != nil
	}
	return contexts.GetTrace(ctx)
}

func observationChannelAPIKey(ctx context.Context) (string, bool) {
	if snapshot := observationContextSnapshotFromContext(ctx); snapshot != nil {
		return snapshot.channelAPIKey, snapshot.hasChannelAPIKey
	}
	return contexts.GetChannelAPIKey(ctx)
}

func observationWorkerContext(ctx context.Context, scope *observationScope) context.Context {
	snapshot := cloneObservationContextSnapshot(observationContextSnapshotFromContext(ctx))
	if snapshot == nil {
		return ctx
	}
	if snapshot.trace != nil && snapshot.trace.ID < 0 {
		if traceID, err := scope.resolve(snapshot.trace.ID); err == nil {
			snapshot.trace.ID = traceID
		} else {
			snapshot.trace = nil
			snapshot.hasTrace = false
		}
	}
	return context.WithValue(ctx, requestObservationContextSnapshotKey{}, snapshot)
}

func cloneObservationAPIKey(apiKey *ent.APIKey) *ent.APIKey {
	if apiKey == nil {
		return nil
	}
	clone := &ent.APIKey{ID: apiKey.ID, Type: apiKey.Type}
	if apiKey.Profiles != nil {
		profiles := *apiKey.Profiles
		profiles.Profiles = make([]objects.APIKeyProfile, len(apiKey.Profiles.Profiles))
		for i := range apiKey.Profiles.Profiles {
			profile := apiKey.Profiles.Profiles[i].Clone()
			if profile != nil {
				profiles.Profiles[i] = *profile
			}
		}
		clone.Profiles = &profiles
	}
	return clone
}

func cloneObservationTrace(trace *ent.Trace) *ent.Trace {
	if trace == nil {
		return nil
	}
	return &ent.Trace{ID: trace.ID, ProjectID: trace.ProjectID, TraceID: trace.TraceID, ThreadID: trace.ThreadID, CreatedAt: trace.CreatedAt, UpdatedAt: trace.UpdatedAt}
}

func cloneObservationHTTPHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	clone := make(http.Header, len(headers))
	for key, values := range headers {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func cloneObservationHTTPClientRequest(req *httpclient.Request) *httpclient.Request {
	if req == nil {
		return &httpclient.Request{}
	}
	return &httpclient.Request{
		Method:      req.Method,
		URL:         req.URL,
		Path:        req.Path,
		Headers:     cloneObservationHTTPHeaders(req.Headers),
		ContentType: req.ContentType,
		Body:        bytes.Clone(req.Body),
		JSONBody:    bytes.Clone(req.JSONBody),
		ClientIP:    req.ClientIP,
		RequestID:   req.RequestID,
		RequestType: req.RequestType,
		APIFormat:   req.APIFormat,
	}
}

func observationRequestBodyBytes(req *httpclient.Request) []byte {
	if req == nil {
		return nil
	}
	if len(req.JSONBody) > 0 {
		return req.JSONBody
	}
	return req.Body
}

func observationBodyInfo(req *httpclient.Request) *requestObservationBodyInfo {
	body := observationRequestBodyBytes(req)
	return observationBodyInfoFromBytes(body)
}

func observationBodyInfoFromBytes(body []byte) *requestObservationBodyInfo {
	digest := sha256.Sum256(body)
	return &requestObservationBodyInfo{
		ByteLength: int64(len(body)),
		SHA256:     hex.EncodeToString(digest[:]),
	}
}

func observationCapturedAt(ctx context.Context) time.Time {
	if capturedAt, ok := observationCreatedAt(ctx); ok {
		return capturedAt
	}
	return time.Now().UTC()
}

func observationTerminalContext(ctx context.Context, capturedAt time.Time, responseInfo *requestObservationBodyInfo) context.Context {
	ctx = withObservationCreatedAt(captureObservationContext(ctx), capturedAt)
	return withObservationResponseBodyInfo(ctx, responseInfo)
}

func observationAPIKeyHasActiveQuota(ctx context.Context) bool {
	apiKey, ok := observationAPIKey(ctx)
	if !ok || apiKey == nil {
		return false
	}
	profile := apiKey.GetActiveProfile()
	return profile != nil && profile.Quota != nil
}

func observationStableLastChannelCacheKey(ctx context.Context) string {
	projectID, projectOK := observationProjectID(ctx)
	trace, traceOK := observationTrace(ctx)
	if !projectOK || !traceOK || trace == nil || trace.TraceID == "" {
		return ""
	}
	return fmt.Sprintf("last_channel:%d:%s", projectID, trace.TraceID)
}

func (s *RequestService) findObservedRequest(ctx context.Context, projectID int, createdAt time.Time, observationID string) (*ent.Request, error) {
	return s.entFromContext(ctx).Request.Query().Where(
		request.ProjectIDEQ(projectID),
		request.CreatedAtGTE(createdAt.Add(-time.Second)),
		request.CreatedAtLTE(createdAt.Add(time.Second)),
		func(selector *sql.Selector) {
			selector.Where(sqljson.ValueEQ(request.FieldEvidenceDisposition, observationID,
				sqljson.Path("observationId"), sqljson.Unquote(true)))
		},
	).Only(ctx)
}

func (s *RequestService) findObservedRequestExecution(ctx context.Context, requestID int, createdAt time.Time, observationID string) (*ent.RequestExecution, error) {
	return s.entFromContext(ctx).RequestExecution.Query().Where(
		requestexecution.RequestIDEQ(requestID),
		requestexecution.CreatedAtGTE(createdAt.Add(-time.Second)),
		requestexecution.CreatedAtLTE(createdAt.Add(time.Second)),
		func(selector *sql.Selector) {
			selector.Where(sqljson.ValueEQ(requestexecution.FieldEvidenceDisposition, observationID,
				sqljson.Path("observationId"), sqljson.Unquote(true)))
		},
	).Only(ctx)
}

func compactObservationHTTPClientRequest(req *httpclient.Request) {
	if req == nil {
		return
	}
	req.Body = nil
	req.JSONBody = []byte("{}")
	req.Headers = nil
}

func observationHTTPClientRequestBytes(req *httpclient.Request) int64 {
	if req == nil {
		return 0
	}
	var size int64
	size += int64(len(req.Method) + len(req.URL) + len(req.Path) + len(req.ContentType))
	size += int64(len(req.Body) + len(req.JSONBody) + len(req.ClientIP) + len(req.RequestID) + len(req.RequestType) + len(req.APIFormat))
	for key, values := range req.Headers {
		size += int64(len(key))
		for _, value := range values {
			size += int64(len(value))
		}
	}
	return size
}

func cloneObservationRequest(req *ent.Request) *ent.Request {
	if req == nil {
		return &ent.Request{}
	}
	return &ent.Request{
		ID:                   req.ID,
		ProjectID:            req.ProjectID,
		DataStorageID:        req.DataStorageID,
		Stream:               req.Stream,
		ManagedObservability: req.ManagedObservability,
	}
}

func cloneObservationString(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneObservationChannel(channel *Channel) *Channel {
	if channel == nil {
		return &Channel{Channel: &ent.Channel{}}
	}
	clone := &Channel{}
	if channel.Channel != nil {
		clone.Channel = &ent.Channel{ID: channel.ID}
		if channel.Settings != nil {
			settings := &objects.ChannelSettings{}
			if channel.Settings.StoreExecutionRequestBody != nil {
				value := *channel.Settings.StoreExecutionRequestBody
				settings.StoreExecutionRequestBody = &value
			}
			if channel.Settings.StoreExecutionResponseBody != nil {
				value := *channel.Settings.StoreExecutionResponseBody
				settings.StoreExecutionResponseBody = &value
			}
			if channel.Settings.StoreExecutionStreamChunks != nil {
				value := *channel.Settings.StoreExecutionStreamChunks
				settings.StoreExecutionStreamChunks = &value
			}
			clone.Settings = settings
		}
	}
	clone.cachedEnabledAPIKeys = append([]string(nil), channel.cachedEnabledAPIKeys...)
	return clone
}

func cloneObservationLLMRequest(req *llm.Request) *llm.Request {
	if req == nil {
		return &llm.Request{}
	}
	clone := &llm.Request{Model: req.Model, ReasoningEffort: req.ReasoningEffort}
	if req.Stream != nil {
		stream := *req.Stream
		clone.Stream = &stream
	}
	return clone
}

func cloneObservationMetrics(metrics *LatencyMetrics) *LatencyMetrics {
	if metrics == nil {
		return nil
	}
	return &LatencyMetrics{
		LatencyMs:           cloneObservationInt64(metrics.LatencyMs),
		FirstTokenLatencyMs: cloneObservationInt64(metrics.FirstTokenLatencyMs),
		ReasoningDurationMs: cloneObservationInt64(metrics.ReasoningDurationMs),
	}
}

func cloneObservationInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneObservationErrorInfo(info *ExecutionErrorInfo) *ExecutionErrorInfo {
	if info == nil {
		return nil
	}
	return &ExecutionErrorInfo{StatusCode: cloneObservationInt(info.StatusCode), ResponseBody: bytes.Clone(info.ResponseBody)}
}

func cloneObservationInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneObservationStreamChunks(chunks []*httpclient.StreamEvent) []*httpclient.StreamEvent {
	if len(chunks) == 0 {
		return nil
	}
	clone := make([]*httpclient.StreamEvent, len(chunks))
	for i, chunk := range chunks {
		if chunk == nil {
			continue
		}
		clone[i] = &httpclient.StreamEvent{
			LastEventID: chunk.LastEventID,
			Type:        chunk.Type,
			Data:        bytes.Clone(chunk.Data),
			Size:        chunk.Size,
		}
	}
	return clone
}

func observationStreamChunksBytes(chunks []*httpclient.StreamEvent) int64 {
	var size int64
	for _, chunk := range chunks {
		if chunk == nil {
			continue
		}
		size += int64(len(chunk.LastEventID) + len(chunk.Type) + len(chunk.Data))
		size += int64(8)
	}
	return size
}

func observationContextBytes(ctx context.Context) int64 {
	var size int64
	if snapshot := observationContextSnapshotFromContext(ctx); snapshot != nil {
		size += 128
		if keep, _ := ctx.Value(requestObservationProfilesKey{}).(bool); keep && snapshot.apiKey != nil && snapshot.apiKey.Profiles != nil {
			if encoded, err := json.Marshal(snapshot.apiKey.Profiles); err == nil {
				size += int64(len(encoded))
			}
		}
		if snapshot.trace != nil {
			size += int64(len(snapshot.trace.TraceID) + 32)
		}
		size += int64(len(snapshot.channelAPIKey))
	}
	return size
}

func observationChannelBytes(channel *Channel) int64 {
	if channel == nil {
		return 0
	}
	var size int64
	for _, key := range channel.cachedEnabledAPIKeys {
		size += int64(len(key))
	}
	if channel.Settings != nil {
		if encoded, err := json.Marshal(channel.Settings); err == nil {
			size += int64(len(encoded))
		}
	}
	return size
}

func observationPayloadFits(scope *observationScope, payloadBytes int64) bool {
	if scope == nil || scope.writer == nil {
		return false
	}
	maxBytes := int64(scope.writer.config.MaxBytesMiB) << 20
	return payloadBytes+4096 <= maxBytes
}

func compactObservationResponseBody(scope *observationScope, responseBody []byte, payloadBytes int64) ([]byte, *requestObservationBodyInfo, int64) {
	info := observationBodyInfoFromBytes(responseBody)
	if observationPayloadFits(scope, payloadBytes) {
		return responseBody, info, payloadBytes
	}
	info.Unavailable = true
	placeholder := []byte("{}")
	return placeholder, info, payloadBytes - int64(len(responseBody)) + int64(len(placeholder))
}

func observationUnavailableBodyDisposition(info *requestObservationBodyInfo, capturedAt time.Time) objects.Disposition {
	failureClass := forwardingObservationBodyFailureClass
	disposition := objects.Disposition{
		Intent:       "persist",
		Location:     "none",
		Outcome:      "unavailable",
		CapturedAt:   capturedAt,
		FailureClass: &failureClass,
	}
	if info != nil {
		disposition.SHA256 = info.SHA256
		length := info.ByteLength
		disposition.ByteLength = &length
	}
	return disposition
}

func forwardingUnavailableEvidenceDisposition(capturedAt time.Time) *objects.EvidenceDisposition {
	return &objects.EvidenceDisposition{
		Version:        1,
		RequestBody:    observationUnavailableBodyDisposition(nil, capturedAt),
		ResponseBody:   observationUnavailableBodyDisposition(nil, capturedAt),
		ResponseChunks: observationUnavailableBodyDisposition(nil, capturedAt),
	}
}

func observationHeaderJSON(req *httpclient.Request) objects.JSONRawMessage {
	if req == nil || len(req.Headers) == 0 {
		return []byte("{}")
	}
	masked := httpclient.MaskSensitiveHeaders(req.Headers)
	encoded, err := json.Marshal(masked)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

func snapshotObservationValue(value any) ([]byte, error) {
	encoded, err := xjson.Marshal(value)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(encoded), nil
}

func observationErrorInfoBytes(info *ExecutionErrorInfo) int64 {
	if info == nil {
		return 0
	}
	var size int64 = int64(len(info.ResponseBody))
	if info.StatusCode != nil {
		size += 8
	}
	return size
}

// CreateRequest returns a local entity immediately when forwarding persistence
// is scoped. The callback persists the frozen snapshot and binds its ID.
func (s *RequestService) CreateRequest(
	ctx context.Context,
	llmRequest *llm.Request,
	httpRequest *httpclient.Request,
	format llm.APIFormat,
) (*ent.Request, error) {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.createRequest(ctx, llmRequest, httpRequest, format)
	}

	capturedAt := time.Now().UTC()
	llmSnapshot := cloneObservationLLMRequest(llmRequest)
	httpSnapshot := cloneObservationHTTPClientRequest(httpRequest)
	bodyInfo := observationBodyInfo(httpSnapshot)
	payloadBytes := observationHTTPClientRequestBytes(httpSnapshot) + observationContextBytes(ctx) + int64(len(llmSnapshot.Model)+len(llmSnapshot.ReasoningEffort)+len(format)+256)
	if !observationPayloadFits(scope, payloadBytes) {
		bodyInfo.Unavailable = true
		compactObservationHTTPClientRequest(httpSnapshot)
		payloadBytes = observationHTTPClientRequestBytes(httpSnapshot) + observationContextBytes(ctx) + int64(len(llmSnapshot.Model)+len(llmSnapshot.ReasoningEffort)+len(format)+256)
	}

	localID := scope.newID()
	observationID := tracing.GenerateTraceID()
	projectID, _ := observationProjectID(ctx)
	source := observationSourceOrDefault(ctx, request.SourceAPI)
	stream := llmSnapshot.Stream != nil && *llmSnapshot.Stream
	apiKeyID := 0
	if apiKey, ok := observationAPIKey(ctx); ok && apiKey != nil {
		apiKeyID = apiKey.ID
	}
	traceID := 0
	if trace, ok := observationTrace(ctx); ok && trace != nil {
		traceID = trace.ID
	}
	synthetic := &ent.Request{
		ID:                  localID,
		CreatedAt:           capturedAt,
		UpdatedAt:           capturedAt,
		APIKeyID:            apiKeyID,
		ProjectID:           projectID,
		TraceID:             traceID,
		Source:              source,
		ModelID:             llmSnapshot.Model,
		ReasoningEffort:     llmSnapshot.ReasoningEffort,
		Format:              string(format),
		RequestHeaders:      observationHeaderJSON(httpSnapshot),
		RequestBody:         bytes.Clone(observationRequestBodyBytes(httpSnapshot)),
		Status:              request.StatusProcessing,
		Stream:              stream,
		ClientIP:            httpSnapshot.ClientIP,
		EvidenceDisposition: forwardingUnavailableEvidenceDisposition(capturedAt),
	}
	synthetic.EvidenceDisposition.ObservationID = observationID
	if bodyInfo.Unavailable {
		synthetic.RequestBody = []byte("{}")
	}

	queuedCtx := withObservationID(withObservationBodyInfo(withObservationCreatedAt(captureObservationContext(ctx), capturedAt), bodyInfo), observationID)
	queuedCtx = context.WithValue(queuedCtx, requestObservationProfilesKey{}, true)
	submitErr := scope.submitCore(queuedCtx, localID, payloadBytes, func(workerCtx context.Context) error {
		if _, exists := scope.ids[localID]; exists {
			return nil
		}
		workerCtx = observationWorkerContext(workerCtx, scope)
		if observedID := observationIDFromContext(workerCtx); observedID != "" {
			persisted, lookupErr := s.findObservedRequest(workerCtx, projectID, capturedAt, observedID)
			if lookupErr == nil {
				if err := s.resumeObservedRequestBody(workerCtx, persisted, observationRequestBodyBytes(httpSnapshot)); err != nil {
					return err
				}
				scope.bindID(localID, persisted.ID)
				return nil
			}
			if !ent.IsNotFound(lookupErr) {
				return lookupErr
			}
		}
		persisted, err := s.createRequest(workerCtx, llmSnapshot, httpSnapshot, format)
		if err != nil {
			return err
		}
		scope.bindID(localID, persisted.ID)
		return nil
	})
	if submitErr != nil && observationAPIKeyHasActiveQuota(queuedCtx) {
		return nil, errors.New("quota accounting observation admission unavailable")
	}

	return synthetic, nil
}

// CreateRequestExecution returns a local entity immediately when forwarding
// persistence is scoped. Its callback resolves the parent before saving.
func (s *RequestService) CreateRequestExecution(
	ctx context.Context,
	channel *Channel,
	modelID string,
	requestEntity *ent.Request,
	channelRequest httpclient.Request,
	format llm.APIFormat,
	passThroughApplied bool,
) (*ent.RequestExecution, error) {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.createRequestExecution(ctx, channel, modelID, requestEntity, channelRequest, format, passThroughApplied)
	}

	capturedAt := time.Now().UTC()
	requestSnapshot := cloneObservationRequest(requestEntity)
	channelSnapshot := cloneObservationChannel(channel)
	channelRequestSnapshot := cloneObservationHTTPClientRequest(&channelRequest)
	bodyInfo := observationBodyInfo(channelRequestSnapshot)
	payloadBytes := observationHTTPClientRequestBytes(channelRequestSnapshot) + observationContextBytes(ctx) + observationChannelBytes(channelSnapshot) + int64(len(modelID)+len(format)+256)
	if !observationPayloadFits(scope, payloadBytes) {
		bodyInfo.Unavailable = true
		compactObservationHTTPClientRequest(channelRequestSnapshot)
		payloadBytes = observationHTTPClientRequestBytes(channelRequestSnapshot) + observationContextBytes(ctx) + observationChannelBytes(channelSnapshot) + int64(len(modelID)+len(format)+256)
	}

	localID := scope.newID()
	observationID := tracing.GenerateTraceID()
	synthetic := &ent.RequestExecution{
		ID:                  localID,
		CreatedAt:           capturedAt,
		UpdatedAt:           capturedAt,
		ProjectID:           requestSnapshot.ProjectID,
		RequestID:           requestSnapshot.ID,
		ChannelID:           channelSnapshot.ID,
		DataStorageID:       requestSnapshot.DataStorageID,
		ModelID:             modelID,
		Format:              string(format),
		RequestBody:         bytes.Clone(observationRequestBodyBytes(channelRequestSnapshot)),
		RequestHeaders:      observationHeaderJSON(channelRequestSnapshot),
		Status:              requestexecution.StatusProcessing,
		Stream:              requestSnapshot.Stream,
		RequestURL:          channelRequestSnapshot.URL,
		PassThroughApplied:  passThroughApplied,
		EvidenceDisposition: forwardingUnavailableEvidenceDisposition(capturedAt),
	}
	synthetic.EvidenceDisposition.ObservationID = observationID
	if bodyInfo.Unavailable {
		synthetic.RequestBody = []byte("{}")
	}

	queuedCtx := withObservationID(withObservationBodyInfo(withObservationCreatedAt(captureObservationContext(ctx), capturedAt), bodyInfo), observationID)
	submitErr := scope.submitCore(queuedCtx, localID, payloadBytes, func(workerCtx context.Context) error {
		if _, exists := scope.ids[localID]; exists {
			return nil
		}
		workerCtx = observationWorkerContext(workerCtx, scope)
		parentID, err := scope.resolve(requestSnapshot.ID)
		if err != nil {
			return err
		}
		if observedID := observationIDFromContext(workerCtx); observedID != "" {
			persisted, lookupErr := s.findObservedRequestExecution(workerCtx, parentID, capturedAt, observedID)
			if lookupErr == nil {
				if err := s.resumeObservedExecutionBody(workerCtx, persisted, observationRequestBodyBytes(channelRequestSnapshot)); err != nil {
					return err
				}
				scope.bindID(localID, persisted.ID)
				return nil
			}
			if !ent.IsNotFound(lookupErr) {
				return lookupErr
			}
		}
		persistedRequest, loadErr := s.entFromContext(workerCtx).Request.Get(workerCtx, parentID)
		if loadErr != nil {
			persistedRequest = cloneObservationRequest(requestSnapshot)
			persistedRequest.ID = parentID
		}
		persisted, err := s.createRequestExecution(workerCtx, channelSnapshot, modelID, persistedRequest, *channelRequestSnapshot, format, passThroughApplied)
		if err != nil {
			return err
		}
		scope.bindID(localID, persisted.ID)
		return nil
	})
	if submitErr != nil && observationAPIKeyHasActiveQuota(queuedCtx) {
		return nil, errors.New("quota accounting observation admission unavailable")
	}

	return synthetic, nil
}

// UpdateRequestStatusFromErrorDetails freezes error evidence before any SQL.
func (s *RequestService) UpdateRequestStatusFromErrorDetails(ctx context.Context, requestID int, rawErr, requestContextErr error, errorInfo *ExecutionErrorInfo) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestStatusFromErrorDetails(ctx, requestID, rawErr, requestContextErr, errorInfo)
	}
	if isCallerCancellation(rawErr, requestContextErr) {
		return s.updateRequestStatus(ctx, requestID, request.StatusCanceled, false)
	}
	causalFailure := !errors.Is(rawErr, context.Canceled)
	if errorInfo == nil || len(errorInfo.ResponseBody) == 0 {
		return s.updateRequestStatus(ctx, requestID, request.StatusFailed, causalFailure)
	}
	body := bytes.Clone(errorInfo.ResponseBody)
	payloadBytes := int64(len(body)+64) + observationContextBytes(ctx)
	body, info, payloadBytes := compactObservationResponseBody(scope, body, payloadBytes)
	return scope.submitTerminalPayload(observationTerminalContext(ctx, time.Now().UTC(), info), requestID, payloadBytes, func() int64 {
		payloadBytes -= int64(len(body))
		body = nil
		info.Unavailable = true
		return payloadBytes
	}, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		id, err := scope.resolve(requestID)
		if err != nil {
			return err
		}
		if frozenInfo := observationResponseBodyInfoFromContext(workerCtx); frozenInfo != nil && frozenInfo.Unavailable {
			return s.updateRequestStatusForUnavailableResponseEvidence(workerCtx, id, causalFailure)
		}
		frozenErr := errors.New("forwarding failed")
		if !causalFailure {
			frozenErr = context.Canceled
		}
		return s.updateRequestStatusFromErrorDetails(workerCtx, id, frozenErr, nil, &ExecutionErrorInfo{ResponseBody: body})
	})
}

// UpdateRequestCompleted persists completion through the scoped writer.
func (s *RequestService) UpdateRequestCompleted(ctx context.Context, requestID int, externalID string, responseBody any, metrics *LatencyMetrics) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestCompleted(ctx, requestID, externalID, responseBody, metrics)
	}
	responseSnapshot, err := snapshotObservationValue(responseBody)
	if err != nil {
		return err
	}
	metricsSnapshot := cloneObservationMetrics(metrics)
	payloadBytes := int64(len(responseSnapshot)+len(externalID)+64) + observationContextBytes(ctx)
	capturedAt := time.Now().UTC()
	responseSnapshot, responseInfo, payloadBytes := compactObservationResponseBody(scope, responseSnapshot, payloadBytes)
	queuedCtx := observationTerminalContext(ctx, capturedAt, responseInfo)
	return scope.submitTerminalPayload(queuedCtx, requestID, payloadBytes, func() int64 {
		payloadBytes -= int64(len(responseSnapshot))
		responseSnapshot = nil
		responseInfo.Unavailable = true
		return payloadBytes
	}, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(requestID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestCompleted(workerCtx, resolvedID, externalID, responseSnapshot, metricsSnapshot)
	})
}

// UpdateRequestCompletedWithAudio persists completion and the frozen audio bytes.
func (s *RequestService) UpdateRequestCompletedWithAudio(ctx context.Context, requestID int, externalID string, responseBody any, audio []byte, filename string, metrics *LatencyMetrics) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestCompletedWithAudio(ctx, requestID, externalID, responseBody, audio, filename, metrics)
	}
	responseSnapshot, err := snapshotObservationValue(responseBody)
	if err != nil {
		return err
	}
	audioSnapshot := bytes.Clone(audio)
	metricsSnapshot := cloneObservationMetrics(metrics)
	payloadBytes := int64(len(responseSnapshot)+len(audioSnapshot)+len(externalID)+len(filename)+64) + observationContextBytes(ctx)
	capturedAt := time.Now().UTC()
	audioBytes := int64(len(audioSnapshot))
	responseSnapshot, responseInfo, payloadBytes := compactObservationResponseBody(scope, responseSnapshot, payloadBytes)
	if responseInfo.Unavailable {
		audioSnapshot = nil
		payloadBytes -= audioBytes
	}
	return scope.submitTerminalPayload(observationTerminalContext(ctx, capturedAt, responseInfo), requestID, payloadBytes, func() int64 {
		payloadBytes -= int64(len(responseSnapshot) + len(audioSnapshot))
		responseSnapshot, audioSnapshot = nil, nil
		responseInfo.Unavailable = true
		return payloadBytes
	}, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(requestID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestCompletedWithAudio(workerCtx, resolvedID, externalID, responseSnapshot, audioSnapshot, filename, metricsSnapshot)
	})
}

// UpdateRequestStatusExternalIDAndResponseBody persists a request state update.
func (s *RequestService) UpdateRequestStatusExternalIDAndResponseBody(ctx context.Context, requestID int, status request.Status, externalID string, responseBody any, metrics *LatencyMetrics) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestStatusExternalIDAndResponseBodyWithDeadline(ctx, requestID, status, externalID, responseBody, metrics)
	}
	responseSnapshot, err := snapshotObservationValue(responseBody)
	if err != nil {
		return err
	}
	metricsSnapshot := cloneObservationMetrics(metrics)
	payloadBytes := int64(len(responseSnapshot)+len(externalID)+64) + observationContextBytes(ctx)
	capturedAt := time.Now().UTC()
	responseSnapshot, responseInfo, payloadBytes := compactObservationResponseBody(scope, responseSnapshot, payloadBytes)
	return scope.submitTerminalPayload(observationTerminalContext(ctx, capturedAt, responseInfo), requestID, payloadBytes, func() int64 {
		payloadBytes -= int64(len(responseSnapshot))
		responseSnapshot = nil
		responseInfo.Unavailable = true
		return payloadBytes
	}, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(requestID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestStatusExternalIDAndResponseBodyWithDeadline(workerCtx, resolvedID, status, externalID, responseSnapshot, metricsSnapshot)
	})
}

// UpdateRequestExecutionCompletedForChannel persists execution completion with
// the selected channel's storage policy snapshot.
func (s *RequestService) UpdateRequestExecutionCompletedForChannel(ctx context.Context, executionID int, externalID string, responseBody any, metrics *LatencyMetrics, channel *Channel) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestExecutionCompletedForChannel(ctx, executionID, externalID, responseBody, metrics, channel)
	}
	responseSnapshot, err := snapshotObservationValue(responseBody)
	if err != nil {
		return err
	}
	metricsSnapshot := cloneObservationMetrics(metrics)
	channelSnapshot := cloneObservationChannel(channel)
	payloadBytes := int64(len(responseSnapshot)+len(externalID)+64) + observationContextBytes(ctx) + observationChannelBytes(channelSnapshot)
	capturedAt := time.Now().UTC()
	responseSnapshot, responseInfo, payloadBytes := compactObservationResponseBody(scope, responseSnapshot, payloadBytes)
	return scope.submitTerminalPayload(observationTerminalContext(ctx, capturedAt, responseInfo), executionID, payloadBytes, func() int64 {
		payloadBytes -= int64(len(responseSnapshot))
		responseSnapshot = nil
		responseInfo.Unavailable = true
		return payloadBytes
	}, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(executionID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestExecutionCompletedForChannel(workerCtx, resolvedID, externalID, responseSnapshot, metricsSnapshot, channelSnapshot)
	})
}

// UpdateRequestExecutionCompletedWithAggregationIncomplete persists the
// successful terminal state with explicit aggregation evidence.
func (s *RequestService) UpdateRequestExecutionCompletedWithAggregationIncomplete(ctx context.Context, executionID int, metrics *LatencyMetrics) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestExecutionCompletedWithAggregationIncomplete(ctx, executionID, metrics)
	}
	metricsSnapshot := cloneObservationMetrics(metrics)
	payloadBytes := int64(64) + observationContextBytes(ctx)
	capturedAt := time.Now().UTC()
	return scope.submitTerminal(observationTerminalContext(ctx, capturedAt, nil), executionID, payloadBytes, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(executionID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestExecutionCompletedWithAggregationIncomplete(workerCtx, resolvedID, metricsSnapshot)
	})
}

// updateRequestExecutionStatus is the shared persistence choke point for all
// execution terminal/status helpers.
func (s *RequestService) updateRequestExecutionStatus(ctx context.Context, executionID int, status requestexecution.Status, errorMsg string, errorInfo *ExecutionErrorInfo, causalFailure bool, metrics *LatencyMetrics) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestExecutionStatusSync(ctx, executionID, status, errorMsg, errorInfo, causalFailure, metrics)
	}
	errorInfoSnapshot := cloneObservationErrorInfo(errorInfo)
	metricsSnapshot := cloneObservationMetrics(metrics)
	payloadBytes := int64(len(errorMsg)+64) + observationErrorInfoBytes(errorInfoSnapshot) + observationContextBytes(ctx)
	capturedAt := time.Now().UTC()
	var responseInfo *requestObservationBodyInfo
	if errorInfoSnapshot != nil {
		compactedBody, bodyInfo, compactedPayload := compactObservationResponseBody(scope, errorInfoSnapshot.ResponseBody, payloadBytes)
		responseInfo = bodyInfo
		if responseInfo.Unavailable {
			errorInfoSnapshot.ResponseBody = compactedBody
			payloadBytes = compactedPayload
		}
	}
	return scope.submitTerminalPayload(observationTerminalContext(ctx, capturedAt, responseInfo), executionID, payloadBytes, func() int64 {
		if errorInfoSnapshot != nil {
			payloadBytes -= int64(len(errorInfoSnapshot.ResponseBody))
			errorInfoSnapshot.ResponseBody = nil
			responseInfo.Unavailable = true
		}
		return payloadBytes
	}, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(executionID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestExecutionStatusSync(workerCtx, resolvedID, status, errorMsg, errorInfoSnapshot, causalFailure, metricsSnapshot)
	})
}

// SaveRequestExecutionChunksForChannel queues a frozen chunk set.
func (s *RequestService) SaveRequestExecutionChunksForChannel(ctx context.Context, executionID int, chunks []*httpclient.StreamEvent, channel *Channel) error {
	if len(chunks) == 0 {
		return nil
	}
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.saveRequestExecutionChunksForChannel(ctx, executionID, chunks, channel)
	}
	chunksSnapshot := cloneObservationStreamChunks(chunks)
	channelSnapshot := cloneObservationChannel(channel)
	payloadBytes := observationStreamChunksBytes(chunksSnapshot) + observationContextBytes(ctx) + observationChannelBytes(channelSnapshot) + 64
	return scope.submit(captureObservationContext(ctx), payloadBytes, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(executionID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.saveRequestExecutionChunksForChannel(workerCtx, resolvedID, chunksSnapshot, channelSnapshot)
	})
}

// SaveRequestChunks queues a frozen request chunk set.
func (s *RequestService) SaveRequestChunks(ctx context.Context, requestID int, chunks []*httpclient.StreamEvent) error {
	if len(chunks) == 0 {
		return nil
	}
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.saveRequestChunks(ctx, requestID, chunks)
	}
	chunksSnapshot := cloneObservationStreamChunks(chunks)
	payloadBytes := observationStreamChunksBytes(chunksSnapshot) + observationContextBytes(ctx) + 64
	return scope.submit(captureObservationContext(ctx), payloadBytes, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(requestID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.saveRequestChunks(workerCtx, resolvedID, chunksSnapshot)
	})
}

// updateRequestStatus is the shared persistence choke point for request status
// helpers and aliases.
func (s *RequestService) updateRequestStatus(ctx context.Context, requestID int, status request.Status, causalFailure bool) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestStatusSync(ctx, requestID, status, causalFailure)
	}
	payloadBytes := int64(64) + observationContextBytes(ctx)
	capturedAt := time.Now().UTC()
	return scope.submitTerminal(observationTerminalContext(ctx, capturedAt, nil), requestID, payloadBytes, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(requestID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestStatusSync(workerCtx, resolvedID, status, causalFailure)
	})
}

// UpdateRequestChannelID queues the channel assignment for a synthetic request.
func (s *RequestService) UpdateRequestChannelID(ctx context.Context, requestID int, channelID int) error {
	scope := observationFromContext(ctx)
	if scope == nil {
		return s.updateRequestChannelID(ctx, requestID, channelID)
	}
	payloadBytes := int64(64) + observationContextBytes(ctx)
	return scope.submit(captureObservationContext(ctx), payloadBytes, func(workerCtx context.Context) error {
		workerCtx = observationWorkerContext(workerCtx, scope)
		resolvedID, resolveErr := scope.resolve(requestID)
		if resolveErr != nil {
			return resolveErr
		}
		return s.updateRequestChannelID(workerCtx, resolvedID, channelID)
	})
}
