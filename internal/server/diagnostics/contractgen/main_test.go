package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateArtifactsDerivesIdentityAndVersionFromSchema(t *testing.T) {
	raw, err := os.ReadFile("../contract/v1/contract.schema.json")
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	defs := schema["$defs"].(map[string]any)
	requestProperties := defs["contractRequest"].(map[string]any)["properties"].(map[string]any)
	responseProperties := defs["contractResponse"].(map[string]any)["properties"].(map[string]any)
	requestProperties["name"].(map[string]any)["const"] = "example.changed-contract"
	responseProperties["name"].(map[string]any)["const"] = "example.changed-contract"
	requestProperties["major"].(map[string]any)["const"] = float64(9)
	responseProperties["major"].(map[string]any)["const"] = float64(9)
	responseProperties["minor"].(map[string]any)["const"] = float64(3)
	edited, err := json.Marshal(schema)
	require.NoError(t, err)

	artifacts, err := generateArtifacts(edited)
	require.NoError(t, err)
	generated := string(artifacts.source)
	require.Contains(t, generated, `const ContractName = "example.changed-contract"`)
	require.Contains(t, generated, "const ContractMajor = 9")
	require.Contains(t, generated, "const ContractMinor = 3")
	require.Contains(t, generated, `version=9.3`)
	require.Contains(t, string(artifacts.manifest), `"name": "example.changed-contract"`)
	require.Contains(t, string(artifacts.manifest), `"maxMinor": 3`)
	require.True(t, strings.Contains(string(artifacts.positiveFixture), `"major":9`) && strings.Contains(string(artifacts.positiveFixture), `"maxMinor":3`))
}

func TestGenerateProtocolTypesIsDeterministicAndTracksSchemaFields(t *testing.T) {
	raw, err := os.ReadFile("../contract/v1/contract.schema.json")
	require.NoError(t, err)
	first, err := generateProtocolTypes(raw)
	require.NoError(t, err)
	second, err := generateProtocolTypes(raw)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Contains(t, string(first), "type PullRequest struct")
	require.Contains(t, string(first), "type PullResponse struct")
	require.Contains(t, string(first), "type ErrorResponse struct")
	require.Contains(t, string(first), "type Evidence struct")
	require.Contains(t, string(first), "Value json.RawMessage `json:\"value,omitempty\"`")

	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	defs := schema["$defs"].(map[string]any)
	scope := defs["scope"].(map[string]any)
	properties := scope["properties"].(map[string]any)
	properties["schemaProbe"] = map[string]any{"type": "string"}
	scope["required"] = append(scope["required"].([]any), "schemaProbe")
	edited, err := json.Marshal(schema)
	require.NoError(t, err)
	probe, err := generateProtocolTypes(edited)
	require.NoError(t, err)
	require.Contains(t, string(probe), "SchemaProbe string `json:\"schemaProbe\"`")
	properties["schemaProbe"] = map[string]any{"type": "integer"}
	editedType, err := json.Marshal(schema)
	require.NoError(t, err)
	typeProbe, err := generateProtocolTypes(editedType)
	require.NoError(t, err)
	require.Contains(t, string(typeProbe), "SchemaProbe int `json:\"schemaProbe\"`")
	require.NotContains(t, string(typeProbe), "SchemaProbe string `json:\"schemaProbe\"`")
}
