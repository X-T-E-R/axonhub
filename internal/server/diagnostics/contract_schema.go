package diagnostics

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// ContractSchema is the exact shared protocol artifact consumed by the
// diagnostics skill, generator, and runtime boundary validators.
//
//go:embed contract/v1/contract.schema.json
var ContractSchema []byte

var (
	contractValidatorsOnce sync.Once
	contractValidators     map[string]*jsonschema.Resolved
	contractValidatorsErr  error
)

func loadContractValidators() (map[string]*jsonschema.Resolved, error) {
	contractValidatorsOnce.Do(func() {
		var root jsonschema.Schema
		if err := json.Unmarshal(ContractSchema, &root); err != nil {
			contractValidatorsErr = fmt.Errorf("decode diagnostics contract schema: %w", err)
			return
		}
		contractValidators = make(map[string]*jsonschema.Resolved, 3)
		for _, definition := range []string{"pullRequest", "pullResponse", "errorResponse"} {
			wrapper := &jsonschema.Schema{
				Schema: root.Schema,
				Defs:   root.Defs,
				Ref:    "#/$defs/" + definition,
			}
			resolved, err := wrapper.Resolve(nil)
			if err != nil {
				contractValidatorsErr = fmt.Errorf("resolve diagnostics %s schema: %w", definition, err)
				return
			}
			contractValidators[definition] = resolved
		}
	})
	return contractValidators, contractValidatorsErr
}

func validateContractJSON(raw []byte, definition string) error {
	validators, err := loadContractValidators()
	if err != nil {
		return err
	}
	validator, ok := validators[definition]
	if !ok {
		return fmt.Errorf("unknown diagnostics contract definition %q", definition)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("must contain one JSON value")
	}
	if err := validator.Validate(value); err != nil {
		return fmt.Errorf("does not match %s schema: %w", definition, err)
	}
	return nil
}

func ValidatePullRequestJSON(raw []byte) error {
	return validateContractJSON(raw, "pullRequest")
}

func ValidatePullResponseJSON(raw []byte) error {
	return validateContractJSON(raw, "pullResponse")
}

func ValidateErrorResponseJSON(raw []byte) error {
	return validateContractJSON(raw, "errorResponse")
}
