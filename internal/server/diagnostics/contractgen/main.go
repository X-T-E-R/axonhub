package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"os"

	"github.com/atombender/go-jsonschema/pkg/generator"
	"github.com/atombender/go-jsonschema/pkg/schemas"
)

type generatedArtifacts struct {
	source          []byte
	manifest        []byte
	positiveFixture []byte
	negativeFixture []byte
	protocolTypes   []byte
}

func main() {
	raw, err := os.ReadFile("contract/v1/contract.schema.json")
	if err != nil {
		panic(err)
	}
	artifacts, err := generateArtifacts(raw)
	if err != nil {
		panic(err)
	}
	for path, data := range map[string][]byte{
		"contract_generated.go":                          artifacts.source,
		"contract/v1/version-manifest.json":              artifacts.manifest,
		"contract/v1/fixture.valid.json":                 artifacts.positiveFixture,
		"contract/v1/fixture.unknown-field.invalid.json": artifacts.negativeFixture,
		"contract/v1/types.generated.go":                 artifacts.protocolTypes,
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			panic(err)
		}
	}
}

func generateArtifacts(raw []byte) (generatedArtifacts, error) {
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return generatedArtifacts{}, err
	}
	root, ok := schema.(map[string]any)
	if !ok {
		return generatedArtifacts{}, fmt.Errorf("contract schema root must be an object")
	}
	defs := object(root, "$defs")
	contractProperties := object(object(defs, "contractRequest"), "properties")
	responseProperties := object(object(defs, "contractResponse"), "properties")
	scopeProperties := object(object(defs, "scope"), "properties")
	limitProperties := object(object(defs, "limits"), "properties")
	includeProperties := object(object(defs, "include"), "properties")
	sectionNames := stringSlice(object(object(includeProperties, "sections"), "items")["enum"])

	name := stringValue(object(contractProperties, "name"), "const")
	major := integer(object(contractProperties, "major"), "const")
	minor := integer(object(responseProperties, "minor"), "const")
	minimumMinor := integer(object(contractProperties, "minMinor"), "minimum")
	if responseName := stringValue(object(responseProperties, "name"), "const"); responseName != name {
		return generatedArtifacts{}, fmt.Errorf("request and response contract names differ")
	}
	if responseMajor := integer(object(responseProperties, "major"), "const"); responseMajor != major {
		return generatedArtifacts{}, fmt.Errorf("request and response contract majors differ")
	}
	if minor < minimumMinor {
		return generatedArtifacts{}, fmt.Errorf("response minor %d is below supported minimum %d", minor, minimumMinor)
	}

	canonical, err := json.Marshal(schema)
	if err != nil {
		return generatedArtifacts{}, err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	source := fmt.Sprintf(`// Code generated from contract/v1/contract.schema.json; DO NOT EDIT.
package diagnostics

const ContractName = %q
const ContractMajor = %d
const ContractMinor = %d
const ContractMediaType = %q
const SchemaSHA256 = %q
const ContractMinorMinimum = %d
const ContractSubjectUserIDMinimum = %d
const ContractDefaultMaxRequests = %d
const ContractMaximumMaxRequests = %d
const ContractDefaultMaxExecutions = %d
const ContractMaximumMaxExecutions = %d
const ContractDefaultMaxRelatedRecords = %d
const ContractMaximumMaxRelatedRecords = %d
const ContractDefaultMaxResponseBytes = %d
const ContractMinimumMaxResponseBytes = %d
const ContractMaximumMaxResponseBytes = %d

var ContractSectionNames = %#v
`, name, major, minor, fmt.Sprintf("application/vnd.axonhub.diagnostics+json;version=%d.%d", major, minor), hash,
		minimumMinor,
		integer(object(scopeProperties, "subjectUserId"), "minimum"),
		integer(object(limitProperties, "maxRequests"), "default"), integer(object(limitProperties, "maxRequests"), "maximum"),
		integer(object(limitProperties, "maxExecutions"), "default"), integer(object(limitProperties, "maxExecutions"), "maximum"),
		integer(object(limitProperties, "maxRelatedRecords"), "default"), integer(object(limitProperties, "maxRelatedRecords"), "maximum"),
		integer(object(limitProperties, "maxResponseBytes"), "default"), integer(object(limitProperties, "maxResponseBytes"), "minimum"), integer(object(limitProperties, "maxResponseBytes"), "maximum"),
		sectionNames,
	)
	manifest := map[string]any{"name": name, "major": major, "minMinor": minimumMinor, "maxMinor": minor, "schemaSha256": hash}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return generatedArtifacts{}, err
	}
	manifestBytes = append(manifestBytes, '\n')
	positive := []byte(fmt.Sprintf(`{"contract":{"name":%q,"major":%d,"minMinor":%d,"maxMinor":%d},"scope":{"projectId":1},"selector":{"kind":"snapshot"},"include":{"sections":["health"],"credentials":false}}`+"\n", name, major, minimumMinor, minor))
	negative := []byte(fmt.Sprintf(`{"contract":{"name":%q,"major":%d,"minMinor":%d,"maxMinor":%d},"scope":{"projectId":1},"selector":{"kind":"snapshot","unknown":true},"include":{"sections":["health"]}}`+"\n", name, major, minimumMinor, minor))
	protocolTypes, err := generateProtocolTypes(raw)
	if err != nil {
		return generatedArtifacts{}, err
	}
	return generatedArtifacts{source: []byte(source), manifest: manifestBytes, positiveFixture: positive, negativeFixture: negative, protocolTypes: protocolTypes}, nil
}

func generateProtocolTypes(raw []byte) ([]byte, error) {
	schema, err := schemas.FromJSONReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode protocol schema for Go generation: %w", err)
	}
	const outputName = "contract/v1/types.generated.go"
	gen, err := generator.New(generator.Config{
		DefaultPackageName: "contractv1",
		DefaultOutputName:  outputName,
		Capitalizations:    []string{"API", "ID", "SHA256", "URL"},
		Tags:               []string{"json"},
		OnlyModels:         true,
		DisableOmitZero:    true,
		Warner:             func(string) {},
	})
	if err != nil {
		return nil, fmt.Errorf("create protocol Go generator: %w", err)
	}
	if err := gen.AddFile("contract.schema.json", schema); err != nil {
		return nil, fmt.Errorf("generate protocol Go model: %w", err)
	}
	sources, err := gen.Sources()
	if err != nil {
		return nil, fmt.Errorf("render protocol Go model: %w", err)
	}
	source, ok := sources[outputName]
	if !ok {
		return nil, fmt.Errorf("protocol Go generator did not emit %s", outputName)
	}
	return preserveEvidenceJSONLexemes(source)
}

// preserveEvidenceJSONLexemes specializes the schema's unconstrained evidence
// value as json.RawMessage. The upstream generator otherwise emits interface{},
// which would normalize large numbers, escapes, and object order on a round trip.
func preserveEvidenceJSONLexemes(source []byte) ([]byte, error) {
	const evidenceStart = "type Evidence struct {"
	start := bytes.Index(source, []byte(evidenceStart))
	if start < 0 {
		return nil, fmt.Errorf("generated protocol model is missing Evidence")
	}
	relativeEnd := bytes.Index(source[start:], []byte("\n}\n\n"))
	if relativeEnd < 0 {
		return nil, fmt.Errorf("generated Evidence declaration is malformed")
	}
	end := start + relativeEnd + len("\n}")
	block := source[start:end]
	oldField := []byte("Value interface{} `json:\"value,omitempty\"`")
	if bytes.Count(block, oldField) != 1 {
		return nil, fmt.Errorf("generated Evidence value field did not have the expected unconstrained type")
	}
	block = bytes.Replace(block, oldField, []byte("Value json.RawMessage `json:\"value,omitempty\"`"), 1)
	source = append(append(append([]byte(nil), source[:start]...), block...), source[end:]...)
	oldImport := []byte("import \"time\"")
	if bytes.Count(source, oldImport) != 1 {
		return nil, fmt.Errorf("generated protocol imports did not have the expected form")
	}
	source = bytes.Replace(source, oldImport, []byte("import (\n\t\"encoding/json\"\n\t\"time\"\n)"), 1)
	formatted, err := format.Source(source)
	if err != nil {
		return nil, fmt.Errorf("format specialized protocol Go model: %w", err)
	}
	return formatted, nil
}

func object(parent map[string]any, key string) map[string]any {
	value, ok := parent[key].(map[string]any)
	if !ok {
		panic(fmt.Errorf("contract schema %s must be an object", key))
	}
	return value
}

func integer(parent map[string]any, key string) int {
	value, ok := parent[key].(float64)
	if !ok || value != float64(int(value)) {
		panic(fmt.Errorf("contract schema %s must be an integer", key))
	}
	return int(value)
}

func stringValue(parent map[string]any, key string) string {
	value, ok := parent[key].(string)
	if !ok || value == "" {
		panic(fmt.Errorf("contract schema %s must be a non-empty string", key))
	}
	return value
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		panic("contract schema enum must be an array")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			panic("contract schema enum values must be strings")
		}
		result = append(result, text)
	}
	return result
}
