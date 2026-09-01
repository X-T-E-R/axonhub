package diagnostics

import (
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
)

func TestManagedRequestBodyPendingAndWriteFailureAreStorageUnavailable(t *testing.T) {
	now := time.Now().UTC()
	for _, testCase := range []struct {
		name        string
		disposition objects.Disposition
		wantState   string
		wantSource  string
	}{
		{
			name: "async pending",
			disposition: objects.Disposition{
				Intent: "persist", Location: "managed", Outcome: "unavailable",
				FailureClass: lo.ToPtr("async_pending"), CapturedAt: now,
			},
			wantState:  "storageUnavailable",
			wantSource: "database",
		},
		{
			name: "write failure",
			disposition: objects.Disposition{
				Intent: "persist", Location: "none", Outcome: "writeFailed",
				FailureClass: lo.ToPtr("managed_write_failed"), CapturedAt: now,
			},
			wantState:  "storageUnavailable",
			wantSource: "none",
		},
		{
			name: "retry exhausted",
			disposition: objects.Disposition{
				Intent: "persist", Location: "none", Outcome: "unavailable",
				FailureClass: lo.ToPtr("managed_write_failed:retry_exhausted"), CapturedAt: now,
			},
			wantState:  "storageUnavailable",
			wantSource: "none",
		},
		{
			name: "capacity pressure terminal omission",
			disposition: objects.Disposition{
				Intent: "persist", Location: "none", Outcome: "omitted",
				FailureClass: lo.ToPtr("capacity_pressure"), CapturedAt: now,
			},
			wantState:  "notPersisted",
			wantSource: "none",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			evidence := evidenceFromRaw(objects.JSONRawMessage(`{}`), &objects.EvidenceDisposition{
				Version:     1,
				RequestBody: testCase.disposition,
			}, nil, "requestBody")
			require.Equal(t, testCase.wantState, evidence.State)
			require.Equal(t, testCase.wantSource, evidence.Source)
		})
	}
}

func TestReadableManagedPointerOverridesStaleDispositionSource(t *testing.T) {
	payloadID := 42
	evidence := evidenceFromRaw(objects.JSONRawMessage(`{"prompt":"available"}`), &objects.EvidenceDisposition{
		Version: 1,
		RequestBody: objects.Disposition{
			Intent: "persist", Location: "none", Outcome: "writeFailed",
		},
	}, nil, "requestBody")
	evidence = normalizeManagedRequestBodyEvidence(evidence, &payloadID)
	require.Equal(t, "available", evidence.State)
	require.Equal(t, "database", evidence.Source)
}
