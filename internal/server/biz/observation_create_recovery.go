package biz

import (
	"context"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

// A committed INSERT can be reported as an error before its body phase runs.
// Resume that phase on the existing row, without repeating INSERT/header charge.
func observedBodyNeedsResume(disposition *objects.EvidenceDisposition, payloadID *int) bool {
	if disposition == nil || payloadID != nil {
		return false
	}
	body := disposition.RequestBody
	return (body.FailureClass != nil && *body.FailureClass == managedRequestBodyAsyncPending) ||
		(body.Outcome == "stored" && (body.Location == "database" || (body.Location == "external" && body.StorageKey == nil)))
}

func (s *RequestService) resumeObservedBodyPlan(ctx context.Context, storageID, channelID int, body []byte, disposition *objects.EvidenceDisposition) (*ent.DataStorage, bool, bool, *managedRequestBodyReservation, error) {
	policy, err := s.SystemService.StoragePolicy(ctx)
	if err != nil {
		return nil, false, false, nil, err
	}
	enabled := policy.StoreRequestBody
	if channelID != 0 {
		channel, err := s.entFromContext(ctx).Channel.Get(ctx, channelID)
		if err != nil {
			return nil, false, false, nil, err
		}
		enabled = s.shouldStoreExecutionRequestBody(ctx, &Channel{Channel: channel})
	}
	if !enabled {
		class := "storage_disabled"
		disposition.RequestBody.Outcome = "omitted"
		disposition.RequestBody.Location = "none"
		disposition.RequestBody.FailureClass = &class
		return nil, false, false, nil, nil
	}
	if disposition.RequestBody.Location == "external" {
		storage, err := s.DataStorageService.GetDataStorageByID(ctx, storageID)
		return storage, true, false, nil, err
	}
	if s.ManagedRequestBodyWriter == nil {
		return nil, false, true, nil, nil
	}
	reservation, rejection := s.ManagedRequestBodyWriter.reserve(body)
	disposition.RequestBody = asyncManagedRequestBodyDisposition(body, rejection, disposition.RequestBody.CapturedAt)
	return nil, false, true, reservation, nil
}

func (s *RequestService) resumeObservedRequestBody(ctx context.Context, req *ent.Request, body []byte) error {
	if !observedBodyNeedsResume(req.EvidenceDisposition, req.RequestBodyPayloadID) {
		return nil
	}
	storage, external, managed, reservation, err := s.resumeObservedBodyPlan(ctx, req.DataStorageID, 0, body, req.EvidenceDisposition)
	if err != nil {
		return err
	}
	if err := s.entFromContext(ctx).Request.UpdateOneID(req.ID).SetEvidenceDisposition(req.EvidenceDisposition).Exec(ctx); err != nil {
		reservation.release()
		return err
	}
	_, err = s.finishCreatedRequestBody(ctx, req, storage, body, external, managed, reservation, req.EvidenceDisposition)
	return err
}

func (s *RequestService) resumeObservedExecutionBody(ctx context.Context, execution *ent.RequestExecution, body []byte) error {
	parent, err := s.entFromContext(ctx).Request.Get(ctx, execution.RequestID)
	if err != nil {
		return err
	}
	var storage *ent.DataStorage
	var external, managed bool
	var reservation *managedRequestBodyReservation
	if observedBodyNeedsResume(execution.EvidenceDisposition, execution.RequestBodyPayloadID) {
		storage, external, managed, reservation, err = s.resumeObservedBodyPlan(ctx, execution.DataStorageID, execution.ChannelID, body, execution.EvidenceDisposition)
		if err != nil {
			return err
		}
		if err := s.entFromContext(ctx).RequestExecution.UpdateOneID(execution.ID).SetEvidenceDisposition(execution.EvidenceDisposition).Exec(ctx); err != nil {
			reservation.release()
			return err
		}
	}
	_, err = s.finishCreatedExecutionBody(ctx, execution, parent, storage, body, external, managed, reservation, execution.EvidenceDisposition)
	return err
}
