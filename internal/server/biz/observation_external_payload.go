package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
)

var errObservationPayloadQueued = errors.New("external observation persistence queued")
var errObservationCompletionPending = errors.New("external observation result queue is full")

const observationExternalPending = "async_external_pending"

func isObservationPersistence(ctx context.Context) bool {
	owner, _ := ctx.Value(observationPersistenceKey{}).(observationPersistenceContext)
	return owner.writer != nil
}

func externalObservationWriteFailure(disposition *objects.Disposition, err error) {
	class := "external_write_failed"
	disposition.Outcome = "writeFailed"
	if errors.Is(err, errObservationPayloadQueued) {
		class = observationExternalPending
		disposition.Outcome = "unavailable"
	} else if errors.Is(err, errObservationQueueUnavailable) {
		class = "async_external_queue_unavailable"
	}
	disposition.FailureClass = &class
}

// deferExternalObservation transfers only optional forwarding evidence. Normal
// business callers have no persistence context and keep synchronous SaveData.
func (s *DataStorageService) deferExternalObservation(ctx context.Context, ds *ent.DataStorage, key string, data []byte) (bool, error) {
	owner, ok := ctx.Value(observationPersistenceKey{}).(observationPersistenceContext)
	if !ok || owner.writer == nil {
		return false, nil
	}
	target, err := parseObservationPayloadTarget(key)
	if err != nil {
		return false, nil
	}
	target.storageID = ds.ID
	settings, err := json.Marshal(ds.Settings)
	if err != nil {
		return true, err
	}
	storage := &ent.DataStorage{ID: ds.ID, Type: ds.Type, Primary: ds.Primary}
	if err := json.Unmarshal(settings, &storage.Settings); err != nil {
		return true, err
	}
	payload := bytes.Clone(data)
	lane := owner.writer.payloadLane
	scope := &observationScope{writer: lane}
	ctx = context.WithValue(ctx, observationPersistenceKey{}, observationPersistenceContext{})
	attempts := 0
	terminalOutcome, terminalFailure := "", ""
	err = scope.submit(ctx, int64(len(payload)+len(settings)+len(key)), func(workerCtx context.Context) error {
		finish := func(outcome, failureClass string) error {
			// Serialize this compact JSON patch with core updates, so a terminal
			// snapshot cannot overwrite a concurrently completed payload result.
			err := (&observationScope{writer: owner.writer}).submit(workerCtx, int64(len(key)+len(failureClass)+64), func(coreCtx context.Context) error {
				return s.finishExternalObservation(coreCtx, target, key, outcome, failureClass)
			})
			if err != nil {
				return fmt.Errorf("%w: %v", errObservationCompletionPending, err)
			}
			return nil
		}
		if terminalOutcome != "" {
			return finish(terminalOutcome, terminalFailure)
		}
		select {
		case <-owner.finished:
		case <-workerCtx.Done():
			return workerCtx.Err()
		}
		policy, err := s.SystemService.StoragePolicy(workerCtx)
		if err != nil {
			return err
		}
		enabled := policy.StoreResponseBody
		if target.part == "requestBody" {
			enabled = policy.StoreRequestBody
			if target.executionID != 0 && policy.StoreExecutionRequestBody != nil {
				enabled = *policy.StoreExecutionRequestBody
			}
		} else if target.part == "responseChunks" {
			enabled = policy.StoreChunks
		}
		if target.executionID != 0 {
			execution, err := s.entFromContext(workerCtx).RequestExecution.Get(workerCtx, target.executionID)
			if ent.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			channel, err := s.entFromContext(workerCtx).Channel.Get(workerCtx, execution.ChannelID)
			if err != nil && !ent.IsNotFound(err) {
				return err
			}
			if channel != nil && channel.Settings != nil {
				var override *bool
				switch target.part {
				case "requestBody":
					override = channel.Settings.StoreExecutionRequestBody
				case "responseBody":
					override = channel.Settings.StoreExecutionResponseBody
				case "responseChunks":
					override = channel.Settings.StoreExecutionStreamChunks
				}
				if override != nil {
					enabled = *override
				}
			}
		}
		if !enabled {
			terminalOutcome, terminalFailure = "omitted", "storage_disabled"
			return finish(terminalOutcome, terminalFailure)
		}
		attempts++
		err = s.SaveData(workerCtx, storage, key, payload)
		if err != nil {
			if attempts >= lane.config.MaxAttempts {
				terminalOutcome, terminalFailure = "writeFailed", "external_write_failed"
				return finish(terminalOutcome, terminalFailure)
			}
			return err
		}
		terminalOutcome = "stored"
		return finish(terminalOutcome, "")
	})
	if err != nil {
		return true, err
	}
	return true, errObservationPayloadQueued
}

type observationPayloadTarget struct {
	projectID, requestID, executionID int
	storageID                         int
	part                              string
}

func parseObservationPayloadTarget(key string) (observationPayloadTarget, error) {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	var target observationPayloadTarget
	if len(parts) != 4 && len(parts) != 6 && len(parts) != 5 {
		return target, errors.New("not a request evidence key")
	}
	if parts[1] != "requests" {
		return target, errors.New("not a request evidence key")
	}
	var err error
	if target.projectID, err = strconv.Atoi(parts[0]); err != nil {
		return target, err
	}
	if target.requestID, err = strconv.Atoi(parts[2]); err != nil {
		return target, err
	}
	if len(parts) == 6 {
		if parts[3] != "executions" {
			return target, errors.New("not an execution evidence key")
		}
		if target.executionID, err = strconv.Atoi(parts[4]); err != nil {
			return target, err
		}
	}
	if len(parts) == 5 && parts[3] == "audio" {
		return target, nil
	}
	switch parts[len(parts)-1] {
	case "request_body.json":
		target.part = "requestBody"
	case "response_body.json":
		target.part = "responseBody"
	case "response_chunks.json":
		target.part = "responseChunks"
	default:
		return target, errors.New("not an observation body key")
	}
	return target, nil
}

func (s *DataStorageService) finishExternalObservation(ctx context.Context, target observationPayloadTarget, key, outcome, failureClass string) error {
	if target.part == "" {
		if outcome == "stored" {
			_, err := s.entFromContext(ctx).Request.Update().Where(request.IDEQ(target.requestID), request.ProjectIDEQ(target.projectID)).
				SetContentSaved(true).SetContentStorageID(target.storageID).SetContentStorageKey(key).SetContentSavedAt(time.Now().UTC()).Save(ctx)
			return err
		}
		return nil
	}
	client := s.entFromContext(ctx)
	column := request.FieldEvidenceDisposition
	if target.executionID != 0 {
		column = requestexecution.FieldEvidenceDisposition
	}
	pending := func(selector *entsql.Selector) {
		selector.Where(sqljson.ValueEQ(column, observationExternalPending, sqljson.Path(target.part, "failureClass"), sqljson.Unquote(true)))
		selector.Where(sqljson.ValueEQ(column, key, sqljson.Path(target.part, "storageKey"), sqljson.Unquote(true)))
	}
	modifier := func(update *entsql.UpdateBuilder) {
		update.Set(column, entsql.ExprFunc(func(b *entsql.Builder) {
			if b.Dialect() == dialect.Postgres {
				b.WriteString("jsonb_set(jsonb_set(").Ident(column).
					WriteString(", '{" + target.part + ",outcome}', to_jsonb(").Arg(outcome).WriteString("::text)), '{" + target.part + ",failureClass}', ")
				if failureClass == "" {
					b.WriteString("'null'::jsonb")
				} else {
					b.WriteString("to_jsonb(").Arg(failureClass).WriteString("::text)")
				}
				b.WriteString(")")
			} else {
				b.WriteString("JSON_SET(").Ident(column).WriteString(", ").Arg("$." + target.part + ".outcome").WriteString(", ").Arg(outcome).
					WriteString(", ").Arg("$." + target.part + ".failureClass").WriteString(", ")
				if failureClass == "" {
					b.Arg(nil)
				} else {
					b.Arg(failureClass)
				}
				b.WriteString(")")
			}
		}))
	}
	if target.executionID != 0 {
		_, err := client.RequestExecution.Update().Where(requestexecution.IDEQ(target.executionID), requestexecution.RequestIDEQ(target.requestID), requestexecution.ProjectIDEQ(target.projectID), pending).Modify(modifier).Save(ctx)
		return err
	}
	_, err := client.Request.Update().Where(request.IDEQ(target.requestID), request.ProjectIDEQ(target.projectID), pending).Modify(modifier).Save(ctx)
	if err != nil {
		return fmt.Errorf("update external observation disposition: %w", err)
	}
	return nil
}
