//nolint:nilerr // Checked.
package biz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/eko/gocache/lib/v4/store"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xjson"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// RequestService handles request and request execution operations.
type RequestService struct {
	*AbstractService

	SystemService      *SystemService
	UsageLogService    *UsageLogService
	DataStorageService *DataStorageService
	LiveStreamRegistry *LiveStreamRegistry
	channelCache       xcache.Cache[int]
}

// NewRequestService creates a new RequestService.
func NewRequestService(ent *ent.Client, systemService *SystemService, usageLogService *UsageLogService, dataStorageService *DataStorageService, liveStreamRegistry *LiveStreamRegistry) *RequestService {
	return &RequestService{
		AbstractService: &AbstractService{
			db: ent,
		},
		SystemService:      systemService,
		UsageLogService:    usageLogService,
		DataStorageService: dataStorageService,
		LiveStreamRegistry: liveStreamRegistry,
		channelCache: xcache.NewFromConfig[int](xcache.Config{
			Mode: xcache.ModeMemory,
			Memory: xcache.MemoryConfig{
				Expiration: 30 * time.Minute,
			},
		}),
	}
}

// shouldUseExternalStorage checks if data should be saved to external storage.
// Returns true if the data storage is not primary (database).
func (s *RequestService) shouldUseExternalStorage(_ context.Context, ds *ent.DataStorage) bool {
	if ds == nil {
		return false
	}

	return !ds.Primary
}

func (s *RequestService) shouldStoreExecutionRequestBody(ctx context.Context, channel *Channel) bool {
	if channel != nil && channel.Settings != nil && channel.Settings.StoreExecutionRequestBody != nil {
		return *channel.Settings.StoreExecutionRequestBody
	}

	storeRequestBody := true
	if policy, err := s.SystemService.StoragePolicy(ctx); err == nil {
		storeRequestBody = policy.StoreRequestBody
		if policy.StoreExecutionRequestBody != nil {
			storeRequestBody = *policy.StoreExecutionRequestBody
		}
	} else {
		log.Warn(ctx, "Failed to get storage policy, defaulting to store request body", log.Cause(err))
	}

	return storeRequestBody
}

func (s *RequestService) shouldStoreExecutionResponseBody(ctx context.Context, execution *ent.RequestExecution, channel *Channel) bool {
	settings := s.getExecutionChannelSettings(ctx, execution, channel)
	if settings != nil && settings.StoreExecutionResponseBody != nil {
		return *settings.StoreExecutionResponseBody
	}

	storeResponseBody := true
	if policy, err := s.SystemService.StoragePolicy(ctx); err == nil {
		storeResponseBody = policy.StoreResponseBody
	} else {
		log.Warn(ctx, "Failed to get storage policy, defaulting to store response body", log.Cause(err))
	}

	return storeResponseBody
}

func (s *RequestService) shouldStoreExecutionStreamChunks(ctx context.Context, execution *ent.RequestExecution, channel *Channel) bool {
	settings := s.getExecutionChannelSettings(ctx, execution, channel)
	if settings != nil && settings.StoreExecutionStreamChunks != nil {
		return *settings.StoreExecutionStreamChunks
	}

	storeChunks, err := s.SystemService.StoreChunks(ctx)
	if err != nil {
		log.Warn(ctx, "Failed to get StoreChunks setting, defaulting to false", log.Cause(err))
		storeChunks = false
	}

	return storeChunks
}

func (s *RequestService) getExecutionChannelSettings(ctx context.Context, execution *ent.RequestExecution, channel *Channel) *objects.ChannelSettings {
	if channel != nil && channel.Channel != nil && channel.ID != 0 && channel.Settings != nil {
		return channel.Settings
	}
	if execution == nil || execution.ChannelID == 0 {
		return nil
	}

	bypassCtx := authz.WithSystemBypass(ctx, "request-execution-channel-storage-settings")
	storedChannel, err := s.entFromContext(bypassCtx).Channel.Get(bypassCtx, execution.ChannelID)
	if err != nil {
		log.Warn(ctx, "Failed to get execution channel storage settings", log.Int("channel_id", execution.ChannelID), log.Cause(err))
		return nil
	}

	return storedChannel.Settings
}

// _InvalidRequestBodyJSON returns a JSON object indicating invalid text.
var _InvalidRequestBodyJSON = objects.JSONRawMessage(`{"message":"invalid text"}`)

// GenerateRequestBodyKey generates the storage key for request body.
func GenerateRequestBodyKey(projectID, requestID int) string {
	return fmt.Sprintf("/%d/requests/%d/request_body.json", projectID, requestID)
}

// GenerateResponseBodyKey generates the storage key for response body.
func GenerateResponseBodyKey(projectID, requestID int) string {
	return fmt.Sprintf("/%d/requests/%d/response_body.json", projectID, requestID)
}

// GenerateAudioKey generates the storage key for a generated audio file (TTS).
func GenerateAudioKey(projectID, requestID int, filename string) string {
	name := strings.TrimSpace(filename)
	if name == "" {
		name = "audio.mp3"
	}

	name = filepath.Base(name)

	return fmt.Sprintf("/%d/requests/%d/audio/%s", projectID, requestID, name)
}

// GenerateResponseChunksKey generates the storage key for response chunks.
func GenerateResponseChunksKey(projectID, requestID int) string {
	return fmt.Sprintf("/%d/requests/%d/response_chunks.json", projectID, requestID)
}

// GenerateRequestDirKey generates the storage key for request.
func GenerateRequestDirKey(projectID, requestID int) string {
	return fmt.Sprintf("/%d/requests/%d", projectID, requestID)
}

func GenerateRequestExecutionsDirKey(projectID, requestID int) string {
	return fmt.Sprintf("/%d/requests/%d/executions", projectID, requestID)
}

// GenerateExecutionRequestBodyKey generates the storage key for execution request body.
func GenerateExecutionRequestBodyKey(projectID, requestID, executionID int) string {
	return fmt.Sprintf("/%d/requests/%d/executions/%d/request_body.json", projectID, requestID, executionID)
}

// GenerateExecutionResponseBodyKey generates the storage key for execution response body.
func GenerateExecutionResponseBodyKey(projectID, requestID, executionID int) string {
	return fmt.Sprintf("/%d/requests/%d/executions/%d/response_body.json", projectID, requestID, executionID)
}

// GenerateExecutionResponseChunksKey generates the storage key for execution response chunks.
func GenerateExecutionResponseChunksKey(projectID, requestID, executionID int) string {
	return fmt.Sprintf("/%d/requests/%d/executions/%d/response_chunks.json", projectID, requestID, executionID)
}

// GenerateExecutionRequestDirKey generates the storage key for execution request.
func GenerateExecutionRequestDirKey(projectID, requestID, executionID int) string {
	return fmt.Sprintf("/%d/requests/%d/executions/%d", projectID, requestID, executionID)
}

// CreateRequest creates a new request record.
func (s *RequestService) CreateRequest(
	ctx context.Context,
	llmRequest *llm.Request,
	httpRequest *httpclient.Request,
	format llm.APIFormat,
) (*ent.Request, error) {
	// Get project ID from context.
	// If project ID is not found, use zero.
	// It will be not prsent in the admin pages,
	// e.g: test channel.
	projectID, _ := contexts.GetProjectID(ctx)

	// Decide whether to store the original request body
	storeRequestBody := true
	capacityManaged := false
	if policy, err := s.SystemService.StoragePolicy(ctx); err == nil {
		storeRequestBody = policy.StoreRequestBody
		_, _, capacityManaged = capacityBytes(policy)
	} else {
		log.Warn(ctx, "Failed to get storage policy, defaulting to store request body", log.Cause(err))
	}

	var (
		requestBodyBytes    objects.JSONRawMessage = []byte("{}")
		requestHeadersBytes objects.JSONRawMessage = []byte("{}")
	)

	if storeRequestBody {
		if len(httpRequest.JSONBody) > 0 {
			requestBodyBytes = httpRequest.JSONBody
		} else {
			b, err := xjson.Marshal(httpRequest.Body)
			if err != nil {
				log.Error(ctx, "Failed to serialize request body", log.Cause(err))
				return nil, err
			}

			requestBodyBytes = b
		}

		if httpRequest != nil && len(httpRequest.Headers) > 0 {
			requestHeadersBytes, _ = xjson.Marshal(httpclient.MaskSensitiveHeaders(httpRequest.Headers))
		}
	} // else keep nil -> stored as JSON null

	isStream := false
	if llmRequest.Stream != nil {
		isStream = *llmRequest.Stream
	}

	// Get default data storage
	dataStorage, err := s.DataStorageService.GetDefaultDataStorage(ctx)
	if err != nil {
		log.Warn(ctx, "Failed to get default data storage, request will be created without data storage", log.Cause(err))
	}
	// Managed payloads are used for primary-database request bodies. Existing
	// non-primary external-storage keys retain their compatibility path.
	useExternalStorage := storeRequestBody && s.shouldUseExternalStorage(ctx, dataStorage)
	useManagedStorage := storeRequestBody && !useExternalStorage
	managedGroup := useManagedStorage || capacityManaged
	if capacityManaged && len(requestHeadersBytes) > 2 {
		_, admitted, _ := s.admitManagedDatabaseEvidence(ctx, "request_headers", int64(len(requestHeadersBytes)))
		if !admitted {
			requestHeadersBytes = []byte("{}")
		}
	}

	client := s.entFromContext(ctx)
	mut := client.Request.Create().
		SetProjectID(projectID).
		SetModelID(llmRequest.Model).
		SetFormat(string(format)).
		SetSource(contexts.GetSourceOrDefault(ctx, request.SourceAPI)).
		SetStatus(request.StatusProcessing).
		SetStream(isStream).
		SetRequestHeaders(requestHeadersBytes).
		SetManagedObservability(managedGroup)

	now := time.Now().UTC()
	disposition := &objects.EvidenceDisposition{Version: 1,
		RequestBody:    objects.Disposition{Intent: "persist", Location: "database", Outcome: "stored", CapturedAt: now},
		ResponseBody:   objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
		ResponseChunks: objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
	}
	if !storeRequestBody {
		disposition.RequestBody = objects.Disposition{Intent: "omit", Location: "none", Outcome: "omitted", CapturedAt: now}
	}
	mut = mut.SetEvidenceDisposition(disposition)

	if httpRequest != nil {
		mut = mut.SetClientIP(httpRequest.ClientIP)
	}

	if llmRequest.ReasoningEffort != "" {
		mut = mut.SetReasoningEffort(llmRequest.ReasoningEffort)
	}

	if useExternalStorage {
		disposition.RequestBody.Location = "external"
		if dataStorage != nil {
			disposition.RequestBody.StorageID = &dataStorage.ID
		}
		mut = mut.SetEvidenceDisposition(disposition)
	}

	if useExternalStorage || useManagedStorage {
		// Set empty JSON for database, actual data will be in external storage
		mut = mut.SetRequestBody([]byte("{}"))
	} else {
		// Store in database
		mut = mut.SetRequestBody(requestBodyBytes)
	}

	if dataStorage != nil {
		mut = mut.SetDataStorageID(dataStorage.ID)
	}

	if apiKey, ok := contexts.GetAPIKey(ctx); ok && apiKey != nil {
		mut = mut.SetAPIKeyID(apiKey.ID)
		profilesBytes, hashErr := canonicalJSON(apiKey.Profiles)
		if hashErr == nil {
			sum := sha256.Sum256(profilesBytes)
			mut = mut.SetRoutingContext(&objects.RoutingContext{
				Version:                 1,
				APIKeyID:                apiKey.ID,
				APIKeyType:              apiKey.Type.String(),
				ProvisioningSource:      "native",
				ProfileMode:             "inline",
				EffectiveProfiles:       apiKey.Profiles,
				EffectiveProfilesSHA256: hex.EncodeToString(sum[:]),
				RequestedModelID:        llmRequest.Model,
			})
		}
	}

	if trace, ok := contexts.GetTrace(ctx); ok && trace != nil {
		mut = mut.SetTraceID(trace.ID)
	}

	// Create request
	req, err := mut.Save(ctx)
	if err != nil {
		if !useExternalStorage {
			log.Warn(ctx, "Failed to save request body due to error, retrying with placeholder", log.Cause(err))

			mut = mut.SetRequestBody(_InvalidRequestBodyJSON)

			req, err = mut.Save(ctx)
			if err != nil {
				log.Error(ctx, "Failed to save request even with placeholder", log.Cause(err))
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// Save request body to external storage if needed
	if useExternalStorage {
		key := GenerateRequestBodyKey(projectID, req.ID)
		disposition.RequestBody.StorageKey = &key

		err := s.DataStorageService.SaveData(ctx, dataStorage, key, requestBodyBytes)
		if err != nil {
			log.Error(ctx, "Failed to save request body to external storage", log.Cause(err))
			failureClass := "external_write_failed"
			disposition.RequestBody.Outcome = "writeFailed"
			disposition.RequestBody.FailureClass = &failureClass
			// Continue anyway, don't fail the request creation
		} else {
			disposition.RequestBody.Outcome = "stored"
		}
		_, _ = client.Request.UpdateOneID(req.ID).SetEvidenceDisposition(disposition).Save(ctx)
		req.EvidenceDisposition = disposition
	} else if useManagedStorage {
		disposition.RequestBody.Location = "managed"
		setManagedBodyMetadata(&disposition.RequestBody, requestBodyBytes)
		managed, managedErr := s.persistManagedRequestBody(ctx, req.ID, requestBodyBytes)
		switch {
		case managedErr != nil:
			metrics.RecordManagedObservabilityAdmissionSkipped(ctx, "write_failed")
			s.SystemService.RecordManagedObservabilityFailure(ctx, "request_body_lock_or_write", "failed")
			log.Warn(ctx, "Managed request-body persistence failed; forwarding with diagnostic skeleton",
				log.Int("request_id", req.ID), log.Cause(managedErr))
			failureClass := "managed_write_failed"
			disposition.RequestBody.Location = "none"
			disposition.RequestBody.Outcome = "writeFailed"
			disposition.RequestBody.FailureClass = &failureClass
		case managed.skipped:
			metrics.RecordManagedObservabilityAdmissionSkipped(ctx, "capacity_pressure")
			log.Warn(ctx, "Managed request-body admission skipped under capacity pressure",
				log.Int("request_id", req.ID), log.String("signal", "managed_observability_capacity_pressure"))
			failureClass := "capacity_pressure"
			disposition.RequestBody.Location = "none"
			disposition.RequestBody.Outcome = "omitted"
			disposition.RequestBody.FailureClass = &failureClass
		default:
			_, managedErr = client.Request.UpdateOneID(req.ID).
				SetRequestBodyPayloadID(managed.payload.ID).
				SetEvidenceDisposition(disposition).
				Save(ctx)
			if managedErr == nil {
				req.RequestBodyPayloadID = &managed.payload.ID
				req.EvidenceDisposition = disposition
				return req, nil
			}
			failureClass := "managed_reference_write_failed"
			disposition.RequestBody.Location = "none"
			disposition.RequestBody.Outcome = "writeFailed"
			disposition.RequestBody.FailureClass = &failureClass
			log.Warn(ctx, "Managed request-body reference failed; forwarding with diagnostic skeleton",
				log.Int("request_id", req.ID), log.Cause(managedErr))
			if !managed.reused {
				s.discardUnreferencedManagedPayload(ctx, managed.payload.ID)
			}
		}
		_, _ = client.Request.UpdateOneID(req.ID).SetEvidenceDisposition(disposition).Save(ctx)
		req.EvidenceDisposition = disposition
	}

	return req, nil
}

// canonicalJSON normalizes typed values through JSON's data model before the
// final encoding. encoding/json deterministically orders object member names;
// UseNumber avoids an intermediate float conversion for numeric lexemes.
func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	// RFC 8785 uses JSON string escaping rather than encoding/json's
	// optional HTML-safe escapes. APIKeyProfiles contain integer/decimal-string
	// numeric values and fixed ASCII member names, so the remaining encoder
	// behavior matches their JCS representation.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(canonical.Bytes(), []byte("\n")), nil
}

// CreateRequestExecution creates a new request execution record.
func (s *RequestService) CreateRequestExecution(
	ctx context.Context,
	channel *Channel,
	modelID string,
	request *ent.Request,
	channelRequest httpclient.Request,
	format llm.APIFormat,
	passThroughApplied bool,
) (*ent.RequestExecution, error) {
	storeRequestBody := s.shouldStoreExecutionRequestBody(ctx, channel)

	var (
		requestBodyBytes    objects.JSONRawMessage = []byte("{}")
		requestHeadersBytes objects.JSONRawMessage = []byte("{}")
	)

	if storeRequestBody {
		if len(channelRequest.JSONBody) > 0 {
			requestBodyBytes = channelRequest.JSONBody
		} else {
			b, err := xjson.Marshal(channelRequest.Body)
			if err != nil {
				log.Error(ctx, "Failed to marshal request body", log.Cause(err))
				return nil, err
			}

			requestBodyBytes = b
		}

		if len(channelRequest.Headers) > 0 {
			requestHeadersBytes, _ = xjson.Marshal(httpclient.MaskSensitiveHeaders(channelRequest.Headers))
		}
	}

	client := s.entFromContext(ctx)

	// Get data storage if set on request
	var dataStorage *ent.DataStorage

	if request.DataStorageID != 0 {
		var err error

		dataStorage, err = s.DataStorageService.GetDataStorageByID(ctx, request.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage for request execution", log.Cause(err))
		}
	}

	// Determine if we should store in database or external storage
	useExternalStorage := storeRequestBody && s.shouldUseExternalStorage(ctx, dataStorage)
	useManagedStorage := storeRequestBody && !useExternalStorage
	capacityManaged := s.SystemService.ManagedObservabilityCapacityEnabled(ctx)
	managedGroup := useManagedStorage || capacityManaged
	if capacityManaged && len(requestHeadersBytes) > 2 {
		_, admitted, _ := s.admitManagedDatabaseEvidence(ctx, "execution_request_headers", int64(len(requestHeadersBytes)))
		if !admitted {
			requestHeadersBytes = []byte("{}")
		}
	}

	var requestBodyForDB objects.JSONRawMessage
	if useExternalStorage || useManagedStorage {
		// Set empty JSON for database, actual data will be in external storage
		requestBodyForDB = []byte("{}")
	} else {
		// Store in database
		requestBodyForDB = requestBodyBytes
	}

	mut := client.RequestExecution.Create().
		SetFormat(string(format)).
		SetRequestID(request.ID).
		SetProjectID(request.ProjectID).
		SetChannelID(channel.ID).
		SetModelID(modelID).
		SetRequestBody(requestBodyForDB).
		SetStatus(requestexecution.StatusProcessing).
		SetStream(request.Stream).
		SetRequestHeaders(requestHeadersBytes).
		SetPassThroughApplied(passThroughApplied).
		SetManagedObservability(managedGroup)
	now := time.Now().UTC()
	disposition := &objects.EvidenceDisposition{Version: 1,
		RequestBody:    objects.Disposition{Intent: "persist", Location: "database", Outcome: "stored", CapturedAt: now},
		ResponseBody:   objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
		ResponseChunks: objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
	}
	if !storeRequestBody {
		disposition.RequestBody = objects.Disposition{Intent: "omit", Location: "none", Outcome: "omitted", CapturedAt: now}
	} else if useExternalStorage {
		disposition.RequestBody.Location = "external"
		if dataStorage != nil {
			disposition.RequestBody.StorageID = &dataStorage.ID
		}
	}
	mut = mut.SetEvidenceDisposition(disposition)

	if channelRequest.URL != "" {
		mut = mut.SetRequestURL(channelRequest.URL)
	}

	// Use the same data storage as the request
	if request.DataStorageID != 0 {
		mut = mut.SetDataStorageID(request.DataStorageID)
	}

	selectedKeyMasked := selectedChannelAPIKeyMasked(ctx, channel)
	if selectedKeyMasked != "" {
		mut = mut.SetSelectedChannelAPIKeyMasked(selectedKeyMasked)
	}

	execution, err := mut.Save(ctx)
	if err != nil {
		if useExternalStorage {
			return nil, err
		}

		log.Warn(ctx, "Failed to save execution request body due to error, retrying with placeholder", log.Cause(err))

		mut = mut.SetRequestBody(_InvalidRequestBodyJSON)

		execution, err = mut.Save(ctx)
		if err != nil {
			log.Error(ctx, "Failed to save execution request even with placeholder", log.Cause(err))
			return nil, err
		}
	}

	// Save request body to external storage if needed
	if useExternalStorage {
		key := GenerateExecutionRequestBodyKey(request.ProjectID, request.ID, execution.ID)
		disposition.RequestBody.StorageKey = &key

		err := s.DataStorageService.SaveData(ctx, dataStorage, key, requestBodyBytes)
		if err != nil {
			log.Error(ctx, "Failed to save execution request body to external storage", log.Cause(err))
			failureClass := "external_write_failed"
			disposition.RequestBody.Outcome = "writeFailed"
			disposition.RequestBody.FailureClass = &failureClass
			// Continue anyway, don't fail the execution creation
		} else {
			disposition.RequestBody.Outcome = "stored"
		}
		_, _ = client.RequestExecution.UpdateOneID(execution.ID).SetEvidenceDisposition(disposition).Save(ctx)
		execution.EvidenceDisposition = disposition
	} else if useManagedStorage {
		disposition.RequestBody.Location = "managed"
		setManagedBodyMetadata(&disposition.RequestBody, requestBodyBytes)
		managed, managedErr := s.persistManagedRequestBody(ctx, request.ID, requestBodyBytes)
		switch {
		case managedErr != nil:
			metrics.RecordManagedObservabilityAdmissionSkippedComponent(ctx, "write_failed", "execution_request_body")
			s.SystemService.RecordManagedObservabilityFailure(ctx, "execution_request_body_lock_or_write", "failed")
			log.Warn(ctx, "Managed execution request-body persistence failed; forwarding with diagnostic skeleton",
				log.Int("request_id", request.ID), log.Int("execution_id", execution.ID), log.Cause(managedErr))
			failureClass := "managed_write_failed"
			disposition.RequestBody.Location = "none"
			disposition.RequestBody.Outcome = "writeFailed"
			disposition.RequestBody.FailureClass = &failureClass
		case managed.skipped:
			metrics.RecordManagedObservabilityAdmissionSkipped(ctx, "capacity_pressure")
			log.Warn(ctx, "Managed execution request-body admission skipped under capacity pressure",
				log.Int("request_id", request.ID), log.Int("execution_id", execution.ID),
				log.String("signal", "managed_observability_capacity_pressure"))
			failureClass := "capacity_pressure"
			disposition.RequestBody.Location = "none"
			disposition.RequestBody.Outcome = "omitted"
			disposition.RequestBody.FailureClass = &failureClass
		default:
			_, managedErr = client.RequestExecution.UpdateOneID(execution.ID).
				SetRequestBodyPayloadID(managed.payload.ID).
				SetEvidenceDisposition(disposition).
				Save(ctx)
			if managedErr == nil {
				execution.RequestBodyPayloadID = &managed.payload.ID
				execution.EvidenceDisposition = disposition
				break
			}
			failureClass := "managed_reference_write_failed"
			disposition.RequestBody.Location = "none"
			disposition.RequestBody.Outcome = "writeFailed"
			disposition.RequestBody.FailureClass = &failureClass
			log.Warn(ctx, "Managed execution request-body reference failed; forwarding with diagnostic skeleton",
				log.Int("request_id", request.ID), log.Int("execution_id", execution.ID), log.Cause(managedErr))
			if !managed.reused {
				s.discardUnreferencedManagedPayload(ctx, managed.payload.ID)
			}
		}
		if execution.RequestBodyPayloadID == nil {
			_, _ = client.RequestExecution.UpdateOneID(execution.ID).SetEvidenceDisposition(disposition).Save(ctx)
			execution.EvidenceDisposition = disposition
		}
	}
	if managedGroup && !request.ManagedObservability {
		if _, err := client.Request.UpdateOneID(request.ID).SetManagedObservability(true).Save(ctx); err != nil {
			log.Warn(ctx, "Failed to mark request group as managed observability",
				log.Int("request_id", request.ID), log.Cause(err))
		} else {
			request.ManagedObservability = true
		}
	}

	if selectedKeyMasked != "" {
		if _, err := client.Request.UpdateOneID(request.ID).SetSelectedChannelAPIKeyMasked(selectedKeyMasked).Save(ctx); err != nil {
			log.Warn(ctx, "Failed to save selected channel API key mask on request", log.Cause(err), log.Int("request_id", request.ID))
		}
	}

	return execution, nil
}

func selectedChannelAPIKeyMasked(ctx context.Context, channel *Channel) string {
	if key, ok := contexts.GetChannelAPIKey(ctx); ok && key != "" {
		return objects.MaskChannelAPIKey(key)
	}

	if channel == nil {
		return ""
	}

	enabled := channel.GetEnabledAPIKeys()
	if len(enabled) == 1 {
		return objects.MaskChannelAPIKey(enabled[0])
	}

	return ""
}

func evidenceDisposition(intent, location, outcome string, storage *ent.DataStorage, key *string) objects.Disposition {
	disposition := objects.Disposition{Intent: intent, Location: location, Outcome: outcome, CapturedAt: time.Now().UTC(), StorageKey: key}
	if storage != nil {
		disposition.StorageID = &storage.ID
	}
	return disposition
}

func cloneEvidenceDisposition(current *objects.EvidenceDisposition) *objects.EvidenceDisposition {
	if current != nil {
		clone := *current
		return &clone
	}
	failureClass := "legacy_missing_disposition"
	now := time.Now().UTC()
	return &objects.EvidenceDisposition{
		Version:        1,
		RequestBody:    objects.Disposition{Intent: "persist", Location: "none", Outcome: "writeFailed", CapturedAt: now, FailureClass: &failureClass},
		ResponseBody:   objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
		ResponseChunks: objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
	}
}

func managedEvidenceSkippedDisposition(component string, writeFailed bool) objects.Disposition {
	failureClass := "capacity_pressure:" + component
	outcome := "omitted"
	if writeFailed {
		failureClass = "managed_admission_failed:" + component
		outcome = "writeFailed"
	}
	return objects.Disposition{
		Intent:       "persist",
		Location:     "none",
		Outcome:      outcome,
		FailureClass: &failureClass,
		CapturedAt:   time.Now().UTC(),
	}
}

func (s *RequestService) admitManagedDatabaseEvidence(ctx context.Context, component string, byteLength int64) (managed, admitted, writeFailed bool) {
	managed, admitted, err := s.SystemService.AdmitManagedObservabilityEvidence(ctx, component, byteLength)
	if err != nil {
		metrics.RecordManagedObservabilityAdmissionSkippedComponent(ctx, "write_failed", component)
		log.Warn(ctx, "Managed observability evidence admission failed; forwarding with skeleton only",
			log.String("component", component), log.Cause(err))
		return managed, false, true
	}
	if managed && !admitted {
		metrics.RecordManagedObservabilityAdmissionSkippedComponent(ctx, "capacity_pressure", component)
		log.Warn(ctx, "Managed observability evidence skipped under capacity pressure",
			log.String("component", component), log.String("signal", "managed_observability_capacity_pressure"))
	}
	return managed, admitted, false
}

// LatencyMetrics holds latency metrics for a request.
type LatencyMetrics struct {
	LatencyMs           *int64
	FirstTokenLatencyMs *int64
	ReasoningDurationMs *int64
}

// UpdateRequestCompleted updates request status to completed with response body.
func (s *RequestService) UpdateRequestCompleted(
	ctx context.Context,
	requestID int,
	externalId string,
	responseBody any,
	metrics *LatencyMetrics,
) error {
	// Decide whether to store the final response body
	storeResponseBody := true
	if policy, err := s.SystemService.StoragePolicy(ctx); err == nil {
		storeResponseBody = policy.StoreResponseBody
	} else {
		log.Warn(ctx, "Failed to get storage policy, defaulting to store response body", log.Cause(err))
	}

	client := s.entFromContext(ctx)

	// Get the request to check data storage
	req, err := client.Request.Get(ctx, requestID)
	if err != nil {
		log.Error(ctx, "Failed to get request", log.Cause(err))
		return err
	}
	if isTerminalRequestStatus(req.Status) {
		return nil
	}

	// Get data storage if set
	var dataStorage *ent.DataStorage
	if req.DataStorageID != 0 {
		dataStorage, err = s.DataStorageService.GetDataStorageByID(ctx, req.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage", log.Cause(err))
		}
	}

	upd := client.Request.UpdateOneID(requestID).
		Where(request.StatusIn(request.StatusPending, request.StatusProcessing)).
		SetStatus(request.StatusCompleted).
		SetExternalID(externalId)
	disposition := cloneEvidenceDisposition(req.EvidenceDisposition)

	// Set latency metrics if provided
	if metrics != nil {
		if metrics.LatencyMs != nil {
			upd = upd.SetMetricsLatencyMs(*metrics.LatencyMs)
		}

		if metrics.FirstTokenLatencyMs != nil {
			upd = upd.SetMetricsFirstTokenLatencyMs(*metrics.FirstTokenLatencyMs)
		}

		if metrics.ReasoningDurationMs != nil {
			upd = upd.SetMetricsReasoningDurationMs(*metrics.ReasoningDurationMs)
		}
	}

	if storeResponseBody {
		responseBodyBytes, err := xjson.Marshal(responseBody)
		if err != nil {
			log.Error(ctx, "Failed to serialize response body", log.Cause(err))
			return err
		}

		// Check if we should use external storage
		if s.shouldUseExternalStorage(ctx, dataStorage) {
			// Save to external storage
			key := GenerateResponseBodyKey(req.ProjectID, requestID)

			disposition.ResponseBody = evidenceDisposition("persist", "external", "stored", dataStorage, &key)
			err := s.DataStorageService.SaveData(ctx, dataStorage, key, responseBodyBytes)
			if err != nil {
				log.Error(ctx, "Failed to save response body to external storage", log.Cause(err))
				failureClass := "external_write_failed"
				disposition.ResponseBody.Outcome = "writeFailed"
				disposition.ResponseBody.FailureClass = &failureClass
				// Continue anyway
			}
		} else {
			managed, admitted, writeFailed := s.admitManagedDatabaseEvidence(ctx, "request_response_body", int64(len(responseBodyBytes)))
			if managed {
				upd = upd.SetManagedObservability(true)
			}
			if admitted {
				upd = upd.SetResponseBody(responseBodyBytes)
				disposition.ResponseBody = evidenceDisposition("persist", "database", "stored", nil, nil)
			} else {
				disposition.ResponseBody = managedEvidenceSkippedDisposition("request_response_body", writeFailed)
			}
		}
	} else {
		disposition.ResponseBody = evidenceDisposition("omit", "none", "omitted", nil, nil)
	}
	upd = upd.SetEvidenceDisposition(disposition)

	_, err = upd.Save(ctx)
	if err != nil {
		if requestTerminalUpdateWasNoop(ctx, client, requestID, err) {
			return nil
		}

		log.Error(ctx, "Failed to update request status to completed", log.Cause(err))
		return err
	}

	return nil
}

// UpdateRequestCompletedWithAudio marks a request completed and persists a binary audio
// payload (TTS) to external storage when configured.
//
// The audio bytes are never stored in the database column: responseBody carries a compact
// metadata placeholder, and the raw audio is saved to the request's external DataStorage
// (when one is configured and non-primary), tracked via the content_storage_* fields,
// mirroring how video artifacts are stored.
func (s *RequestService) UpdateRequestCompletedWithAudio(
	ctx context.Context,
	requestID int,
	externalId string,
	responseBody any,
	audio []byte,
	filename string,
	metrics *LatencyMetrics,
) error {
	// Decide whether to store the final response body metadata.
	storeResponseBody := true
	if policy, err := s.SystemService.StoragePolicy(ctx); err == nil {
		storeResponseBody = policy.StoreResponseBody
	} else {
		log.Warn(ctx, "Failed to get storage policy, defaulting to store response body", log.Cause(err))
	}

	client := s.entFromContext(ctx)

	req, err := client.Request.Get(ctx, requestID)
	if err != nil {
		log.Error(ctx, "Failed to get request", log.Cause(err))
		return err
	}
	if isTerminalRequestStatus(req.Status) {
		return nil
	}

	var dataStorage *ent.DataStorage
	if req.DataStorageID != 0 {
		dataStorage, err = s.DataStorageService.GetDataStorageByID(ctx, req.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage", log.Cause(err))
		}
	}

	upd := client.Request.UpdateOneID(requestID).
		Where(request.StatusIn(request.StatusPending, request.StatusProcessing)).
		SetStatus(request.StatusCompleted).
		SetExternalID(externalId)
	disposition := cloneEvidenceDisposition(req.EvidenceDisposition)

	if metrics != nil {
		if metrics.LatencyMs != nil {
			upd = upd.SetMetricsLatencyMs(*metrics.LatencyMs)
		}

		if metrics.FirstTokenLatencyMs != nil {
			upd = upd.SetMetricsFirstTokenLatencyMs(*metrics.FirstTokenLatencyMs)
		}

		if metrics.ReasoningDurationMs != nil {
			upd = upd.SetMetricsReasoningDurationMs(*metrics.ReasoningDurationMs)
		}
	}

	if storeResponseBody {
		responseBodyBytes, err := xjson.Marshal(responseBody)
		if err != nil {
			log.Error(ctx, "Failed to serialize response body", log.Cause(err))
			return err
		}

		if s.shouldUseExternalStorage(ctx, dataStorage) {
			key := GenerateResponseBodyKey(req.ProjectID, requestID)
			disposition.ResponseBody = evidenceDisposition("persist", "external", "stored", dataStorage, &key)
			if err := s.DataStorageService.SaveData(ctx, dataStorage, key, responseBodyBytes); err != nil {
				log.Error(ctx, "Failed to save response body to external storage", log.Cause(err))
				failureClass := "external_write_failed"
				disposition.ResponseBody.Outcome = "writeFailed"
				disposition.ResponseBody.FailureClass = &failureClass
			}
		} else {
			managed, admitted, writeFailed := s.admitManagedDatabaseEvidence(ctx, "request_response_body", int64(len(responseBodyBytes)))
			if managed {
				upd = upd.SetManagedObservability(true)
			}
			if admitted {
				upd = upd.SetResponseBody(responseBodyBytes)
				disposition.ResponseBody = evidenceDisposition("persist", "database", "stored", nil, nil)
			} else {
				disposition.ResponseBody = managedEvidenceSkippedDisposition("request_response_body", writeFailed)
			}
		}
	} else {
		disposition.ResponseBody = evidenceDisposition("omit", "none", "omitted", nil, nil)
	}

	// Persist the binary audio to external storage when one is configured.
	if len(audio) > 0 && s.shouldUseExternalStorage(ctx, dataStorage) {
		key := GenerateAudioKey(req.ProjectID, requestID, filename)
		if err := s.DataStorageService.SaveData(ctx, dataStorage, key, audio); err != nil {
			log.Error(ctx, "Failed to save audio to external storage", log.Cause(err))
		} else {
			upd = upd.
				SetContentSaved(true).
				SetContentStorageID(dataStorage.ID).
				SetContentStorageKey(key).
				SetContentSavedAt(time.Now().UTC())
		}
	}

	_, err = upd.SetEvidenceDisposition(disposition).Save(ctx)
	if err != nil {
		if requestTerminalUpdateWasNoop(ctx, client, requestID, err) {
			return nil
		}

		log.Error(ctx, "Failed to update audio request status to completed", log.Cause(err))
		return err
	}

	return nil
}

// UpdateRequestStatusExternalIDAndResponseBody updates request status/external_id and optionally persists response body.
// It is intended for non-pipeline async task flows where task status is polled later.
func (s *RequestService) UpdateRequestStatusExternalIDAndResponseBody(
	ctx context.Context,
	requestID int,
	status request.Status,
	externalId string,
	responseBody any,
	metrics *LatencyMetrics,
) error {
	// Decide whether to store the final response body
	storeResponseBody := true
	if policy, err := s.SystemService.StoragePolicy(ctx); err == nil {
		storeResponseBody = policy.StoreResponseBody
	} else {
		log.Warn(ctx, "Failed to get storage policy, defaulting to store response body", log.Cause(err))
	}

	client := s.entFromContext(ctx)

	// Get the request to check data storage
	req, err := client.Request.Get(ctx, requestID)
	if err != nil {
		log.Error(ctx, "Failed to get request", log.Cause(err))
		return err
	}
	if isTerminalRequestStatus(req.Status) &&
		!(status == request.StatusFailed && req.Status == request.StatusCompleted) {
		return nil
	}

	// Get data storage if set
	var dataStorage *ent.DataStorage
	if req.DataStorageID != 0 {
		dataStorage, err = s.DataStorageService.GetDataStorageByID(ctx, req.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage", log.Cause(err))
		}
	}

	upd := client.Request.UpdateOneID(requestID).SetStatus(status).SetExternalID(externalId)
	active := request.StatusIn(request.StatusPending, request.StatusProcessing)
	if status == request.StatusFailed {
		upd = upd.Where(request.Or(active, request.StatusEQ(request.StatusCompleted)))
	} else {
		upd = upd.Where(active)
	}
	disposition := cloneEvidenceDisposition(req.EvidenceDisposition)

	// Set latency metrics if provided
	if metrics != nil {
		if metrics.LatencyMs != nil {
			upd = upd.SetMetricsLatencyMs(*metrics.LatencyMs)
		}

		if metrics.FirstTokenLatencyMs != nil {
			upd = upd.SetMetricsFirstTokenLatencyMs(*metrics.FirstTokenLatencyMs)
		}

		if metrics.ReasoningDurationMs != nil {
			upd = upd.SetMetricsReasoningDurationMs(*metrics.ReasoningDurationMs)
		}
	}

	if storeResponseBody {
		responseBodyBytes, err := xjson.Marshal(responseBody)
		if err != nil {
			log.Error(ctx, "Failed to serialize response body", log.Cause(err))
			return err
		}

		// Check if we should use external storage
		if s.shouldUseExternalStorage(ctx, dataStorage) {
			// Save to external storage
			key := GenerateResponseBodyKey(req.ProjectID, requestID)

			disposition.ResponseBody = evidenceDisposition("persist", "external", "stored", dataStorage, &key)
			err := s.DataStorageService.SaveData(ctx, dataStorage, key, responseBodyBytes)
			if err != nil {
				log.Error(ctx, "Failed to save response body to external storage", log.Cause(err))
				failureClass := "external_write_failed"
				disposition.ResponseBody.Outcome = "writeFailed"
				disposition.ResponseBody.FailureClass = &failureClass
				// Continue anyway
			}
		} else {
			managed, admitted, writeFailed := s.admitManagedDatabaseEvidence(ctx, "request_response_body", int64(len(responseBodyBytes)))
			if managed {
				upd = upd.SetManagedObservability(true)
			}
			if admitted {
				upd = upd.SetResponseBody(responseBodyBytes)
				disposition.ResponseBody = evidenceDisposition("persist", "database", "stored", nil, nil)
			} else {
				disposition.ResponseBody = managedEvidenceSkippedDisposition("request_response_body", writeFailed)
			}
		}
	} else {
		disposition.ResponseBody = evidenceDisposition("omit", "none", "omitted", nil, nil)
	}
	upd = upd.SetEvidenceDisposition(disposition)

	_, err = upd.Save(ctx)
	if err != nil {
		if requestTerminalUpdateWasNoop(ctx, client, requestID, err) {
			return nil
		}

		log.Error(ctx, "Failed to update request status", log.Cause(err))
		return err
	}

	return nil
}

// UpdateRequestExecutionCompleted updates request execution status to completed with response body.
func (s *RequestService) UpdateRequestExecutionCompleted(
	ctx context.Context,
	executionID int,
	externalId string,
	responseBody any,
	metrics *LatencyMetrics,
) error {
	return s.UpdateRequestExecutionCompletedForChannel(ctx, executionID, externalId, responseBody, metrics, nil)
}

// UpdateRequestExecutionCompletedForChannel updates execution completion using
// the selected channel's storage overrides when the caller already has it.
func (s *RequestService) UpdateRequestExecutionCompletedForChannel(
	ctx context.Context,
	executionID int,
	externalId string,
	responseBody any,
	metrics *LatencyMetrics,
	channel *Channel,
) error {
	client := s.entFromContext(ctx)

	// Get the execution to check data storage
	execution, err := client.RequestExecution.Get(ctx, executionID)
	if err != nil {
		log.Error(ctx, "Failed to get request execution", log.Cause(err))
		return err
	}
	if isTerminalRequestExecutionStatus(execution.Status) {
		return nil
	}

	// Get data storage if set
	var dataStorage *ent.DataStorage
	if execution.DataStorageID != 0 {
		dataStorage, err = client.DataStorage.Get(ctx, execution.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage", log.Cause(err))
		}
	}
	storeResponseBody := s.shouldStoreExecutionResponseBody(ctx, execution, channel)

	upd := client.RequestExecution.UpdateOneID(executionID).
		Where(requestexecution.StatusIn(
			requestexecution.StatusPending,
			requestexecution.StatusProcessing,
		)).
		SetStatus(requestexecution.StatusCompleted).
		SetExternalID(externalId)
	disposition := cloneEvidenceDisposition(execution.EvidenceDisposition)
	managedEvidence := false

	// Set latency metrics if provided
	if metrics != nil {
		if metrics.LatencyMs != nil {
			upd = upd.SetMetricsLatencyMs(*metrics.LatencyMs)
		}

		if metrics.FirstTokenLatencyMs != nil {
			upd = upd.SetMetricsFirstTokenLatencyMs(*metrics.FirstTokenLatencyMs)
		}

		if metrics.ReasoningDurationMs != nil {
			upd = upd.SetMetricsReasoningDurationMs(*metrics.ReasoningDurationMs)
		}
	}

	if storeResponseBody {
		responseBodyBytes, err := xjson.Marshal(responseBody)
		if err != nil {
			return err
		}

		// Check if we should use external storage
		if s.shouldUseExternalStorage(ctx, dataStorage) {
			// Save to external storage
			key := GenerateExecutionResponseBodyKey(execution.ProjectID, execution.RequestID, executionID)

			disposition.ResponseBody = evidenceDisposition("persist", "external", "stored", dataStorage, &key)
			err := s.DataStorageService.SaveData(ctx, dataStorage, key, responseBodyBytes)
			if err != nil {
				log.Error(ctx, "Failed to save execution response body to external storage", log.Cause(err))
				failureClass := "external_write_failed"
				disposition.ResponseBody.Outcome = "writeFailed"
				disposition.ResponseBody.FailureClass = &failureClass
			}
		} else {
			managed, admitted, writeFailed := s.admitManagedDatabaseEvidence(ctx, "execution_response_body", int64(len(responseBodyBytes)))
			if managed {
				managedEvidence = true
				upd = upd.SetManagedObservability(true)
			}
			if admitted {
				upd = upd.SetResponseBody(responseBodyBytes)
				disposition.ResponseBody = evidenceDisposition("persist", "database", "stored", nil, nil)
			} else {
				disposition.ResponseBody = managedEvidenceSkippedDisposition("execution_response_body", writeFailed)
			}
		}
	} else {
		disposition.ResponseBody = evidenceDisposition("omit", "none", "omitted", nil, nil)
	}
	upd = upd.SetEvidenceDisposition(disposition)

	_, err = upd.Save(ctx)
	if err != nil {
		if requestExecutionTerminalUpdateWasNoop(ctx, client, executionID, err) {
			return nil
		}

		log.Error(ctx, "Failed to update request execution status to completed", log.Cause(err))
		return err
	}
	if managedEvidence {
		if _, markErr := client.Request.UpdateOneID(execution.RequestID).SetManagedObservability(true).Save(ctx); markErr != nil {
			s.SystemService.RecordManagedObservabilityFailure(ctx, "execution_response_group_mark", "failed")
			log.Warn(ctx, "Failed to mark managed execution response request group", log.Cause(markErr))
		}
	}

	return nil
}

// UpdateRequestExecutionCompletedWithAggregationIncomplete records a confirmed
// provider success when the diagnostic stream aggregator could not construct a
// complete response body. Aggregation is an evidence concern and must not turn
// a successfully forwarded execution into a channel/provider failure.
func (s *RequestService) UpdateRequestExecutionCompletedWithAggregationIncomplete(
	ctx context.Context,
	executionID int,
	metrics *LatencyMetrics,
) error {
	client := s.entFromContext(ctx)
	execution, err := client.RequestExecution.Get(ctx, executionID)
	if err != nil {
		return err
	}
	if isTerminalRequestExecutionStatus(execution.Status) {
		return nil
	}

	disposition := cloneEvidenceDisposition(execution.EvidenceDisposition)
	failureClass := "stream_aggregation_incomplete"
	disposition.ResponseBody = objects.Disposition{
		Intent:       "persist",
		Location:     "none",
		Outcome:      "unavailable",
		CapturedAt:   time.Now().UTC(),
		FailureClass: &failureClass,
	}
	upd := client.RequestExecution.UpdateOneID(executionID).
		Where(requestexecution.StatusIn(requestexecution.StatusPending, requestexecution.StatusProcessing)).
		SetStatus(requestexecution.StatusCompleted).
		SetEvidenceDisposition(disposition)
	if metrics != nil {
		if metrics.LatencyMs != nil {
			upd = upd.SetMetricsLatencyMs(*metrics.LatencyMs)
		}
		if metrics.FirstTokenLatencyMs != nil {
			upd = upd.SetMetricsFirstTokenLatencyMs(*metrics.FirstTokenLatencyMs)
		}
		if metrics.ReasoningDurationMs != nil {
			upd = upd.SetMetricsReasoningDurationMs(*metrics.ReasoningDurationMs)
		}
	}
	_, err = upd.Save(ctx)
	if err != nil && requestExecutionTerminalUpdateWasNoop(ctx, client, executionID, err) {
		return nil
	}
	return err
}

// UpdateRequestExecutionCanceled updates request execution status to canceled with error message.
func (s *RequestService) UpdateRequestExecutionCanceled(
	ctx context.Context,
	executionID int,
	errorMsg string,
) error {
	return s.UpdateRequestExecutionStatus(ctx, executionID, requestexecution.StatusCanceled, errorMsg, nil)
}

// ExecutionErrorInfo holds error details for a failed request execution.
type ExecutionErrorInfo struct {
	StatusCode *int
}

// UpdateRequestExecutionFailed updates request execution status to failed with error message and optional error details.
func (s *RequestService) UpdateRequestExecutionFailed(
	ctx context.Context,
	executionID int,
	errorMsg string,
	errorInfo *ExecutionErrorInfo,
) error {
	return s.UpdateRequestExecutionFailedWithMetrics(ctx, executionID, errorMsg, errorInfo, nil)
}

func (s *RequestService) UpdateRequestExecutionFailedWithMetrics(
	ctx context.Context,
	executionID int,
	errorMsg string,
	errorInfo *ExecutionErrorInfo,
	metrics *LatencyMetrics,
) error {
	return s.updateRequestExecutionStatus(
		ctx,
		executionID,
		requestexecution.StatusFailed,
		errorMsg,
		errorInfo,
		true,
		metrics,
	)
}

// UpdateRequestExecutionStatus updates request execution status to the provided value (e.g., canceled or failed), with optional error message.
func (s *RequestService) UpdateRequestExecutionStatus(
	ctx context.Context,
	executionID int,
	status requestexecution.Status,
	errorMsg string,
	errorInfo *ExecutionErrorInfo,
) error {
	return s.updateRequestExecutionStatus(
		ctx,
		executionID,
		status,
		errorMsg,
		errorInfo,
		status == requestexecution.StatusFailed,
		nil,
	)
}

// updateRequestExecutionStatus implements the atomic terminal transition rule:
// completion is provisional until the remaining response pipeline succeeds, so
// a concrete later failure may replace completed. Failed and canceled are
// otherwise terminal, and completion/cancellation cannot replace them.
func (s *RequestService) updateRequestExecutionStatus(
	ctx context.Context,
	executionID int,
	status requestexecution.Status,
	errorMsg string,
	errorInfo *ExecutionErrorInfo,
	causalFailure bool,
	metrics *LatencyMetrics,
) error {
	client := s.entFromContext(ctx)

	upd := client.RequestExecution.UpdateOneID(executionID).SetStatus(status)
	active := requestexecution.StatusIn(
		requestexecution.StatusPending,
		requestexecution.StatusProcessing,
	)
	if status == requestexecution.StatusFailed && causalFailure {
		upd = upd.Where(requestexecution.Or(
			active,
			requestexecution.StatusEQ(requestexecution.StatusCompleted),
			requestexecution.And(
				requestexecution.StatusEQ(requestexecution.StatusFailed),
				requestexecution.ErrorMessageEQ(context.Canceled.Error()),
				requestexecution.ResponseStatusCodeIsNil(),
			),
		))
	} else {
		upd = upd.Where(active)
	}

	if errorMsg != "" {
		upd = upd.SetErrorMessage(errorMsg)
	}

	if errorInfo != nil && errorInfo.StatusCode != nil {
		upd = upd.SetResponseStatusCode(*errorInfo.StatusCode)
	}
	if metrics != nil && metrics.LatencyMs != nil {
		upd = upd.SetMetricsLatencyMs(*metrics.LatencyMs)
	}

	_, err := upd.Save(ctx)
	if err != nil {
		if requestExecutionTerminalUpdateWasNoop(ctx, client, executionID, err) {
			return nil
		}

		log.Error(ctx, "Failed to update request execution status", log.Cause(err), log.Any("status", status))
		return err
	}

	return nil
}

func requestExecutionTerminalUpdateWasNoop(
	ctx context.Context,
	client *ent.Client,
	executionID int,
	updateErr error,
) bool {
	if !ent.IsNotFound(updateErr) {
		return false
	}

	execution, err := client.RequestExecution.Get(ctx, executionID)
	return err == nil && isTerminalRequestExecutionStatus(execution.Status)
}

func isTerminalRequestExecutionStatus(status requestexecution.Status) bool {
	switch status {
	case requestexecution.StatusCompleted, requestexecution.StatusFailed, requestexecution.StatusCanceled:
		return true
	default:
		return false
	}
}

func isCallerCancellation(rawErr, requestContextErr error) bool {
	return errors.Is(rawErr, context.Canceled) &&
		!errors.Is(rawErr, context.DeadlineExceeded) &&
		errors.Is(requestContextErr, context.Canceled)
}

// UpdateRequestExecutionStatusFromError stores the causal terminal status.
// requestContextErr must be captured with context.Cause from the request
// context before callers detach a persistence context. A canceled
// persistence/cleanup context is not evidence that the request itself was
// canceled.
func (s *RequestService) UpdateRequestExecutionStatusFromError(
	ctx context.Context,
	executionID int,
	rawErr error,
	requestContextErr error,
) error {
	return s.UpdateRequestExecutionStatusFromErrorWithMetrics(
		ctx,
		executionID,
		rawErr,
		requestContextErr,
		nil,
	)
}

func (s *RequestService) UpdateRequestExecutionStatusFromErrorWithMetrics(
	ctx context.Context,
	executionID int,
	rawErr error,
	requestContextErr error,
	metrics *LatencyMetrics,
) error {
	errorMsg := ""
	if rawErr != nil {
		errorMsg = rawErr.Error()
	}

	return s.UpdateRequestExecutionStatusFromErrorDetailsWithMetrics(
		ctx,
		executionID,
		rawErr,
		requestContextErr,
		errorMsg,
		nil,
		metrics,
	)
}

// UpdateRequestExecutionStatusFromErrorDetails classifies rawErr while
// preserving a provider-specific message and response metadata.
func (s *RequestService) UpdateRequestExecutionStatusFromErrorDetails(
	ctx context.Context,
	executionID int,
	rawErr error,
	requestContextErr error,
	errorMsg string,
	errorInfo *ExecutionErrorInfo,
) error {
	return s.UpdateRequestExecutionStatusFromErrorDetailsWithMetrics(
		ctx,
		executionID,
		rawErr,
		requestContextErr,
		errorMsg,
		errorInfo,
		nil,
	)
}

func (s *RequestService) UpdateRequestExecutionStatusFromErrorDetailsWithMetrics(
	ctx context.Context,
	executionID int,
	rawErr error,
	requestContextErr error,
	errorMsg string,
	errorInfo *ExecutionErrorInfo,
	metrics *LatencyMetrics,
) error {
	status := requestexecution.StatusFailed
	if isCallerCancellation(rawErr, requestContextErr) {
		status = requestexecution.StatusCanceled
	}

	causalFailure := status == requestexecution.StatusFailed && !errors.Is(rawErr, context.Canceled)
	return s.updateRequestExecutionStatus(ctx, executionID, status, errorMsg, errorInfo, causalFailure, metrics)
}

type jsonStreamEvent struct {
	LastEventID string          `json:"last_event_id,omitempty"`
	Type        string          `json:"event"`
	Data        json.RawMessage `json:"data"`
}

type binaryStreamChunkSummary struct {
	Object      string `json:"object"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
}

func isBinaryStreamChunk(chunk *httpclient.StreamEvent) bool {
	if chunk == nil {
		return false
	}

	eventType := strings.ToLower(strings.TrimSpace(chunk.Type))

	return strings.HasPrefix(eventType, "audio/") || eventType == "application/octet-stream"
}

func shouldSkipStoredStreamChunk(chunk *httpclient.StreamEvent) bool {
	return chunk == nil ||
		(!isBinaryStreamChunk(chunk) && bytes.Equal(chunk.Data, llm.DoneStreamEvent.Data)) ||
		chunk.Type == httpclient.BinaryStreamDoneEventType
}

func marshalStreamEventForStorage(chunk *httpclient.StreamEvent) (objects.JSONRawMessage, error) {
	data := json.RawMessage(chunk.Data)
	if isBinaryStreamChunk(chunk) {
		// Prefer chunk.Size, which is set when the persistence layer summarized the
		// raw audio chunk to avoid buffering audio bytes in memory.
		byteCount := len(chunk.Data)
		if byteCount == 0 {
			byteCount = chunk.Size
		}

		var err error
		data, err = json.Marshal(binaryStreamChunkSummary{
			Object:      "binary.stream_chunk",
			ContentType: strings.TrimSpace(chunk.Type),
			Bytes:       byteCount,
		})
		if err != nil {
			return nil, err
		}
	}

	return xjson.Marshal(jsonStreamEvent{
		LastEventID: chunk.LastEventID,
		Type:        chunk.Type,
		Data:        data,
	})
}

// SaveRequestExecutionChunks saves all response chunks to request execution at once.
// Only stores chunks if the system StoreChunks setting is enabled.
func (s *RequestService) SaveRequestExecutionChunks(
	ctx context.Context,
	executionID int,
	chunks []*httpclient.StreamEvent,
) error {
	return s.SaveRequestExecutionChunksForChannel(ctx, executionID, chunks, nil)
}

// SaveRequestExecutionChunksForChannel persists execution chunks using the
// selected channel's storage override when the caller already has it.
func (s *RequestService) SaveRequestExecutionChunksForChannel(
	ctx context.Context,
	executionID int,
	chunks []*httpclient.StreamEvent,
	channel *Channel,
) error {
	if len(chunks) == 0 {
		return nil
	}

	client := s.entFromContext(ctx)
	execution, err := client.RequestExecution.Get(ctx, executionID)
	if err != nil {
		return fmt.Errorf("failed to get request execution: %w", err)
	}

	storeChunks := s.shouldStoreExecutionStreamChunks(ctx, execution, channel)

	// Only store chunks if enabled
	if !storeChunks {
		disposition := cloneEvidenceDisposition(execution.EvidenceDisposition)
		disposition.ResponseChunks = evidenceDisposition("omit", "none", "omitted", nil, nil)
		_, err = client.RequestExecution.UpdateOneID(executionID).SetEvidenceDisposition(disposition).Save(ctx)
		return err
	}

	// Convert chunks to JSON format, filtering out done events
	var chunkBytes []objects.JSONRawMessage

	for _, chunk := range chunks {
		if shouldSkipStoredStreamChunk(chunk) {
			continue
		}

		b, err := marshalStreamEventForStorage(chunk)
		if err != nil {
			log.Warn(ctx, "Failed to marshal chunk, skipping", log.Cause(err))

			continue
		}

		chunkBytes = append(chunkBytes, b)
	}

	if len(chunkBytes) == 0 {
		disposition := cloneEvidenceDisposition(execution.EvidenceDisposition)
		disposition.ResponseChunks = evidenceDisposition("notApplicable", "none", "omitted", nil, nil)
		_, err = client.RequestExecution.UpdateOneID(executionID).SetEvidenceDisposition(disposition).Save(ctx)
		return err
	}

	// Get data storage if set
	var dataStorage *ent.DataStorage
	if execution.DataStorageID != 0 {
		dataStorage, err = client.DataStorage.Get(ctx, execution.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage", log.Cause(err))
		}
	}

	// Check if we should use external storage
	if s.shouldUseExternalStorage(ctx, dataStorage) {
		key := GenerateExecutionResponseChunksKey(execution.ProjectID, execution.RequestID, executionID)
		disposition := cloneEvidenceDisposition(execution.EvidenceDisposition)
		disposition.ResponseChunks = evidenceDisposition("persist", "external", "stored", dataStorage, &key)

		allChunksBytes, err := json.Marshal(chunkBytes)
		if err != nil {
			return fmt.Errorf("failed to marshal all chunks: %w", err)
		}

		err = s.DataStorageService.SaveData(ctx, dataStorage, key, allChunksBytes)
		if err != nil {
			failureClass := "external_write_failed"
			disposition.ResponseChunks.Outcome = "writeFailed"
			disposition.ResponseChunks.FailureClass = &failureClass
			_, _ = client.RequestExecution.UpdateOneID(executionID).SetEvidenceDisposition(disposition).Save(ctx)
			return fmt.Errorf("failed to save chunks to external storage: %w", err)
		}
		_, err = client.RequestExecution.UpdateOneID(executionID).SetEvidenceDisposition(disposition).Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to save execution chunk disposition: %w", err)
		}
	} else {
		disposition := cloneEvidenceDisposition(execution.EvidenceDisposition)
		encoded, marshalErr := json.Marshal(chunkBytes)
		if marshalErr != nil {
			return fmt.Errorf("failed to size response chunks: %w", marshalErr)
		}
		managed, admitted, writeFailed := s.admitManagedDatabaseEvidence(ctx, "execution_response_chunks", int64(len(encoded)))
		upd := client.RequestExecution.UpdateOneID(executionID).SetEvidenceDisposition(disposition)
		if managed {
			upd = upd.SetManagedObservability(true)
			if !execution.ManagedObservability {
				_, _ = client.Request.UpdateOneID(execution.RequestID).SetManagedObservability(true).Save(ctx)
			}
		}
		if admitted {
			disposition.ResponseChunks = evidenceDisposition("persist", "database", "stored", nil, nil)
			upd = upd.SetResponseChunks(chunkBytes).SetEvidenceDisposition(disposition)
		} else {
			disposition.ResponseChunks = managedEvidenceSkippedDisposition("execution_response_chunks", writeFailed)
			upd = upd.SetEvidenceDisposition(disposition)
		}
		_, err = upd.Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to save response chunks: %w", err)
		}
	}

	return nil
}

// SaveRequestChunks saves all response chunks to request at once.
// Only stores chunks if the system StoreChunks setting is enabled.
func (s *RequestService) SaveRequestChunks(
	ctx context.Context,
	requestID int,
	chunks []*httpclient.StreamEvent,
) error {
	if len(chunks) == 0 {
		return nil
	}

	client := s.entFromContext(ctx)
	req, err := client.Request.Get(ctx, requestID)
	if err != nil {
		return fmt.Errorf("failed to get request: %w", err)
	}

	storeChunks, err := s.SystemService.StoreChunks(ctx)
	if err != nil {
		log.Warn(ctx, "Failed to get StoreChunks setting, defaulting to false", log.Cause(err))

		storeChunks = false
	}

	// Only store chunks if enabled
	if !storeChunks {
		disposition := cloneEvidenceDisposition(req.EvidenceDisposition)
		disposition.ResponseChunks = evidenceDisposition("omit", "none", "omitted", nil, nil)
		_, err = client.Request.UpdateOneID(requestID).SetEvidenceDisposition(disposition).Save(ctx)
		return err
	}

	// Convert chunks to JSON format, filtering out done events
	var chunkBytes []objects.JSONRawMessage

	for _, chunk := range chunks {
		if shouldSkipStoredStreamChunk(chunk) {
			continue
		}

		b, err := marshalStreamEventForStorage(chunk)
		if err != nil {
			log.Warn(ctx, "Failed to marshal chunk, skipping", log.Cause(err))

			continue
		}

		chunkBytes = append(chunkBytes, b)
	}

	if len(chunkBytes) == 0 {
		disposition := cloneEvidenceDisposition(req.EvidenceDisposition)
		disposition.ResponseChunks = evidenceDisposition("notApplicable", "none", "omitted", nil, nil)
		_, err = client.Request.UpdateOneID(requestID).SetEvidenceDisposition(disposition).Save(ctx)
		return err
	}

	// Get data storage if set
	var dataStorage *ent.DataStorage
	if req.DataStorageID != 0 {
		dataStorage, err = client.DataStorage.Get(ctx, req.DataStorageID)
		if err != nil {
			log.Warn(ctx, "Failed to get data storage", log.Cause(err))
		}
	}

	// Check if we should use external storage
	if s.shouldUseExternalStorage(ctx, dataStorage) {
		key := GenerateResponseChunksKey(req.ProjectID, requestID)
		disposition := cloneEvidenceDisposition(req.EvidenceDisposition)
		disposition.ResponseChunks = evidenceDisposition("persist", "external", "stored", dataStorage, &key)

		allChunksBytes, err := json.Marshal(chunkBytes)
		if err != nil {
			return fmt.Errorf("failed to marshal all chunks: %w", err)
		}

		err = s.DataStorageService.SaveData(ctx, dataStorage, key, allChunksBytes)
		if err != nil {
			failureClass := "external_write_failed"
			disposition.ResponseChunks.Outcome = "writeFailed"
			disposition.ResponseChunks.FailureClass = &failureClass
			_, _ = client.Request.UpdateOneID(requestID).SetEvidenceDisposition(disposition).Save(ctx)
			return fmt.Errorf("failed to save chunks to external storage: %w", err)
		}
		_, err = client.Request.UpdateOneID(requestID).SetEvidenceDisposition(disposition).Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to save request chunk disposition: %w", err)
		}
	} else {
		disposition := cloneEvidenceDisposition(req.EvidenceDisposition)
		encoded, marshalErr := json.Marshal(chunkBytes)
		if marshalErr != nil {
			return fmt.Errorf("failed to size response chunks: %w", marshalErr)
		}
		managed, admitted, writeFailed := s.admitManagedDatabaseEvidence(ctx, "request_response_chunks", int64(len(encoded)))
		upd := client.Request.UpdateOneID(requestID).SetEvidenceDisposition(disposition)
		if managed {
			upd = upd.SetManagedObservability(true)
		}
		if admitted {
			disposition.ResponseChunks = evidenceDisposition("persist", "database", "stored", nil, nil)
			upd = upd.SetResponseChunks(chunkBytes).SetEvidenceDisposition(disposition)
		} else {
			disposition.ResponseChunks = managedEvidenceSkippedDisposition("request_response_chunks", writeFailed)
			upd = upd.SetEvidenceDisposition(disposition)
		}
		_, err = upd.Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to save response chunks: %w", err)
		}
	}

	return nil
}

// MarkRequestCanceled updates request status to canceled.
func (s *RequestService) MarkRequestCanceled(ctx context.Context, requestID int) error {
	return s.UpdateRequestStatus(ctx, requestID, request.StatusCanceled)
}

// MarkRequestFailed updates request status to failed.
func (s *RequestService) MarkRequestFailed(ctx context.Context, requestID int) error {
	return s.UpdateRequestStatus(ctx, requestID, request.StatusFailed)
}

// UpdateRequestStatus updates request status to the provided value (e.g., canceled or failed).
func (s *RequestService) UpdateRequestStatus(ctx context.Context, requestID int, status request.Status) error {
	return s.updateRequestStatus(ctx, requestID, status, status == request.StatusFailed)
}

func (s *RequestService) updateRequestStatus(
	ctx context.Context,
	requestID int,
	status request.Status,
	causalFailure bool,
) error {
	client := s.entFromContext(ctx)

	upd := client.Request.UpdateOneID(requestID).SetStatus(status)
	active := request.StatusIn(request.StatusPending, request.StatusProcessing)
	if status == request.StatusFailed && causalFailure {
		upd = upd.Where(request.Or(
			active,
			request.StatusEQ(request.StatusCompleted),
		))
	} else {
		upd = upd.Where(active)
	}

	_, err := upd.Save(ctx)
	if err != nil {
		if requestTerminalUpdateWasNoop(ctx, client, requestID, err) {
			return nil
		}

		return fmt.Errorf("failed to update request status: %w", err)
	}

	return nil
}

func requestTerminalUpdateWasNoop(
	ctx context.Context,
	client *ent.Client,
	requestID int,
	updateErr error,
) bool {
	if !ent.IsNotFound(updateErr) {
		return false
	}

	req, err := client.Request.Get(ctx, requestID)
	return err == nil && isTerminalRequestStatus(req.Status)
}

func isTerminalRequestStatus(status request.Status) bool {
	switch status {
	case request.StatusCompleted, request.StatusFailed, request.StatusCanceled:
		return true
	default:
		return false
	}
}

// UpdateRequestStatusFromError stores canceled only for a causal caller
// cancellation; all other terminal errors are failures.
func (s *RequestService) UpdateRequestStatusFromError(
	ctx context.Context,
	requestID int,
	rawErr error,
	requestContextErr error,
) error {
	if isCallerCancellation(rawErr, requestContextErr) {
		return s.updateRequestStatus(ctx, requestID, request.StatusCanceled, false)
	}

	return s.updateRequestStatus(
		ctx,
		requestID,
		request.StatusFailed,
		!errors.Is(rawErr, context.Canceled),
	)
}

// failStaleRecords updates records older than maxAge to failed status. These
// records survived a prior process lifetime, so they are interrupted work, not
// evidence of a caller cancellation.
func (s *RequestService) failStaleRecords(
	ctx context.Context,
	maxAge time.Duration,
	entityName string,
	updateFn func(ctx context.Context, cutoff time.Time) (int, error),
) error {
	cutoff := time.Now().UTC().Add(-maxAge)
	return authz.RunWithSystemBypassVoid(ctx, "cleanup-"+entityName, func(ctx context.Context) error {
		count, err := updateFn(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("failed to mark stale %s failed: %w", entityName, err)
		}
		if count > 0 {
			log.Info(ctx, "failed stale processing records",
				log.String("entity", entityName),
				log.Int("count", count),
				log.Duration("maxAge", maxAge))
		}
		return nil
	})
}

// maxProcessingDuration defines how long a record can be in "processing" state.
// Records exceeding this are considered stuck and will be failed on startup.
const maxProcessingDuration = 1 * time.Hour

func (s *RequestService) ClearStaleProcessingOnStartup(ctx context.Context) error {
	var errs []error

	if err := s.failStaleRecords(ctx, maxProcessingDuration, "requests", func(ctx context.Context, cutoff time.Time) (int, error) {
		return s.entFromContext(ctx).Request.Update().
			Where(
				request.StatusEQ(request.StatusProcessing),
				request.CreatedAtLT(cutoff),
			).
			SetStatus(request.StatusFailed).
			Save(ctx)
	}); err != nil {
		errs = append(errs, err)
	}

	if err := s.failStaleRecords(ctx, maxProcessingDuration, "executions", func(ctx context.Context, cutoff time.Time) (int, error) {
		return s.entFromContext(ctx).RequestExecution.Update().
			Where(
				requestexecution.StatusEQ(requestexecution.StatusProcessing),
				requestexecution.CreatedAtLT(cutoff),
			).
			SetStatus(requestexecution.StatusFailed).
			SetErrorMessage("request execution interrupted before server restart").
			Save(ctx)
	}); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("startup cleanup failed: %w", errors.Join(errs...))
	}
	return nil
}

// UpdateRequestChannelID updates request with channel ID after channel selection.
func (s *RequestService) UpdateRequestChannelID(ctx context.Context, requestID int, channelID int) error {
	client := s.entFromContext(ctx)

	request, err := client.Request.UpdateOneID(requestID).
		SetChannelID(channelID).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("failed to update request channel ID: %w", err)
	}

	// Reset channel cache for this trace when request completes
	if request.TraceID != 0 {
		s.setLastSuccessfulChannelID(ctx, request.TraceID, channelID)
	}

	return nil
}

// LoadRequestBody returns the stored request body, loading from external storage when necessary.
func (s *RequestService) loadEvidenceData(ctx context.Context, storage *ent.DataStorage, key string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return s.DataStorageService.LoadData(ctx, storage, key)
	}
	return s.DataStorageService.LoadDataBounded(ctx, storage, key, maxBytes)
}

func boundedRawMessage(raw objects.JSONRawMessage, maxBytes int64) (objects.JSONRawMessage, error) {
	if maxBytes >= 0 && int64(len(raw)) > maxBytes {
		return nil, &DataTooLargeError{Size: int64(len(raw)), Max: maxBytes}
	}
	return raw, nil
}

func boundedChunks(chunks []objects.JSONRawMessage, maxBytes int64) ([]objects.JSONRawMessage, error) {
	if maxBytes < 0 {
		return chunks, nil
	}
	size := int64(2)
	for index, chunk := range chunks {
		size += int64(len(chunk))
		if index > 0 {
			size++
		}
		if size > maxBytes {
			return nil, &DataTooLargeError{Size: size, Max: maxBytes}
		}
	}
	return chunks, nil
}

func (s *RequestService) LoadRequestBody(ctx context.Context, req *ent.Request) (objects.JSONRawMessage, error) {
	return s.loadRequestBody(ctx, req, -1)
}

// LoadRequestBodyBounded is LoadRequestBody with a hard byte limit applied
// before or during external storage reads.
func (s *RequestService) LoadRequestBodyBounded(ctx context.Context, req *ent.Request, maxBytes int64) (objects.JSONRawMessage, error) {
	return s.loadRequestBody(ctx, req, maxBytes)
}

func (s *RequestService) loadRequestBody(ctx context.Context, req *ent.Request, maxBytes int64) (objects.JSONRawMessage, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	if data, handled, err := s.loadManagedRequestBody(ctx, req.RequestBodyPayloadID, req.ID, maxBytes); handled {
		if err != nil {
			return nil, err
		}
		if data == nil {
			return xjson.EmptyJSONRawMessage, nil
		}
		return data, nil
	}

	dataStorage, err := s.getDataStorage(ctx, req.DataStorageID)
	if err != nil {
		if maxBytes >= 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn(ctx, "Failed to get data storage for request body", log.Cause(err), log.Int("request_id", req.ID))
		return xjson.EmptyJSONRawMessage, nil
	}

	if !s.shouldUseExternalStorage(ctx, dataStorage) {
		if req.RequestBody == nil {
			return xjson.EmptyJSONRawMessage, nil
		}

		return boundedRawMessage(req.RequestBody, maxBytes)
	}

	key := GenerateRequestBodyKey(req.ProjectID, req.ID)

	data, err := s.loadEvidenceData(ctx, dataStorage, key, maxBytes)
	if err != nil {
		if maxBytes >= 0 {
			return nil, err
		}
		return xjson.EmptyJSONRawMessage, nil
	}

	if json.Valid(data) {
		return objects.JSONRawMessage(data), nil
	}

	return xjson.EmptyJSONRawMessage, nil
}

// LoadResponseBody returns the request response body, loading from external storage when necessary.
func (s *RequestService) LoadResponseBody(ctx context.Context, req *ent.Request) (objects.JSONRawMessage, error) {
	return s.loadResponseBody(ctx, req, -1, false)
}

// LoadResponseBodyBounded is LoadResponseBody with a hard byte limit applied
// before or during external storage reads.
func (s *RequestService) LoadResponseBodyBounded(ctx context.Context, req *ent.Request, maxBytes int64) (objects.JSONRawMessage, error) {
	return s.loadResponseBody(ctx, req, maxBytes, false)
}

// LoadResponseBodyEvidenceBounded loads persisted terminal response evidence
// for diagnostics without changing the success-only LoadResponseBody contract.
// Failed and canceled rows can retain a response produced before a later
// pipeline failure; pending and processing rows never expose stored fields.
func (s *RequestService) LoadResponseBodyEvidenceBounded(ctx context.Context, req *ent.Request, maxBytes int64) (objects.JSONRawMessage, error) {
	return s.loadResponseBody(ctx, req, maxBytes, true)
}

func (s *RequestService) loadResponseBody(
	ctx context.Context,
	req *ent.Request,
	maxBytes int64,
	includeTerminalEvidence bool,
) (objects.JSONRawMessage, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}

	if includeTerminalEvidence {
		if !isTerminalRequestStatus(req.Status) {
			return xjson.EmptyJSONRawMessage, nil
		}
	} else if req.Status != request.StatusCompleted {
		return xjson.EmptyJSONRawMessage, nil
	}
	// Diagnostics evidence queries hydrate inline columns directly. Prefer an
	// actually populated inline value before consulting storage configuration,
	// which may be missing on legacy rows or in recovery-only environments.
	if includeTerminalEvidence && len(req.ResponseBody) > 0 {
		return boundedRawMessage(req.ResponseBody, maxBytes)
	}

	dataStorage, err := s.getDataStorage(ctx, req.DataStorageID)
	if err != nil {
		if maxBytes >= 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn(ctx, "Failed to get data storage for request response body", log.Cause(err), log.Int("request_id", req.ID))
		return xjson.EmptyJSONRawMessage, nil
	}

	if !s.shouldUseExternalStorage(ctx, dataStorage) {
		if req.ResponseBody == nil {
			return xjson.EmptyJSONRawMessage, nil
		}

		return boundedRawMessage(req.ResponseBody, maxBytes)
	}

	key := GenerateResponseBodyKey(req.ProjectID, req.ID)

	data, err := s.loadEvidenceData(ctx, dataStorage, key, maxBytes)
	if err != nil {
		if maxBytes >= 0 {
			return nil, err
		}
		return xjson.EmptyJSONRawMessage, nil
	}

	if json.Valid(data) {
		return objects.JSONRawMessage(data), nil
	}

	return xjson.EmptyJSONRawMessage, nil
}

// LoadResponseChunks returns the request response chunks, loading from external storage when necessary.
func (s *RequestService) LoadResponseChunks(ctx context.Context, req *ent.Request) ([]objects.JSONRawMessage, error) {
	return s.loadResponseChunks(ctx, req, -1)
}

// LoadResponseChunksBounded bounds the encoded chunks blob before JSON
// unmarshalling, preventing a large external array from being materialized.
func (s *RequestService) LoadResponseChunksBounded(ctx context.Context, req *ent.Request, maxBytes int64) ([]objects.JSONRawMessage, error) {
	return s.loadResponseChunks(ctx, req, maxBytes)
}

func (s *RequestService) loadResponseChunks(ctx context.Context, req *ent.Request, maxBytes int64) ([]objects.JSONRawMessage, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	// Live preview for active streaming requests
	if req.Stream && req.Status == request.StatusProcessing {
		chunks := s.LiveStreamRegistry.GetRequestChunks(req.ID)
		return boundedChunks(chunks, maxBytes)
	}
	// Stored chunks are useful evidence for every terminal streaming request.
	if !req.Stream || !isTerminalRequestStatus(req.Status) {
		return []objects.JSONRawMessage{}, nil
	}

	dataStorage, err := s.getDataStorage(ctx, req.DataStorageID)
	if err != nil {
		if maxBytes >= 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn(ctx, "Failed to get data storage for request response chunks", log.Cause(err), log.Int("request_id", req.ID))
		return []objects.JSONRawMessage{}, nil
	}

	if !s.shouldUseExternalStorage(ctx, dataStorage) {
		return boundedChunks(req.ResponseChunks, maxBytes)
	}

	key := GenerateResponseChunksKey(req.ProjectID, req.ID)

	data, err := s.loadEvidenceData(ctx, dataStorage, key, maxBytes)
	if err != nil {
		log.Warn(ctx, "Failed to load request response chunks", log.Cause(err), log.Int("request_id", req.ID))

		if maxBytes >= 0 {
			return nil, err
		}
		return []objects.JSONRawMessage{}, nil
	}

	if len(data) == 0 {
		return []objects.JSONRawMessage{}, nil
	}

	var chunks []objects.JSONRawMessage
	if err := json.Unmarshal(data, &chunks); err != nil {
		log.Warn(ctx, "Failed to unmarshal request response chunks", log.Cause(err), log.Int("request_id", req.ID))
		return []objects.JSONRawMessage{}, nil
	}

	return chunks, nil
}

// LoadRequestExecutionRequestBody returns the execution request body, loading from external storage when necessary.
func (s *RequestService) LoadRequestExecutionRequestBody(ctx context.Context, exec *ent.RequestExecution) (objects.JSONRawMessage, error) {
	return s.loadRequestExecutionRequestBody(ctx, exec, -1)
}

// LoadRequestExecutionRequestBodyBounded is the bounded execution request-body
// variant used by diagnostics exports.
func (s *RequestService) LoadRequestExecutionRequestBodyBounded(ctx context.Context, exec *ent.RequestExecution, maxBytes int64) (objects.JSONRawMessage, error) {
	return s.loadRequestExecutionRequestBody(ctx, exec, maxBytes)
}

func (s *RequestService) loadRequestExecutionRequestBody(ctx context.Context, exec *ent.RequestExecution, maxBytes int64) (objects.JSONRawMessage, error) {
	if exec == nil {
		return nil, fmt.Errorf("request execution is nil")
	}
	if data, handled, err := s.loadManagedRequestBody(ctx, exec.RequestBodyPayloadID, exec.RequestID, maxBytes); handled {
		if err != nil {
			return nil, err
		}
		if data == nil {
			return xjson.EmptyJSONRawMessage, nil
		}
		return data, nil
	}

	dataStorage, err := s.getDataStorage(ctx, exec.DataStorageID)
	if err != nil {
		if maxBytes >= 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn(ctx, "Failed to get data storage for execution request body", log.Cause(err), log.Int("execution_id", exec.ID))
		return xjson.EmptyJSONRawMessage, nil
	}

	if !s.shouldUseExternalStorage(ctx, dataStorage) {
		if exec.RequestBody == nil {
			return xjson.EmptyJSONRawMessage, nil
		}

		return boundedRawMessage(exec.RequestBody, maxBytes)
	}

	key := GenerateExecutionRequestBodyKey(exec.ProjectID, exec.RequestID, exec.ID)

	data, err := s.loadEvidenceData(ctx, dataStorage, key, maxBytes)
	if err != nil {
		if maxBytes >= 0 {
			return nil, err
		}
		return xjson.EmptyJSONRawMessage, nil
	}

	if json.Valid(data) {
		return objects.JSONRawMessage(data), nil
	}

	return xjson.EmptyJSONRawMessage, nil
}

// LoadRequestExecutionResponseBody returns the execution response body, loading from external storage when necessary.
func (s *RequestService) LoadRequestExecutionResponseBody(ctx context.Context, exec *ent.RequestExecution) (objects.JSONRawMessage, error) {
	return s.loadRequestExecutionResponseBody(ctx, exec, -1, false)
}

// LoadRequestExecutionResponseBodyBounded is the bounded execution
// response-body variant used by diagnostics exports.
func (s *RequestService) LoadRequestExecutionResponseBodyBounded(ctx context.Context, exec *ent.RequestExecution, maxBytes int64) (objects.JSONRawMessage, error) {
	return s.loadRequestExecutionResponseBody(ctx, exec, maxBytes, false)
}

// LoadRequestExecutionResponseBodyEvidenceBounded loads persisted terminal
// execution response evidence for diagnostics while preserving the
// success-only application loader contract.
func (s *RequestService) LoadRequestExecutionResponseBodyEvidenceBounded(
	ctx context.Context,
	exec *ent.RequestExecution,
	maxBytes int64,
) (objects.JSONRawMessage, error) {
	return s.loadRequestExecutionResponseBody(ctx, exec, maxBytes, true)
}

func (s *RequestService) loadRequestExecutionResponseBody(
	ctx context.Context,
	exec *ent.RequestExecution,
	maxBytes int64,
	includeTerminalEvidence bool,
) (objects.JSONRawMessage, error) {
	if exec == nil {
		return nil, fmt.Errorf("request execution is nil")
	}

	if includeTerminalEvidence {
		if !isTerminalRequestExecutionStatus(exec.Status) {
			return xjson.EmptyJSONRawMessage, nil
		}
	} else if exec.Status != requestexecution.StatusCompleted {
		return xjson.EmptyJSONRawMessage, nil
	}
	if includeTerminalEvidence && len(exec.ResponseBody) > 0 {
		return boundedRawMessage(exec.ResponseBody, maxBytes)
	}

	dataStorage, err := s.getDataStorage(ctx, exec.DataStorageID)
	if err != nil {
		if maxBytes >= 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn(ctx, "Failed to get data storage for execution response body", log.Cause(err), log.Int("execution_id", exec.ID))
		return xjson.EmptyJSONRawMessage, nil
	}

	if !s.shouldUseExternalStorage(ctx, dataStorage) {
		if exec.ResponseBody == nil {
			return xjson.EmptyJSONRawMessage, nil
		}

		return boundedRawMessage(exec.ResponseBody, maxBytes)
	}

	key := GenerateExecutionResponseBodyKey(exec.ProjectID, exec.RequestID, exec.ID)

	data, err := s.loadEvidenceData(ctx, dataStorage, key, maxBytes)
	if err != nil {
		if maxBytes >= 0 {
			return nil, err
		}
		return xjson.EmptyJSONRawMessage, nil
	}

	if json.Valid(data) {
		return objects.JSONRawMessage(data), nil
	}

	return xjson.EmptyJSONRawMessage, nil
}

// LoadRequestExecutionResponseChunks returns the execution response chunks, loading from external storage when necessary.
func (s *RequestService) LoadRequestExecutionResponseChunks(ctx context.Context, exec *ent.RequestExecution) ([]objects.JSONRawMessage, error) {
	return s.loadRequestExecutionResponseChunks(ctx, exec, -1)
}

// LoadRequestExecutionResponseChunksBounded bounds the encoded chunks blob
// before JSON unmarshalling.
func (s *RequestService) LoadRequestExecutionResponseChunksBounded(ctx context.Context, exec *ent.RequestExecution, maxBytes int64) ([]objects.JSONRawMessage, error) {
	return s.loadRequestExecutionResponseChunks(ctx, exec, maxBytes)
}

func (s *RequestService) loadRequestExecutionResponseChunks(ctx context.Context, exec *ent.RequestExecution, maxBytes int64) ([]objects.JSONRawMessage, error) {
	if exec == nil {
		return nil, fmt.Errorf("request execution is nil")
	}

	// Live preview for active streaming executions
	if exec.Stream && exec.Status == requestexecution.StatusProcessing {
		chunks := s.LiveStreamRegistry.GetExecutionChunks(exec.ID)
		return boundedChunks(chunks, maxBytes)
	}
	// Stored chunks are meaningful only for terminal streaming executions. Active
	// processing executions continue to use the live registry above, while pending
	// executions must not expose stale or pre-populated data.
	if !exec.Stream {
		return []objects.JSONRawMessage{}, nil
	}
	switch exec.Status {
	case requestexecution.StatusCompleted, requestexecution.StatusFailed, requestexecution.StatusCanceled:
	default:
		return []objects.JSONRawMessage{}, nil
	}

	dataStorage, err := s.getDataStorage(ctx, exec.DataStorageID)
	if err != nil {
		if maxBytes >= 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Warn(ctx, "Failed to get data storage for execution response chunks", log.Cause(err), log.Int("execution_id", exec.ID))
		return []objects.JSONRawMessage{}, nil
	}

	if !s.shouldUseExternalStorage(ctx, dataStorage) {
		return boundedChunks(exec.ResponseChunks, maxBytes)
	}

	key := GenerateExecutionResponseChunksKey(exec.ProjectID, exec.RequestID, exec.ID)

	data, err := s.loadEvidenceData(ctx, dataStorage, key, maxBytes)
	if err != nil {
		log.Warn(ctx, "Failed to load request execution response chunks", log.Cause(err), log.Int("execution_id", exec.ID))

		if maxBytes >= 0 {
			return nil, err
		}
		return []objects.JSONRawMessage{}, nil
	}

	if json.Valid(data) {
		var chunks []objects.JSONRawMessage
		if err := json.Unmarshal(data, &chunks); err != nil {
			log.Warn(ctx, "Failed to unmarshal request execution response chunks", log.Cause(err), log.Int("execution_id", exec.ID))
			return []objects.JSONRawMessage{}, nil
		}

		return chunks, nil
	}

	return []objects.JSONRawMessage{}, nil
}

func (s *RequestService) GetTraceFirstRequest(ctx context.Context, traceID int) (*ent.Request, error) {
	client := s.entFromContext(ctx)
	if client == nil {
		return nil, fmt.Errorf("ent client not found in context")
	}

	request, err := client.Request.Query().
		Where(request.TraceIDEQ(traceID), request.StatusEQ(request.StatusCompleted)).
		Order(ent.Asc(request.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to get first request for trace: %w", err)
	}

	return request, nil
}

func (s *RequestService) GetTraceFirstSegment(ctx context.Context, traceID int) (*Segment, error) {
	request, err := s.GetTraceFirstRequest(ctx, traceID)
	if err != nil {
		return nil, err
	}

	if request == nil {
		return nil, nil
	}

	body, err := s.LoadRequestBody(ctx, request)
	if err != nil {
		return nil, err
	}

	request.RequestBody = body

	body, err = s.LoadResponseBody(ctx, request)
	if err != nil {
		return nil, err
	}

	request.ResponseBody = body

	return requestToSegment(ctx, request)
}

// GetLastSuccessfulChannelID retrieves the last successful channel ID from a trace.
// Returns 0 if no successful channel is found.
func (s *RequestService) GetLastSuccessfulChannelID(ctx context.Context, traceID int) (int, error) {
	// Try cache first
	cacheKey := buildLastChannelCacheKey(traceID)
	if channelID, err := s.channelCache.Get(ctx, cacheKey); err == nil {
		return channelID, nil
	}

	req, err := s.entFromContext(ctx).Request.Query().
		Where(
			request.TraceIDEQ(traceID),
			// Only successful requests
			request.StatusEQ(request.StatusCompleted),
			// Must have a channel
			request.ChannelIDNotNil(),
		).
		Order(ent.Desc(request.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// Cache the zero result
			_ = s.channelCache.Set(ctx, cacheKey, 0, store.WithExpiration(5*time.Second))
			return 0, nil
		}

		return 0, fmt.Errorf("failed to query last successful request: %w", err)
	}

	// Cache the result
	s.setLastSuccessfulChannelID(ctx, traceID, req.ChannelID)

	return req.ChannelID, nil
}

func (s *RequestService) setLastSuccessfulChannelID(ctx context.Context, traceID, channelID int) {
	cacheKey := buildLastChannelCacheKey(traceID)
	_ = s.channelCache.Set(ctx, cacheKey, channelID, store.WithExpiration(1*time.Minute))
}

func buildLastChannelCacheKey(traceID int) string {
	return fmt.Sprintf("last_channel:%d", traceID)
}
