package biz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/managedobservabilitystate"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
)

type ManagedObservabilityStatus struct {
	CapacityEnabled bool   `json:"capacityEnabled"`
	HardMiB         *int   `json:"hardMiB,omitempty"`
	LowMiB          *int   `json:"lowMiB,omitempty"`
	ChargedBytes    int64  `json:"chargedBytes"`
	UnderPressure   bool   `json:"underPressure"`
	LastError       string `json:"lastError,omitempty"`
}

func (s *SystemService) ManagedObservabilityStatus(ctx context.Context) (*ManagedObservabilityStatus, error) {
	ctx = authz.WithSystemBypass(ctx, "managed-observability-status")
	policy, err := s.StoragePolicy(ctx)
	if err != nil {
		return nil, err
	}
	status := &ManagedObservabilityStatus{
		CapacityEnabled: policy.ManagedObservabilityHardMiB != nil && policy.ManagedObservabilityLowMiB != nil,
		HardMiB:         policy.ManagedObservabilityHardMiB,
		LowMiB:          policy.ManagedObservabilityLowMiB,
	}
	state, err := s.entFromContext(ctx).ManagedObservabilityState.Get(ctx, 1)
	if ent.IsNotFound(err) {
		return status, nil
	}
	if err != nil {
		return nil, err
	}
	status.ChargedBytes = state.ChargedBytes
	status.UnderPressure = state.UnderPressure
	status.LastError = state.LastError
	return status, nil
}

const managedPayloadFixedChargeBytes int64 = 4096

func managedPayloadCharge(byteLength int64) int64 {
	// A 75% variable margin plus a page covers row/index/TOAST bookkeeping and
	// deliberately overstates steady-state live allocation for capacity gates.
	return byteLength + (3*byteLength)/4 + managedPayloadFixedChargeBytes
}

type managedPayloadResult struct {
	payload *ent.ObservabilityPayload
	reused  bool
	skipped bool
}

func setManagedBodyMetadata(disposition *objects.Disposition, body []byte) {
	digest := sha256.Sum256(body)
	disposition.SHA256 = hex.EncodeToString(digest[:])
	length := int64(len(body))
	disposition.ByteLength = &length
}

func capacityBytes(policy *StoragePolicy) (hard, low int64, enabled bool) {
	if policy == nil || policy.ManagedObservabilityHardMiB == nil || policy.ManagedObservabilityLowMiB == nil {
		return 0, 0, false
	}
	return int64(*policy.ManagedObservabilityHardMiB) << 20,
		int64(*policy.ManagedObservabilityLowMiB) << 20, true
}

func (s *SystemService) ManagedObservabilityCapacityEnabled(ctx context.Context) bool {
	policy, err := s.StoragePolicy(ctx)
	if err != nil {
		return false
	}
	_, _, enabled := capacityBytes(policy)
	return enabled
}

// RecordManagedObservabilityFailure exposes a component-level degraded signal
// through both metrics and the non-blocking health/status record. Persistence is
// best effort because the underlying failure may itself be a database outage.
func (s *SystemService) RecordManagedObservabilityFailure(ctx context.Context, component, reason string) {
	component = strings.TrimSpace(component)
	reason = strings.TrimSpace(reason)
	if component == "" {
		component = "unknown"
	}
	if reason == "" {
		reason = "unknown"
	}
	metrics.RecordManagedObservabilityFailure(ctx, component, reason)
	ctx = authz.WithSystemBypass(ctx, "managed-observability-failure-state")
	errValue := component + ":" + reason
	if err := s.entFromContext(ctx).ManagedObservabilityState.Create().
		SetID(1).
		SetUnderPressure(true).
		SetLastError(errValue).
		OnConflictColumns(managedobservabilitystate.FieldID).
		Update(func(update *ent.ManagedObservabilityStateUpsert) {
			update.SetUnderPressure(true)
			update.SetLastError(errValue)
		}).Exec(ctx); err != nil {
		log.Warn(ctx, "Failed to persist managed observability degraded component",
			log.String("component", component), log.String("reason", reason), log.Cause(err))
	}
}

// AdmitManagedObservabilityEvidence reserves a conservative charge for a
// primary-database variable evidence item. It returns whether capacity mode is
// enabled and whether the item may be persisted. Any error is fail-open for
// traffic but fail-closed for evidence: callers preserve their skeleton and
// skip the variable item.
func (s *SystemService) AdmitManagedObservabilityEvidence(ctx context.Context, component string, byteLength int64) (managed, admitted bool, err error) {
	if byteLength < 0 {
		byteLength = 0
	}
	err = s.RunInTransaction(ctx, func(txCtx context.Context) error {
		policy, loadErr := s.StoragePolicy(txCtx)
		if loadErr != nil {
			return fmt.Errorf("load capacity policy: %w", loadErr)
		}
		hard, _, enabled := capacityBytes(policy)
		if !enabled {
			admitted = true
			return nil
		}
		managed = true
		client := s.entFromContext(txCtx)
		state, stateErr := ensureManagedState(txCtx, client)
		if stateErr != nil {
			return stateErr
		}
		charge := managedPayloadCharge(byteLength)
		if state.UnderPressure || state.ChargedBytes+charge > hard {
			if _, updateErr := client.ManagedObservabilityState.UpdateOneID(1).
				SetUnderPressure(true).
				SetLastError("capacity_hard_limit:" + component).
				Save(txCtx); updateErr != nil {
				return fmt.Errorf("mark capacity pressure for %s: %w", component, updateErr)
			}
			return nil
		}
		if _, updateErr := client.ManagedObservabilityState.UpdateOneID(1).
			AddChargedBytes(charge).
			ClearLastError().
			Save(txCtx); updateErr != nil {
			return fmt.Errorf("reserve capacity for %s: %w", component, updateErr)
		}
		admitted = true
		return nil
	})
	if err != nil {
		s.RecordManagedObservabilityFailure(ctx, "admission_lock:"+component, "failed")
	}
	return managed, admitted, err
}

func supportsRowLock(client *ent.Client) bool {
	switch client.Driver().Dialect() {
	case dialect.Postgres, dialect.MySQL:
		return true
	default:
		return false
	}
}

func lockRequestGroup(ctx context.Context, client *ent.Client, requestID int) error {
	query := client.Request.Query().Where(request.IDEQ(requestID)).Select(request.FieldID)
	if supportsRowLock(client) {
		query.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	if _, err := query.Only(ctx); err != nil {
		return fmt.Errorf("lock managed observability request group %d: %w", requestID, err)
	}
	return nil
}

func ensureManagedState(ctx context.Context, client *ent.Client) (*ent.ManagedObservabilityState, error) {
	if _, err := client.ManagedObservabilityState.Get(ctx, 1); ent.IsNotFound(err) {
		payloads, queryErr := client.ObservabilityPayload.Query().
			Select(observabilitypayload.FieldChargedBytes).
			All(ctx)
		if queryErr != nil {
			return nil, fmt.Errorf("reconcile managed observability charge: %w", queryErr)
		}
		var charged int64
		for _, payload := range payloads {
			charged += payload.ChargedBytes
		}
		if err := client.ManagedObservabilityState.Create().
			SetID(1).
			SetChargedBytes(charged).
			OnConflictColumns(managedobservabilitystate.FieldID).
			Ignore().
			Exec(ctx); err != nil {
			return nil, fmt.Errorf("initialize managed observability state: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("load managed observability state: %w", err)
	}

	query := client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1))
	if supportsRowLock(client) {
		query.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	state, err := query.Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock managed observability state: %w", err)
	}
	return state, nil
}

// persistManagedRequestBody performs exact byte deduplication inside one
// request group. The request row lock makes concurrent retry/failover attempts
// converge on a single physical payload. Hash and length only narrow the
// candidates; bytes.Equal is the reuse authority.
func (s *RequestService) persistManagedRequestBody(ctx context.Context, requestID int, body objects.JSONRawMessage) (managedPayloadResult, error) {
	var result managedPayloadResult
	err := s.RunInTransaction(ctx, func(txCtx context.Context) error {
		client := s.entFromContext(txCtx)
		if err := lockRequestGroup(txCtx, client, requestID); err != nil {
			return err
		}

		digestBytes := sha256.Sum256(body)
		digest := hex.EncodeToString(digestBytes[:])
		candidates, err := client.ObservabilityPayload.Query().Where(
			observabilitypayload.RequestIDEQ(requestID),
			observabilitypayload.KindEQ(observabilitypayload.KindRequestBody),
			observabilitypayload.Sha256EQ(digest),
			observabilitypayload.ByteLengthEQ(int64(len(body))),
		).All(txCtx)
		if err != nil {
			return fmt.Errorf("query managed request-body payload: %w", err)
		}
		for _, candidate := range candidates {
			if bytes.Equal(candidate.Data, body) {
				result.payload = candidate
				result.reused = true
				return nil
			}
		}

		policy, err := s.SystemService.StoragePolicy(txCtx)
		if err != nil {
			return fmt.Errorf("load managed observability capacity policy: %w", err)
		}
		hard, _, capacityEnabled := capacityBytes(policy)
		charge := managedPayloadCharge(int64(len(body)))
		state, err := ensureManagedState(txCtx, client)
		if err != nil {
			return err
		}
		if capacityEnabled {
			if state.UnderPressure || state.ChargedBytes+charge > hard {
				if !state.UnderPressure {
					if _, err := client.ManagedObservabilityState.UpdateOneID(1).
						SetUnderPressure(true).
						SetLastError("capacity_hard_limit").
						Save(txCtx); err != nil {
						return fmt.Errorf("mark managed observability pressure: %w", err)
					}
				}
				result.skipped = true
				return nil
			}
		}

		payload, err := client.ObservabilityPayload.Create().
			SetRequestID(requestID).
			SetKind(observabilitypayload.KindRequestBody).
			SetSha256(digest).
			SetByteLength(int64(len(body))).
			SetChargedBytes(charge).
			SetData(bytes.Clone(body)).
			Save(txCtx)
		if err != nil {
			return fmt.Errorf("create managed request-body payload: %w", err)
		}
		if _, err := client.ManagedObservabilityState.UpdateOneID(1).
			AddChargedBytes(charge).
			ClearLastError().
			Save(txCtx); err != nil {
			return fmt.Errorf("charge managed request-body payload: %w", err)
		}
		result.payload = payload
		return nil
	})
	return result, err
}

func (s *RequestService) loadManagedRequestBody(ctx context.Context, payloadID *int, requestID int, maxBytes int64) (objects.JSONRawMessage, bool, error) {
	if payloadID == nil {
		return nil, false, nil
	}
	client := s.entFromContext(ctx)
	// Read only immutable metadata first. Diagnostics commonly uses a 2 MiB
	// bound; selecting Data here would allocate/transfer a much larger TOAST/blob
	// value before the bound could reject it.
	metadata, err := client.ObservabilityPayload.Query().
		Where(observabilitypayload.IDEQ(*payloadID)).
		Select(
			observabilitypayload.FieldID,
			observabilitypayload.FieldRequestID,
			observabilitypayload.FieldKind,
			observabilitypayload.FieldByteLength,
		).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("load managed request-body payload %d: %w", *payloadID, err)
	}
	if metadata.RequestID != requestID || metadata.Kind != observabilitypayload.KindRequestBody {
		return nil, true, fmt.Errorf("managed request-body payload %d is outside request group %d", metadata.ID, requestID)
	}
	if maxBytes >= 0 && metadata.ByteLength > maxBytes {
		return nil, true, ErrDataTooLarge
	}
	payload, err := client.ObservabilityPayload.Query().
		Where(observabilitypayload.IDEQ(*payloadID)).
		Select(observabilitypayload.FieldID, observabilitypayload.FieldData).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("load managed request-body payload data %d: %w", *payloadID, err)
	}
	data, err := boundedRawMessage(objects.JSONRawMessage(payload.Data), maxBytes)
	return data, true, err
}

func (s *RequestService) discardUnreferencedManagedPayload(ctx context.Context, payloadID int) {
	if err := s.RunInTransaction(ctx, func(txCtx context.Context) error {
		client := s.entFromContext(txCtx)
		payload, err := client.ObservabilityPayload.Get(txCtx, payloadID)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		requestRef, err := client.Request.Query().Where(request.RequestBodyPayloadIDEQ(payloadID)).Exist(txCtx)
		if err != nil || requestRef {
			return err
		}
		executionRef, err := client.RequestExecution.Query().Where(requestexecution.RequestBodyPayloadIDEQ(payloadID)).Exist(txCtx)
		if err != nil || executionRef {
			return err
		}
		stateQuery := client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1))
		if supportsRowLock(client) {
			stateQuery.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
		}
		state, stateErr := stateQuery.Only(txCtx)
		if stateErr != nil && !ent.IsNotFound(stateErr) {
			return stateErr
		}
		if err := client.ObservabilityPayload.DeleteOneID(payloadID).Exec(txCtx); err != nil {
			return err
		}
		if stateErr == nil {
			_, err = client.ManagedObservabilityState.UpdateOneID(1).
				SetChargedBytes(max(int64(0), state.ChargedBytes-payload.ChargedBytes)).
				Save(txCtx)
		}
		return err
	}); err != nil {
		s.SystemService.RecordManagedObservabilityFailure(ctx, "payload_orphan_cleanup", "failed")
		log.Warn(ctx, "Failed to discard unreferenced managed observability payload",
			log.Int("payload_id", payloadID), log.Cause(err))
	}
}
