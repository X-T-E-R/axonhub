package gql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

const exactRequestEvidenceQuery = `query ExactRequestEvidence($id: ID!) {
	node(id: $id) { __typename ... on Request {
		id projectID status contentSaved metricsLatencyMs requestBody responseBody
		executions(first: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
			edges { node {
				id requestID projectID status errorMessage responseStatusCode passThroughApplied
				metricsLatencyMs requestBody responseBody
			} }
			totalCount
		}
	} }
}`

const exactRequestExecutionEvidenceQuery = `query ExactRequestExecutionPage($id: ID!, $first: Int!) {
	node(id: $id) { __typename ... on Request {
		id projectID
		executions(first: $first, orderBy: {field: CREATED_AT, direction: ASC}) {
			edges { node {
				id requestID projectID status errorMessage responseStatusCode passThroughApplied
				requestBody responseBody
			} }
			totalCount
		}
	} }
}`

type requestEvidenceGraphQLEnv struct {
	client         *ent.Client
	ctx            context.Context
	project        *ent.Project
	primary        *ent.DataStorage
	systemService  *biz.SystemService
	usageService   *biz.UsageLogService
	storageService *biz.DataStorageService
	requestService *biz.RequestService
	handler        http.Handler
}

func newRequestEvidenceGraphQLEnv(t *testing.T) *requestEvidenceGraphQLEnv {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	systemService := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	channelService := biz.NewChannelServiceForTest(client)
	t.Cleanup(channelService.Stop)
	usageService := biz.NewUsageLogService(client, systemService, channelService)
	storageService := biz.NewDataStorageService(biz.DataStorageServiceParams{
		SystemService: systemService,
		CacheConfig:   xcache.Config{},
		Client:        client,
	})
	requestService := biz.NewRequestService(client, systemService, usageService, storageService, biz.NewLiveStreamRegistry())
	projectRow := client.Project.Create().SetName("request-evidence-gql").SetStatus(project.StatusActive).SaveX(ctx)
	primary := client.DataStorage.Create().
		SetName("request-evidence-primary").
		SetDescription("request evidence GraphQL primary").
		SetPrimary(true).
		SetType(datastorage.TypeDatabase).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		SaveX(ctx)
	graphqlHandler := handler.NewDefaultServer(NewExecutableSchema(Config{Resolvers: &Resolver{
		client:         client,
		requestService: requestService,
	}}))

	return &requestEvidenceGraphQLEnv{
		client:         client,
		ctx:            ctx,
		project:        projectRow,
		primary:        primary,
		systemService:  systemService,
		usageService:   usageService,
		storageService: storageService,
		requestService: requestService,
		handler:        graphqlHandler,
	}
}

func (e *requestEvidenceGraphQLEnv) graphQL(
	t *testing.T,
	ctx context.Context,
	query string,
	variables map[string]any,
) (map[string]any, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/admin/graphql", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response, recorder.Body.String()
}

func requireGraphQLNode(t *testing.T, response map[string]any, raw string) map[string]any {
	t.Helper()
	if errorsValue, ok := response["errors"]; ok {
		require.Empty(t, errorsValue, raw)
	}
	data, ok := response["data"].(map[string]any)
	require.True(t, ok, raw)
	node, ok := data["node"].(map[string]any)
	require.True(t, ok, raw)
	return node
}

func requireObject(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	require.True(t, ok, "expected object, got %T", value)
	return result
}

func requireExecutionNode(t *testing.T, requestNode map[string]any) map[string]any {
	t.Helper()
	connection := requireObject(t, requestNode["executions"])
	edges, ok := connection["edges"].([]any)
	require.True(t, ok)
	require.Len(t, edges, 1)
	return requireObject(t, requireObject(t, edges[0])["node"])
}

func requestGUID(id int) string {
	return "gid://axonhub/Request/" + strconv.Itoa(id)
}

func executionGUID(id int) string {
	return "gid://axonhub/RequestExecution/" + strconv.Itoa(id)
}

func testStoredDisposition(location string) *objects.EvidenceDisposition {
	now := time.Now().UTC()
	return &objects.EvidenceDisposition{
		Version:        1,
		RequestBody:    objects.Disposition{Intent: "persist", Location: location, Outcome: "stored", CapturedAt: now},
		ResponseBody:   objects.Disposition{Intent: "persist", Location: location, Outcome: "stored", CapturedAt: now},
		ResponseChunks: objects.Disposition{Intent: "notApplicable", Location: "none", Outcome: "omitted", CapturedAt: now},
	}
}

func TestAdminExactRequestDetailLoadsTerminalInlineEvidence(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)

	for _, test := range []struct {
		name            string
		requestStatus   request.Status
		executionStatus requestexecution.Status
	}{
		{name: "failed", requestStatus: request.StatusFailed, executionStatus: requestexecution.StatusFailed},
		{name: "canceled", requestStatus: request.StatusCanceled, executionStatus: requestexecution.StatusCanceled},
		{name: "completed unchanged", requestStatus: request.StatusCompleted, executionStatus: requestexecution.StatusCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			parentRequestBody := objects.JSONRawMessage(`{"request":"parent-` + test.name + `"}`)
			parentResponseBody := objects.JSONRawMessage(`{"response":"parent-` + test.name + `"}`)
			executionRequestBody := objects.JSONRawMessage(`{"request":"execution-` + test.name + `"}`)
			executionResponseBody := objects.JSONRawMessage(`{"response":"execution-` + test.name + `"}`)
			req := e.client.Request.Create().
				SetProjectID(e.project.ID).
				SetDataStorageID(e.primary.ID).
				SetModelID("terminal-inline-" + test.name).
				SetRequestBody(parentRequestBody).
				SetResponseBody(parentResponseBody).
				SetStatus(test.requestStatus).
				SetMetricsLatencyMs(701).
				SetEvidenceDisposition(testStoredDisposition("database")).
				SaveX(e.ctx)
			exec := e.client.RequestExecution.Create().
				SetProjectID(e.project.ID).
				SetRequestID(req.ID).
				SetDataStorageID(e.primary.ID).
				SetModelID("terminal-inline-" + test.name).
				SetRequestBody(executionRequestBody).
				SetResponseBody(executionResponseBody).
				SetStatus(test.executionStatus).
				SetErrorMessage("fixed terminal error").
				SetResponseStatusCode(http.StatusBadRequest).
				SetPassThroughApplied(true).
				SetMetricsLatencyMs(613).
				SetEvidenceDisposition(testStoredDisposition("database")).
				SaveX(e.ctx)

			response, raw := e.graphQL(t, e.ctx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
			node := requireGraphQLNode(t, response, raw)
			require.Equal(t, string(test.requestStatus), node["status"])
			require.Equal(t, float64(701), node["metricsLatencyMs"])
			require.Equal(t, false, node["contentSaved"])
			require.Equal(t, map[string]any{"request": "parent-" + test.name}, requireObject(t, node["requestBody"]))
			require.Equal(t, map[string]any{"response": "parent-" + test.name}, requireObject(t, node["responseBody"]))

			executionNode := requireExecutionNode(t, node)
			require.Equal(t, executionGUID(exec.ID), executionNode["id"])
			require.Equal(t, string(test.executionStatus), executionNode["status"])
			require.Equal(t, float64(http.StatusBadRequest), executionNode["responseStatusCode"])
			require.Equal(t, true, executionNode["passThroughApplied"])
			require.Equal(t, float64(613), executionNode["metricsLatencyMs"])
			require.Equal(t, map[string]any{"request": "execution-" + test.name}, requireObject(t, executionNode["requestBody"]))
			require.Equal(t, map[string]any{"response": "execution-" + test.name}, requireObject(t, executionNode["responseBody"]))
		})
	}
}

func TestAdminExactRequestDetailLoadsManagedRequestEvidence(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	parentBody := []byte(`{"managed":"parent"}`)
	executionBody := []byte(`{"managed":"execution"}`)
	parentResponse := objects.JSONRawMessage(`{"managedResponse":"parent"}`)
	executionResponse := objects.JSONRawMessage(`{"managedResponse":"execution"}`)
	parentDisposition := testStoredDisposition("database")
	executionDisposition := testStoredDisposition("database")
	setManagedRequestDisposition(parentDisposition, parentBody)
	setManagedRequestDisposition(executionDisposition, executionBody)
	parentDisposition.RequestBody.Outcome = "unavailable"
	parentDisposition.RequestBody.FailureClass = lo.ToPtr("async_pending")
	executionDisposition.RequestBody.Outcome = "unavailable"
	executionDisposition.RequestBody.FailureClass = lo.ToPtr("async_pending")

	req := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("managed-terminal-evidence").
		SetRequestBody([]byte(`{}`)).
		SetResponseBody(parentResponse).
		SetStatus(request.StatusFailed).
		SetManagedObservability(true).
		SetEvidenceDisposition(parentDisposition).
		SaveX(e.ctx)
	exec := e.client.RequestExecution.Create().
		SetProjectID(e.project.ID).
		SetRequestID(req.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("managed-terminal-evidence").
		SetRequestBody([]byte(`{}`)).
		SetResponseBody(executionResponse).
		SetStatus(requestexecution.StatusFailed).
		SetManagedObservability(true).
		SetEvidenceDisposition(executionDisposition).
		SaveX(e.ctx)
	parentPayload := createManagedPayload(t, e, req.ID, parentBody)
	executionPayload := createManagedPayload(t, e, req.ID, executionBody)
	e.client.Request.UpdateOneID(req.ID).SetRequestBodyPayloadID(parentPayload.ID).SaveX(e.ctx)
	e.client.RequestExecution.UpdateOneID(exec.ID).SetRequestBodyPayloadID(executionPayload.ID).SaveX(e.ctx)

	response, raw := e.graphQL(t, e.ctx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
	node := requireGraphQLNode(t, response, raw)
	require.Equal(t, map[string]any{"managed": "parent"}, requireObject(t, node["requestBody"]))
	require.Equal(t, map[string]any{"managedResponse": "parent"}, requireObject(t, node["responseBody"]))
	executionNode := requireExecutionNode(t, node)
	require.Equal(t, map[string]any{"managed": "execution"}, requireObject(t, executionNode["requestBody"]))
	require.Equal(t, map[string]any{"managedResponse": "execution"}, requireObject(t, executionNode["responseBody"]))

	// The UI and GraphQL fallback page executions in a second exact Request
	// operation that does not select the parent status or bodies.
	executionPage, raw := e.graphQL(t, e.ctx, exactRequestExecutionEvidenceQuery, map[string]any{"id": requestGUID(req.ID), "first": 10})
	executionNode = requireExecutionNode(t, requireGraphQLNode(t, executionPage, raw))
	require.Equal(t, map[string]any{"managed": "execution"}, requireObject(t, executionNode["requestBody"]))
	require.Equal(t, map[string]any{"managedResponse": "execution"}, requireObject(t, executionNode["responseBody"]))
}

func setManagedRequestDisposition(disposition *objects.EvidenceDisposition, body []byte) {
	sum := sha256.Sum256(body)
	length := int64(len(body))
	disposition.RequestBody.Location = "managed"
	disposition.RequestBody.SHA256 = hex.EncodeToString(sum[:])
	disposition.RequestBody.ByteLength = &length
}

func createManagedPayload(t *testing.T, e *requestEvidenceGraphQLEnv, requestID int, body []byte) *ent.ObservabilityPayload {
	t.Helper()
	sum := sha256.Sum256(body)
	length := int64(len(body))
	return e.client.ObservabilityPayload.Create().
		SetRequestID(requestID).
		SetKind(observabilitypayload.KindRequestBody).
		SetSha256(hex.EncodeToString(sum[:])).
		SetByteLength(length).
		SetChargedBytes(length + 1024).
		SetData(body).
		SaveX(e.ctx)
}

func TestAdminExactRequestDetailLoadsExternalTerminalEvidence(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	dir := t.TempDir()
	storage := e.client.DataStorage.Create().
		SetName("request-evidence-external").
		SetDescription("external terminal evidence").
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SetStatus(datastorage.StatusActive).
		SaveX(e.ctx)
	req := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetDataStorageID(storage.ID).
		SetModelID("external-terminal-evidence").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusCanceled).
		SaveX(e.ctx)
	exec := e.client.RequestExecution.Create().
		SetProjectID(e.project.ID).
		SetRequestID(req.ID).
		SetDataStorageID(storage.ID).
		SetModelID("external-terminal-evidence").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusCanceled).
		SaveX(e.ctx)

	parentRequestKey := biz.GenerateRequestBodyKey(e.project.ID, req.ID)
	parentResponseKey := biz.GenerateResponseBodyKey(e.project.ID, req.ID)
	executionRequestKey := biz.GenerateExecutionRequestBodyKey(e.project.ID, req.ID, exec.ID)
	executionResponseKey := biz.GenerateExecutionResponseBodyKey(e.project.ID, req.ID, exec.ID)
	parentDisposition := externalStoredDisposition(storage.ID, parentRequestKey, parentResponseKey)
	executionDisposition := externalStoredDisposition(storage.ID, executionRequestKey, executionResponseKey)
	e.client.Request.UpdateOneID(req.ID).SetEvidenceDisposition(parentDisposition).SaveX(e.ctx)
	e.client.RequestExecution.UpdateOneID(exec.ID).SetEvidenceDisposition(executionDisposition).SaveX(e.ctx)
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, parentRequestKey, []byte(`{"external":"parent-request"}`)))
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, parentResponseKey, []byte(`{"external":"parent-response"}`)))
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, executionRequestKey, []byte(`{"external":"execution-request"}`)))
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, executionResponseKey, []byte(`{"external":"execution-response"}`)))

	response, raw := e.graphQL(t, e.ctx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
	node := requireGraphQLNode(t, response, raw)
	require.Equal(t, map[string]any{"external": "parent-request"}, requireObject(t, node["requestBody"]))
	require.Equal(t, map[string]any{"external": "parent-response"}, requireObject(t, node["responseBody"]))
	executionNode := requireExecutionNode(t, node)
	require.Equal(t, map[string]any{"external": "execution-request"}, requireObject(t, executionNode["requestBody"]))
	require.Equal(t, map[string]any{"external": "execution-response"}, requireObject(t, executionNode["responseBody"]))
}

func externalStoredDisposition(storageID int, requestKey, responseKey string) *objects.EvidenceDisposition {
	disposition := testStoredDisposition("external")
	disposition.RequestBody.StorageID = lo.ToPtr(storageID)
	disposition.RequestBody.StorageKey = lo.ToPtr(requestKey)
	disposition.ResponseBody.StorageID = lo.ToPtr(storageID)
	disposition.ResponseBody.StorageKey = lo.ToPtr(responseKey)
	return disposition
}

func TestAdminExactRequestDetailKeepsUnavailableEvidenceEmpty(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)

	for _, test := range []struct {
		name         string
		intent       string
		outcome      string
		failureClass *string
	}{
		{name: "omitted", intent: "omit", outcome: "omitted"},
		{name: "unavailable", intent: "persist", outcome: "unavailable", failureClass: lo.ToPtr("capacity_pressure")},
		{name: "async pending without payload", intent: "persist", outcome: "unavailable", failureClass: lo.ToPtr("async_pending")},
		{name: "write failed", intent: "persist", outcome: "writeFailed", failureClass: lo.ToPtr("external_write_failed")},
		{name: "evicted", intent: "persist", outcome: "evicted", failureClass: lo.ToPtr("capacity_evicted")},
	} {
		t.Run(test.name, func(t *testing.T) {
			disposition := testStoredDisposition("database")
			disposition.RequestBody = objects.Disposition{
				Intent: test.intent, Location: "none", Outcome: test.outcome, FailureClass: test.failureClass, CapturedAt: time.Now().UTC(),
			}
			disposition.ResponseBody = disposition.RequestBody
			req := e.client.Request.Create().
				SetProjectID(e.project.ID).
				SetDataStorageID(e.primary.ID).
				SetModelID("unavailable-" + test.name).
				SetRequestBody([]byte(`{"stale":"parent-request"}`)).
				SetResponseBody([]byte(`{"stale":"parent-response"}`)).
				SetStatus(request.StatusFailed).
				SetEvidenceDisposition(disposition).
				SaveX(e.ctx)
			exec := e.client.RequestExecution.Create().
				SetProjectID(e.project.ID).
				SetRequestID(req.ID).
				SetDataStorageID(e.primary.ID).
				SetModelID("unavailable-" + test.name).
				SetRequestBody([]byte(`{"stale":"execution-request"}`)).
				SetResponseBody([]byte(`{"stale":"execution-response"}`)).
				SetStatus(requestexecution.StatusFailed).
				SetEvidenceDisposition(disposition).
				SaveX(e.ctx)

			response, raw := e.graphQL(t, e.ctx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
			node := requireGraphQLNode(t, response, raw)
			require.Empty(t, requireObject(t, node["requestBody"]))
			require.Empty(t, requireObject(t, node["responseBody"]))
			executionNode := requireExecutionNode(t, node)
			require.Empty(t, requireObject(t, executionNode["requestBody"]))
			require.Empty(t, requireObject(t, executionNode["responseBody"]))

			stored := e.client.Request.GetX(e.ctx, req.ID)
			require.Equal(t, test.outcome, stored.EvidenceDisposition.ResponseBody.Outcome)
			storedExecution := e.client.RequestExecution.GetX(e.ctx, exec.ID)
			require.Equal(t, test.outcome, storedExecution.EvidenceDisposition.ResponseBody.Outcome)
		})
	}
}

func TestAdminExactRequestDetailKeepsPendingMissingAndUnreadableEvidenceEmpty(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)

	t.Run("pending response", func(t *testing.T) {
		req := e.client.Request.Create().
			SetProjectID(e.project.ID).
			SetDataStorageID(e.primary.ID).
			SetModelID("pending-evidence").
			SetRequestBody([]byte(`{"request":"admitted"}`)).
			SetResponseBody([]byte(`{"response":"not-terminal"}`)).
			SetStatus(request.StatusPending).
			SetEvidenceDisposition(testStoredDisposition("database")).
			SaveX(e.ctx)
		response, raw := e.graphQL(t, e.ctx, `query Pending($id: ID!) { node(id: $id) { ... on Request { status requestBody responseBody } } }`, map[string]any{"id": requestGUID(req.ID)})
		node := requireGraphQLNode(t, response, raw)
		require.Equal(t, map[string]any{"request": "admitted"}, requireObject(t, node["requestBody"]))
		require.Empty(t, requireObject(t, node["responseBody"]))
	})

	t.Run("missing", func(t *testing.T) {
		req := e.client.Request.Create().
			SetProjectID(e.project.ID).
			SetDataStorageID(e.primary.ID).
			SetModelID("missing-evidence").
			SetRequestBody([]byte(`{}`)).
			SetStatus(request.StatusFailed).
			SetEvidenceDisposition(testStoredDisposition("database")).
			SaveX(e.ctx)
		response, raw := e.graphQL(t, e.ctx, `query Missing($id: ID!) { node(id: $id) { ... on Request { status responseBody } } }`, map[string]any{"id": requestGUID(req.ID)})
		node := requireGraphQLNode(t, response, raw)
		require.Empty(t, requireObject(t, node["responseBody"]))
	})

	t.Run("unsupported bounded backend", func(t *testing.T) {
		storage := e.client.DataStorage.Create().
			SetName("request-evidence-gcs-unreadable").
			SetDescription("bounded reads unsupported without backend access").
			SetType(datastorage.TypeGcs).
			SetSettings(&objects.DataStorageSettings{}).
			SetStatus(datastorage.StatusActive).
			SaveX(e.ctx)
		req := e.client.Request.Create().
			SetProjectID(e.project.ID).
			SetDataStorageID(storage.ID).
			SetModelID("unreadable-evidence").
			SetRequestBody([]byte(`{}`)).
			SetStatus(request.StatusFailed).
			SetEvidenceDisposition(externalStoredDisposition(storage.ID, "request.json", "response.json")).
			SaveX(e.ctx)
		response, raw := e.graphQL(t, e.ctx, `query Unreadable($id: ID!) { node(id: $id) { ... on Request { status requestBody responseBody } } }`, map[string]any{"id": requestGUID(req.ID)})
		node := requireGraphQLNode(t, response, raw)
		require.Empty(t, requireObject(t, node["requestBody"]))
		require.Empty(t, requireObject(t, node["responseBody"]))
	})
}

func TestAdminExactRequestDetailPreservesPlainTextAndSizeSchemaLimits(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	dir := t.TempDir()
	storage := e.client.DataStorage.Create().
		SetName("request-evidence-schema-limits").
		SetDescription("external raw evidence schema limits").
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SetStatus(datastorage.StatusActive).
		SaveX(e.ctx)

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "plain text", body: []byte("provider failed before returning JSON")},
		{name: "oversized JSON", body: []byte(`{"payload":"` + strings.Repeat("x", int(adminTerminalEvidenceMaxBytes)) + `"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := e.client.Request.Create().
				SetProjectID(e.project.ID).
				SetDataStorageID(storage.ID).
				SetModelID("schema-limit-" + test.name).
				SetRequestBody([]byte(`{}`)).
				SetStatus(request.StatusFailed).
				SaveX(e.ctx)
			key := biz.GenerateResponseBodyKey(e.project.ID, req.ID)
			disposition := externalStoredDisposition(storage.ID, biz.GenerateRequestBodyKey(e.project.ID, req.ID), key)
			e.client.Request.UpdateOneID(req.ID).SetEvidenceDisposition(disposition).SaveX(e.ctx)
			require.NoError(t, e.storageService.SaveData(e.ctx, storage, key, test.body))

			response, raw := e.graphQL(t, e.ctx, `query SchemaLimit($id: ID!) { node(id: $id) { ... on Request { status responseBody } } }`, map[string]any{"id": requestGUID(req.ID)})
			node := requireGraphQLNode(t, response, raw)
			require.Empty(t, requireObject(t, node["responseBody"]))
			stored, err := e.storageService.LoadData(e.ctx, storage, key)
			require.NoError(t, err)
			require.Equal(t, test.body, stored, "GraphQL projection must not rewrite stored raw bytes")
		})
	}
}

func TestTerminalEvidenceAccessRequiresOneRequestRoot(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	responseBody := objects.JSONRawMessage(`{"terminal":"exact-only"}`)
	req := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("exact-only").
		SetRequestBody([]byte(`{}`)).
		SetResponseBody(responseBody).
		SetStatus(request.StatusFailed).
		SetEvidenceDisposition(testStoredDisposition("database")).
		SaveX(e.ctx)
	exec := e.client.RequestExecution.Create().
		SetProjectID(e.project.ID).
		SetRequestID(req.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("exact-only").
		SetRequestBody([]byte(`{}`)).
		SetResponseBody(responseBody).
		SetStatus(requestexecution.StatusFailed).
		SetEvidenceDisposition(testStoredDisposition("database")).
		SaveX(e.ctx)

	direct, raw := e.graphQL(t, e.ctx, `query DirectExecution($id: ID!) { node(id: $id) { ... on RequestExecution { status responseBody } } }`, map[string]any{"id": executionGUID(exec.ID)})
	directNode := requireGraphQLNode(t, direct, raw)
	require.Empty(t, requireObject(t, directNode["responseBody"]))

	multi, raw := e.graphQL(t, e.ctx, `query MultiRoot($id: ID!) {
		first: node(id: $id) { ... on Request { status responseBody } }
		second: node(id: $id) { ... on Request { status responseBody } }
	}`, map[string]any{"id": requestGUID(req.ID)})
	if errorsValue, ok := multi["errors"]; ok {
		require.Empty(t, errorsValue, raw)
	}
	data := requireObject(t, multi["data"])
	require.Empty(t, requireObject(t, requireObject(t, data["first"])["responseBody"]))
	require.Empty(t, requireObject(t, requireObject(t, data["second"])["responseBody"]))
}

func TestTerminalEvidenceAccessRequiresDirectRequestExecutionsPath(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	dir := t.TempDir()
	storage := e.client.DataStorage.Create().
		SetName("request-evidence-relation-boundary").
		SetDescription("external evidence for relation-boundary tests").
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SetStatus(datastorage.StatusActive).
		SaveX(e.ctx)
	channelRow := e.client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("request-evidence-relation-boundary").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{}).
		SetSupportedModels([]string{"relation-boundary"}).
		SetDefaultTestModel("relation-boundary").
		SaveX(e.ctx)
	traceRow := e.client.Trace.Create().
		SetProjectID(e.project.ID).
		SetTraceID("request-evidence-relation-boundary").
		SaveX(e.ctx)

	root := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetTraceID(traceRow.ID).
		SetChannelID(channelRow.ID).
		SetDataStorageID(storage.ID).
		SetModelID("authorized-root").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusFailed).
		SaveX(e.ctx)
	directExecution := e.client.RequestExecution.Create().
		SetProjectID(e.project.ID).
		SetRequestID(root.ID).
		SetChannelID(channelRow.ID).
		SetDataStorageID(storage.ID).
		SetModelID("authorized-direct-execution").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusFailed).
		SaveX(e.ctx)
	sibling := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetTraceID(traceRow.ID).
		SetChannelID(channelRow.ID).
		SetDataStorageID(storage.ID).
		SetModelID("unrelated-sibling").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusFailed).
		SaveX(e.ctx)
	siblingExecution := e.client.RequestExecution.Create().
		SetProjectID(e.project.ID).
		SetRequestID(sibling.ID).
		SetChannelID(channelRow.ID).
		SetDataStorageID(storage.ID).
		SetModelID("unrelated-sibling-execution").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusFailed).
		SaveX(e.ctx)

	rootKey := biz.GenerateResponseBodyKey(e.project.ID, root.ID)
	directKey := biz.GenerateExecutionResponseBodyKey(e.project.ID, root.ID, directExecution.ID)
	siblingKey := biz.GenerateResponseBodyKey(e.project.ID, sibling.ID)
	siblingExecutionKey := biz.GenerateExecutionResponseBodyKey(e.project.ID, sibling.ID, siblingExecution.ID)
	e.client.Request.UpdateOneID(root.ID).
		SetEvidenceDisposition(externalStoredDisposition(storage.ID, biz.GenerateRequestBodyKey(e.project.ID, root.ID), rootKey)).
		SaveX(e.ctx)
	e.client.RequestExecution.UpdateOneID(directExecution.ID).
		SetEvidenceDisposition(externalStoredDisposition(storage.ID, biz.GenerateExecutionRequestBodyKey(e.project.ID, root.ID, directExecution.ID), directKey)).
		SaveX(e.ctx)
	e.client.Request.UpdateOneID(sibling.ID).
		SetEvidenceDisposition(externalStoredDisposition(storage.ID, biz.GenerateRequestBodyKey(e.project.ID, sibling.ID), siblingKey)).
		SaveX(e.ctx)
	e.client.RequestExecution.UpdateOneID(siblingExecution.ID).
		SetEvidenceDisposition(externalStoredDisposition(storage.ID, biz.GenerateExecutionRequestBodyKey(e.project.ID, sibling.ID, siblingExecution.ID), siblingExecutionKey)).
		SaveX(e.ctx)
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, rootKey, []byte(`{"path":"root"}`)))
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, directKey, []byte(`{"path":"direct-execution"}`)))
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, siblingKey, []byte(`{"path":"sibling-request"}`)))
	require.NoError(t, e.storageService.SaveData(e.ctx, storage, siblingExecutionKey, []byte(`{"path":"sibling-execution"}`)))

	query := `query DirectPathBoundary($id: ID!) {
		detail: node(id: $id) { ...RequestEvidencePaths }
	}
	fragment RequestEvidencePaths on Request {
		status
		rootBody: responseBody
		directAttempts: executions(first: 10) {
			edges { node {
				id projectID requestID dataStorageID status directBody: responseBody
			} }
		}
		byStorage: dataStorage {
			storageRequests: requests(first: 10) { edges { node {
				id projectID dataStorageID status responseBody
			} } }
			storageExecutions: executions(first: 10) { edges { node {
				id projectID requestID dataStorageID status responseBody
			} } }
		}
		byChannel: channel {
			channelExecutions: executions(first: 10) { edges { node {
				id projectID requestID dataStorageID status responseBody
			} } }
		}
		byTrace: trace {
			traceRequests: requests(first: 10) { edges { node {
				id projectID dataStorageID status responseBody
			} } }
		}
	}`
	response, raw := e.graphQL(t, e.ctx, query, map[string]any{"id": requestGUID(root.ID)})
	if errorsValue, ok := response["errors"]; ok {
		require.Empty(t, errorsValue, raw)
	}
	detail := requireObject(t, requireObject(t, response["data"])["detail"])
	require.Equal(t, map[string]any{"path": "root"}, requireObject(t, detail["rootBody"]))

	directConnection := requireObject(t, detail["directAttempts"])
	directEdges := directConnection["edges"].([]any)
	require.Len(t, directEdges, 1, raw)
	directNode := requireObject(t, requireObject(t, directEdges[0])["node"])
	require.Equal(t, executionGUID(directExecution.ID), directNode["id"])
	require.Equal(t, map[string]any{"path": "direct-execution"}, requireObject(t, directNode["directBody"]))

	storageNode := requireObject(t, detail["byStorage"])
	assertTerminalEvidenceBodiesEmpty(t, storageNode, "storageRequests", "responseBody", 2)
	assertTerminalEvidenceBodiesEmpty(t, storageNode, "storageExecutions", "responseBody", 2)
	assertTerminalEvidenceBodiesEmpty(t, requireObject(t, detail["byChannel"]), "channelExecutions", "responseBody", 2)
	assertTerminalEvidenceBodiesEmpty(t, requireObject(t, detail["byTrace"]), "traceRequests", "responseBody", 2)
	require.NotContains(t, raw, "sibling-request")
	require.NotContains(t, raw, "sibling-execution")
}

func assertTerminalEvidenceBodiesEmpty(t *testing.T, parent map[string]any, connectionName, bodyName string, count int) {
	t.Helper()
	connection := requireObject(t, parent[connectionName])
	edges, ok := connection["edges"].([]any)
	require.True(t, ok)
	require.Len(t, edges, count)
	for _, edge := range edges {
		node := requireObject(t, requireObject(t, edge)["node"])
		require.Empty(t, requireObject(t, node[bodyName]))
	}
}

func TestRequestIndexSelectionDoesNotLoadBodyColumns(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	req := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("index-skeleton").
		SetRequestBody([]byte(`{"must":"stay out of index SQL"}`)).
		SetResponseBody([]byte(`{"must":"stay out of index SQL"}`)).
		SetStatus(request.StatusFailed).
		SetEvidenceDisposition(testStoredDisposition("database")).
		SaveX(e.ctx)
	e.client.RequestExecution.Create().
		SetProjectID(e.project.ID).
		SetRequestID(req.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("index-skeleton").
		SetRequestBody([]byte(`{"must":"stay out of index SQL"}`)).
		SetResponseBody([]byte(`{"must":"stay out of index SQL"}`)).
		SetStatus(requestexecution.StatusFailed).
		SetEvidenceDisposition(testStoredDisposition("database")).
		SaveX(e.ctx)

	statements := []string{}
	debugClient := ent.NewClient(
		ent.Driver(e.client.Driver()),
		ent.Debug(),
		ent.Log(func(values ...any) { statements = append(statements, fmt.Sprint(values...)) }),
	)
	indexHandler := handler.NewDefaultServer(NewExecutableSchema(Config{Resolvers: &Resolver{
		client:         debugClient,
		requestService: e.requestService,
	}}))
	originalHandler := e.handler
	e.handler = indexHandler
	t.Cleanup(func() { e.handler = originalHandler })
	debugCtx := ent.NewContext(e.ctx, debugClient)
	response, raw := e.graphQL(t, debugCtx, `query GetRequests($first: Int!) {
		requests(first: $first, orderBy: {field: CREATED_AT, direction: DESC}) {
			edges { node {
				id createdAt updatedAt source modelID status
				executions(first: 10, orderBy: {field: CREATED_AT, direction: DESC}) {
					edges { node { id modelID status passThroughApplied } }
					totalCount
				}
			} }
			totalCount
		}
	}`, map[string]any{"first": 10})
	if errorsValue, ok := response["errors"]; ok {
		require.Empty(t, errorsValue, raw)
	}
	require.NotEmpty(t, statements)
	require.LessOrEqual(t, len(statements), 5, "index query count grew unexpectedly: %v", statements)
	for _, statement := range statements {
		lower := strings.ToLower(statement)
		require.NotContains(t, lower, "request_body")
		require.NotContains(t, lower, "response_body")
		require.NotContains(t, lower, "response_chunks")
		require.NotContains(t, lower, "observability_payloads")
	}
}

func TestAdminExactRequestEvidenceHonorsProjectRequestScope(t *testing.T) {
	e := newRequestEvidenceGraphQLEnv(t)
	foreignProject := e.client.Project.Create().SetName("request-evidence-foreign").SetStatus(project.StatusActive).SaveX(e.ctx)
	req := e.client.Request.Create().
		SetProjectID(e.project.ID).
		SetDataStorageID(e.primary.ID).
		SetModelID("project-scoped-evidence").
		SetRequestBody([]byte(`{"scope":"allowed"}`)).
		SetResponseBody([]byte(`{"scope":"allowed"}`)).
		SetStatus(request.StatusFailed).
		SetEvidenceDisposition(testStoredDisposition("database")).
		SaveX(e.ctx)
	user := &ent.User{
		ID: 90210,
		Edges: ent.UserEdges{ProjectUsers: []*ent.UserProject{{
			UserID: 90210, ProjectID: e.project.ID, Scopes: []string{string(scopes.ScopeReadRequests)},
		}}},
	}

	allowedCtx := requestEvidenceUserContext(e.client, user, e.project.ID)
	allowed, raw := e.graphQL(t, allowedCtx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
	allowedNode := requireGraphQLNode(t, allowed, raw)
	require.Equal(t, map[string]any{"scope": "allowed"}, requireObject(t, allowedNode["responseBody"]))

	foreignCtx := requestEvidenceUserContext(e.client, user, foreignProject.ID)
	foreign, raw := e.graphQL(t, foreignCtx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
	require.NotContains(t, raw, `"scope":"allowed"`)
	foreignData := requireObject(t, foreign["data"])
	require.Nil(t, foreignData["node"])

	noScope := &ent.User{ID: 90211, Edges: ent.UserEdges{ProjectUsers: []*ent.UserProject{{UserID: 90211, ProjectID: e.project.ID}}}}
	deniedCtx := requestEvidenceUserContext(e.client, noScope, e.project.ID)
	denied, raw := e.graphQL(t, deniedCtx, exactRequestEvidenceQuery, map[string]any{"id": requestGUID(req.ID)})
	require.NotContains(t, raw, `"scope":"allowed"`)
	require.NotEmpty(t, denied["errors"])
}

func requestEvidenceUserContext(client *ent.Client, user *ent.User, projectID int) context.Context {
	ctx := ent.NewContext(context.Background(), client)
	ctx = contexts.WithUser(ctx, user)
	ctx = contexts.WithProjectID(ctx, projectID)
	return authz.NewUserContext(ctx, user.ID)
}
