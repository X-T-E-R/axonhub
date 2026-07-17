package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

func TestContractHashMatchesCanonicalSchema(t *testing.T) {
	raw, err := os.ReadFile("contract/v1/contract.schema.json")
	require.NoError(t, err)
	var schema any
	require.NoError(t, json.Unmarshal(raw, &schema))
	canonical, err := json.Marshal(schema)
	require.NoError(t, err)
	sum := sha256.Sum256(canonical)
	require.Equal(t, SchemaSHA256, hex.EncodeToString(sum[:]))
	assertClosedObjectSchemas(t, schema, "$")
}

func TestContractGeneratorTreatsSchemaAsAuthoritativeInput(t *testing.T) {
	source, err := os.ReadFile("contractgen/main.go")
	require.NoError(t, err)
	require.NotContains(t, string(source), `WriteFile("contract/v1/contract.schema.json"`)
	require.NotContains(t, string(source), "jsonschema.ForType")

	raw, err := os.ReadFile("contract/v1/contract.schema.json")
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	defs := schema["$defs"].(map[string]any)
	scope := defs["scope"].(map[string]any)["properties"].(map[string]any)
	subject := scope["subjectUserId"].(map[string]any)
	require.Equal(t, ContractSubjectUserIDMinimum, int(subject["minimum"].(float64)))
	contract := defs["contractRequest"].(map[string]any)["properties"].(map[string]any)
	minMinor := contract["minMinor"].(map[string]any)
	require.Equal(t, ContractMinorMinimum, int(minMinor["minimum"].(float64)))
}

func assertClosedObjectSchemas(t *testing.T, value any, path string) {
	t.Helper()
	switch node := value.(type) {
	case map[string]any:
		if node["type"] == "object" {
			require.Equalf(t, false, node["additionalProperties"], "object schema must be closed at %s", path)
		}
		for key, child := range node {
			assertClosedObjectSchemas(t, child, path+"/"+key)
		}
	case []any:
		for index, child := range node {
			assertClosedObjectSchemas(t, child, fmt.Sprintf("%s/%d", path, index))
		}
	}
}

func TestDecodePullRequestRejectsUnknownNestedField(t *testing.T) {
	raw := `{"contract":{"name":"axonhub.remote-diagnostics","major":1,"minMinor":0,"maxMinor":0},"scope":{"projectId":1},"selector":{"kind":"snapshot","unknown":true},"include":{"sections":["health"]}}`
	_, err := DecodePullRequest(strings.NewReader(raw))
	require.Error(t, err)
	require.ErrorContains(t, err, "pullRequest schema")
}

func TestDecodeContractPullRequestPreservesSelectorIntegerLexemes(t *testing.T) {
	raw := `{"contract":{"name":"axonhub.remote-diagnostics","major":1,"minMinor":0,"maxMinor":0},"scope":{"projectId":1},"selector":{"kind":"requestIds","ids":[9007199254740993]},"include":{"sections":["requests"]}}`
	boundary, err := DecodeContractPullRequest(strings.NewReader(raw))
	require.NoError(t, err)
	selector, ok := boundary.Selector.(map[string]any)
	require.True(t, ok)
	ids, ok := selector["ids"].([]any)
	require.True(t, ok)
	require.Equal(t, json.Number("9007199254740993"), ids[0])

	internal, err := PullRequestFromContract(boundary)
	require.NoError(t, err)
	require.JSONEq(t, `[9007199254740993]`, string(internal.Selector.IDs))
	decoded, err := decodeRequestIDs(internal.Selector.IDs)
	require.NoError(t, err)
	require.Equal(t, []int{9007199254740993}, decoded)

	overflow := strings.Replace(raw, "9007199254740993", "9223372036854775808", 1)
	overflowBoundary, err := DecodeContractPullRequest(strings.NewReader(overflow))
	require.NoError(t, err)
	overflowInternal, err := PullRequestFromContract(overflowBoundary)
	require.NoError(t, err)
	require.Error(t, validateRequest(overflowInternal))
}

func TestDecodePullRequestEnforcesSchemaPresenceAndExplicitBounds(t *testing.T) {
	base := `{"contract":{"name":"axonhub.remote-diagnostics","major":1,"minMinor":0,"maxMinor":0},"scope":{"projectId":1},"selector":{"kind":"snapshot"},"include":{"sections":["health"]}}`
	_, err := DecodePullRequest(strings.NewReader(strings.Replace(base, `"scope":{"projectId":1}`, `"scope":{}`, 1)))
	require.Error(t, err)
	_, err = DecodePullRequest(strings.NewReader(strings.Replace(base, `"scope":{"projectId":1}`, `"scope":{"projectId":1,"subjectUserId":null}`, 1)))
	require.Error(t, err)
	_, err = DecodePullRequest(strings.NewReader(strings.Replace(base, `"scope":{"projectId":1}`, `"scope":{"projectId":1,"subjectUserId":0}`, 1)))
	require.Error(t, err)
	_, err = DecodePullRequest(strings.NewReader(strings.Replace(base, `"minMinor":0`, `"minMinor":-1`, 1)))
	require.Error(t, err)
	_, err = DecodePullRequest(strings.NewReader(strings.Replace(base, `"maxMinor":0`, `"maxMinor":-1`, 1)))
	require.Error(t, err)
	_, err = DecodePullRequest(strings.NewReader(strings.Replace(base, `"include":{"sections":["health"]}`, `"include":{"sections":[]}`, 1)))
	require.Error(t, err)
	_, err = DecodePullRequest(strings.NewReader(strings.Replace(base, `"include":{"sections":["health"]}`, `"include":{"sections":["health"]},"limits":{"maxRequests":0}`, 1)))
	require.Error(t, err)
	timeRange := strings.Replace(base, `"selector":{"kind":"snapshot"}`, `"selector":{"kind":"timeRange","from":"2026-07-18T00:00:00Z","to":"2026-07-18T01:00:00Z","statuses":[]}`, 1)
	_, err = DecodePullRequest(strings.NewReader(timeRange))
	require.Error(t, err)
}

func TestValidateRequestRejectsSelectorShapeMixing(t *testing.T) {
	req := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: 1}, Selector: Selector{Kind: "snapshot", From: time.Now().UTC().Format(time.RFC3339Nano)}, Include: Include{Sections: []string{"health"}}}
	err := validateRequest(req)
	require.Error(t, err)
	require.ErrorContains(t, err, "snapshot does not accept")
}

func TestEvidenceRFC8785Canonicalization(t *testing.T) {
	firstRaw := objects.JSONRawMessage(`{"b":1.0,"a":"\u00e9","slash":"\/","control":"\b"}`)
	secondRaw := objects.JSONRawMessage("{\"control\":\"\\b\",\"slash\":\"/\",\"a\":\"é\",\"b\":1e0}")
	first := evidenceFromRawWithSource(firstRaw, nil, nil, "responseChunks", "live")
	second := evidenceFromRawWithSource(secondRaw, nil, nil, "responseChunks", "live")
	require.Equal(t, "available", first.State)
	require.Equal(t, "live", first.Source)
	require.Equal(t, "rfc8785", first.Canonicalization)
	require.Equal(t, "available", first.CanonicalizationStatus)
	require.Equal(t, first.CanonicalSHA256, second.CanonicalSHA256)
	require.NotEmpty(t, first.CanonicalSHA256)
	require.NotEqual(t, first.RawSHA256, second.RawSHA256)
	require.Equal(t, string(firstRaw), string(first.Value.(json.RawMessage)))
	require.Equal(t, string(secondRaw), string(second.Value.(json.RawMessage)))
	require.Equal(t, len(firstRaw), first.ByteLength)
}

func TestEvidenceRejectsRFC8785IntegerPrecisionLoss(t *testing.T) {
	raw := objects.JSONRawMessage(`{"large":9007199254740993,"lexical":1.0}`)
	evidence := evidenceFromRawWithSource(raw, nil, nil, "responseChunks", "live")
	require.Equal(t, "available", evidence.State)
	require.Equal(t, "live", evidence.Source)
	require.Equal(t, "rfc8785", evidence.Canonicalization)
	require.Equal(t, "unavailable", evidence.CanonicalizationStatus)
	require.Contains(t, evidence.CanonicalizationReason, "IEEE-754 binary64")
	require.Empty(t, evidence.CanonicalSHA256)
	require.Equal(t, string(raw), string(evidence.Value.(json.RawMessage)))
	rawHash := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(rawHash[:]), evidence.RawSHA256)
	require.Equal(t, len(raw), evidence.ByteLength)
}

func TestContractRejectsSchemaInvalidResponse(t *testing.T) {
	require.Error(t, ValidatePullResponseJSON([]byte(`{}`)))
	validError := ErrorResponse{Contract: ContractResponse{Name: ContractName, Major: ContractMajor, Minor: ContractMinor, SchemaSHA256: SchemaSHA256}, Error: ErrorDetail{Code: "TEST", Message: "test", CorrelationID: "correlation", Retryable: false, Supported: []SupportedRange{}}}
	raw, err := json.Marshal(validError)
	require.NoError(t, err)
	require.NoError(t, ValidateErrorResponseJSON(raw))
}

func TestPullSnapshotAuthorizationAndVersion(t *testing.T) {
	service := &Service{}
	owner := &ent.User{ID: 2, IsOwner: true}
	ctx := authz.NewUserContext(context.Background(), owner.ID)
	ctx = contexts.WithUser(ctx, owner)
	ctx = contexts.WithProjectID(ctx, 7)
	req := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: 7}, Selector: Selector{Kind: "snapshot"}, Include: Include{Sections: []string{"health"}}}
	result, err := service.Pull(ctx, req)
	require.NoError(t, err)
	require.Equal(t, SchemaSHA256, result.Contract.SchemaSHA256)
	require.Equal(t, "partial", result.Sections.Health.Status)
	require.Equal(t, "unknown", result.Sections.Health.Data.(map[string]any)["databaseStatus"])
	require.Equal(t, "notRequested", result.Sections.Requests.Status)

	req.Scope.ProjectID = 8
	_, err = service.Pull(ctx, req)
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, "PROJECT_FORBIDDEN", serviceErr.Code)
	req.Scope.ProjectID = 7
	req.Contract.MaxMinor = -1
	_, err = service.Pull(ctx, req)
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, 400, serviceErr.Status)
	req.Contract.MaxMinor = 0
	invalidSubject := 0
	req.Scope.SubjectUserID = &invalidSubject
	_, err = service.Pull(ctx, req)
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, 400, serviceErr.Status)
}

func TestServiceAccountCredentialModeIsForbidden(t *testing.T) {
	service := &Service{}
	key := &ent.APIKey{ID: 3, Scopes: []string{string(scopes.ScopeReadDiagnostics)}}
	ctx := authz.NewAPIKeyContext(context.Background(), key.ID, 5)
	ctx = contexts.WithAPIKey(ctx, key)
	ctx = contexts.WithProjectID(ctx, 5)
	req := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: 5}, Selector: Selector{Kind: "snapshot"}, Include: Include{Sections: []string{"health"}, Credentials: true}}
	_, err := service.Pull(ctx, req)
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, "CREDENTIAL_EXPORT_FORBIDDEN", serviceErr.Code)
}
