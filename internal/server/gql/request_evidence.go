package gql

import (
	"context"

	"github.com/99designs/gqlgen/graphql"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xjson"
)

const adminTerminalEvidenceMaxBytes int64 = 2 << 20

func exactAdminRequestNodeField(ctx context.Context) (*graphql.FieldContext, bool) {
	if !graphql.HasOperationContext(ctx) {
		return nil, false
	}

	op := graphql.GetOperationContext(ctx)
	if op.Operation == nil {
		return nil, false
	}
	rootFields := graphql.CollectFields(op, op.Operation.SelectionSet, []string{"Query"})
	if len(rootFields) != 1 || rootFields[0].Name != "node" {
		return nil, false
	}

	for field := graphql.GetFieldContext(ctx); field != nil; field = field.Parent {
		if field.Field.Field == nil {
			continue
		}
		if field.Object != "Query" || field.Field.Name != "node" {
			continue
		}
		id, ok := field.Args["id"].(objects.GUID)
		return field, ok && id.Type == ent.TypeRequest
	}
	return nil, false
}

func exactAdminRequestID(ctx context.Context) (int, bool) {
	field, ok := exactAdminRequestNodeField(ctx)
	if !ok {
		return 0, false
	}
	id, ok := field.Args["id"].(objects.GUID)
	return id.ID, ok
}

type requestEvidencePathField struct {
	object string
	name   string
}

func requestEvidenceFieldPath(ctx context.Context) []requestEvidencePathField {
	path := []requestEvidencePathField{}
	for field := graphql.GetFieldContext(ctx); field != nil; field = field.Parent {
		if field.Field.Field == nil {
			continue
		}
		path = append(path, requestEvidencePathField{object: field.Object, name: field.Field.Name})
	}
	return path
}

func requestEvidencePathEquals(ctx context.Context, expected ...requestEvidencePathField) bool {
	actual := requestEvidenceFieldPath(ctx)
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func isExactAdminRequestBody(ctx context.Context, requestID int, fieldName string) bool {
	rootRequestID, ok := exactAdminRequestID(ctx)
	return ok && requestID == rootRequestID && requestEvidencePathEquals(ctx,
		requestEvidencePathField{object: ent.TypeRequest, name: fieldName},
		requestEvidencePathField{object: "Query", name: "node"},
	)
}

func isDirectExactRequestExecutionBody(ctx context.Context, execution *ent.RequestExecution, fieldName string) bool {
	rootRequestID, ok := exactAdminRequestID(ctx)
	return ok && execution.RequestID == rootRequestID && requestEvidencePathEquals(ctx,
		requestEvidencePathField{object: ent.TypeRequestExecution, name: fieldName},
		requestEvidencePathField{object: "RequestExecutionEdge", name: "node"},
		requestEvidencePathField{object: "RequestExecutionConnection", name: "edges"},
		requestEvidencePathField{object: ent.TypeRequest, name: "executions"},
		requestEvidencePathField{object: "Query", name: "node"},
	)
}

func requestSelectionFields(ctx context.Context, nodeField *graphql.FieldContext) []graphql.CollectedField {
	return graphql.CollectFields(
		graphql.GetOperationContext(ctx),
		nodeField.Field.Selections,
		[]string{ent.TypeRequest},
	)
}

func (r *queryResolver) hydrateExactRequestEvidenceMetadata(ctx context.Context, node ent.Noder) (ent.Noder, error) {
	nodeField, exact := exactAdminRequestNodeField(ctx)
	req, isRequest := node.(*ent.Request)
	rootRequestID, hasRootRequest := exactAdminRequestID(ctx)
	if !exact || !hasRootRequest || !isRequest || req.ID != rootRequestID {
		return node, nil
	}

	requestFields := requestSelectionFields(ctx, nodeField)
	loadParentMetadata := false
	executionNodes := make(map[int][]*ent.RequestExecution)
	for _, field := range requestFields {
		switch field.Name {
		case "requestBody", "responseBody":
			loadParentMetadata = isFailedOrCanceledRequest(req.Status)
		case "executions":
			nodes, err := req.NamedExecutions(field.Alias)
			if err != nil {
				continue
			}
			for _, execution := range nodes {
				if !isFailedOrCanceledExecution(execution.Status) {
					continue
				}
				executionNodes[execution.ID] = append(executionNodes[execution.ID], execution)
			}
		}
	}

	if loadParentMetadata {
		metadata, err := r.client.Request.Query().
			Where(request.IDEQ(req.ID)).
			Select(
				request.FieldID,
				request.FieldProjectID,
				request.FieldDataStorageID,
				request.FieldStatus,
				request.FieldRequestBodyPayloadID,
				request.FieldEvidenceDisposition,
			).
			Only(ctx)
		if err != nil {
			return nil, err
		}
		req.ProjectID = metadata.ProjectID
		req.DataStorageID = metadata.DataStorageID
		req.Status = metadata.Status
		req.RequestBodyPayloadID = metadata.RequestBodyPayloadID
		req.EvidenceDisposition = metadata.EvidenceDisposition
	}

	if len(executionNodes) == 0 {
		return req, nil
	}
	ids := make([]int, 0, len(executionNodes))
	for id := range executionNodes {
		ids = append(ids, id)
	}
	metadataRows, err := r.client.RequestExecution.Query().
		Where(requestexecution.IDIn(ids...)).
		Select(
			requestexecution.FieldID,
			requestexecution.FieldProjectID,
			requestexecution.FieldRequestID,
			requestexecution.FieldDataStorageID,
			requestexecution.FieldStatus,
			requestexecution.FieldRequestBodyPayloadID,
			requestexecution.FieldEvidenceDisposition,
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, metadata := range metadataRows {
		for _, execution := range executionNodes[metadata.ID] {
			execution.ProjectID = metadata.ProjectID
			execution.RequestID = metadata.RequestID
			execution.DataStorageID = metadata.DataStorageID
			execution.Status = metadata.Status
			execution.RequestBodyPayloadID = metadata.RequestBodyPayloadID
			execution.EvidenceDisposition = metadata.EvidenceDisposition
		}
	}

	return req, nil
}

func isFailedOrCanceledRequest(status request.Status) bool {
	return status == request.StatusFailed || status == request.StatusCanceled
}

func isFailedOrCanceledExecution(status requestexecution.Status) bool {
	return status == requestexecution.StatusFailed || status == requestexecution.StatusCanceled
}

func storedDisposition(disposition objects.Disposition) bool {
	if disposition.Intent == "omit" || disposition.Intent == "notApplicable" {
		return false
	}
	// Empty disposition fields identify legacy rows. Physical bytes remain the
	// source of truth for those rows, matching the existing diagnostics loader.
	return disposition.Outcome == "" || disposition.Outcome == "stored"
}

func requestBodyDispositionAllowsRead(obj *ent.Request) bool {
	if obj.EvidenceDisposition == nil {
		return true
	}
	if storedDisposition(obj.EvidenceDisposition.RequestBody) {
		return true
	}
	// The managed writer attaches the authoritative payload pointer after its
	// initial async_pending disposition was stored.
	disposition := obj.EvidenceDisposition.RequestBody
	return obj.RequestBodyPayloadID != nil && disposition.Intent == "persist" && disposition.Location == "managed" &&
		disposition.Outcome == "unavailable" && disposition.FailureClass != nil && *disposition.FailureClass == "async_pending"
}

func executionRequestBodyDispositionAllowsRead(obj *ent.RequestExecution) bool {
	if obj.EvidenceDisposition == nil {
		return true
	}
	if storedDisposition(obj.EvidenceDisposition.RequestBody) {
		return true
	}
	disposition := obj.EvidenceDisposition.RequestBody
	return obj.RequestBodyPayloadID != nil && disposition.Intent == "persist" && disposition.Location == "managed" &&
		disposition.Outcome == "unavailable" && disposition.FailureClass != nil && *disposition.FailureClass == "async_pending"
}

func responseBodyDispositionAllowsRead(disposition *objects.EvidenceDisposition) bool {
	return disposition == nil || storedDisposition(disposition.ResponseBody)
}

func boundedEvidenceOrEmpty(
	ctx context.Context,
	recordType string,
	recordID int,
	load func() (objects.JSONRawMessage, error),
) (objects.JSONRawMessage, error) {
	value, err := load()
	if err == nil {
		return value, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}

	log.Warn(ctx, "Admin request detail evidence is unavailable",
		log.String("record_type", recordType),
		log.Int("record_id", recordID),
		log.Cause(err),
	)
	return xjson.EmptyJSONRawMessage, nil
}

func (r *requestResolver) loadRequestBody(ctx context.Context, obj *ent.Request) (objects.JSONRawMessage, error) {
	if !isExactAdminRequestBody(ctx, obj.ID, "requestBody") || !isFailedOrCanceledRequest(obj.Status) {
		return r.requestService.LoadRequestBody(ctx, obj)
	}
	if !requestBodyDispositionAllowsRead(obj) {
		return xjson.EmptyJSONRawMessage, nil
	}
	return boundedEvidenceOrEmpty(ctx, ent.TypeRequest, obj.ID, func() (objects.JSONRawMessage, error) {
		return r.requestService.LoadRequestBodyBounded(ctx, obj, adminTerminalEvidenceMaxBytes)
	})
}

func (r *requestResolver) loadResponseBody(ctx context.Context, obj *ent.Request) (objects.JSONRawMessage, error) {
	if !isExactAdminRequestBody(ctx, obj.ID, "responseBody") || !isFailedOrCanceledRequest(obj.Status) {
		return r.requestService.LoadResponseBody(ctx, obj)
	}
	if !responseBodyDispositionAllowsRead(obj.EvidenceDisposition) {
		return xjson.EmptyJSONRawMessage, nil
	}
	return boundedEvidenceOrEmpty(ctx, ent.TypeRequest, obj.ID, func() (objects.JSONRawMessage, error) {
		return r.requestService.LoadResponseBodyEvidenceBounded(ctx, obj, adminTerminalEvidenceMaxBytes)
	})
}

func (r *requestExecutionResolver) loadRequestBody(ctx context.Context, obj *ent.RequestExecution) (objects.JSONRawMessage, error) {
	if !isDirectExactRequestExecutionBody(ctx, obj, "requestBody") || !isFailedOrCanceledExecution(obj.Status) {
		return r.requestService.LoadRequestExecutionRequestBody(ctx, obj)
	}
	if !executionRequestBodyDispositionAllowsRead(obj) {
		return xjson.EmptyJSONRawMessage, nil
	}
	return boundedEvidenceOrEmpty(ctx, ent.TypeRequestExecution, obj.ID, func() (objects.JSONRawMessage, error) {
		return r.requestService.LoadRequestExecutionRequestBodyBounded(ctx, obj, adminTerminalEvidenceMaxBytes)
	})
}

func (r *requestExecutionResolver) loadResponseBody(ctx context.Context, obj *ent.RequestExecution) (objects.JSONRawMessage, error) {
	if !isDirectExactRequestExecutionBody(ctx, obj, "responseBody") || !isFailedOrCanceledExecution(obj.Status) {
		return r.requestService.LoadRequestExecutionResponseBody(ctx, obj)
	}
	if !responseBodyDispositionAllowsRead(obj.EvidenceDisposition) {
		return xjson.EmptyJSONRawMessage, nil
	}
	return boundedEvidenceOrEmpty(ctx, ent.TypeRequestExecution, obj.ID, func() (objects.JSONRawMessage, error) {
		return r.requestService.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, obj, adminTerminalEvidenceMaxBytes)
	})
}
