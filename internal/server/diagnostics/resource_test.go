package diagnostics

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
)

func TestEffectiveLimitsAreServerOwned(t *testing.T) {
	got := effectiveLimits(Limits{
		MaxRequests:       ContractMaximumMaxRequests,
		MaxExecutions:     ContractMaximumMaxExecutions,
		MaxRelatedRecords: ContractMaximumMaxRelatedRecords,
		MaxResponseBytes:  ContractMaximumMaxResponseBytes,
	})
	require.Equal(t, serverMaxRequests, got.MaxRequests)
	require.Equal(t, serverMaxExecutions, got.MaxExecutions)
	require.Equal(t, serverMaxRelatedRecords, got.MaxRelatedRecords)
	require.Equal(t, serverMaxResponseBytes, got.MaxResponseBytes)
}

func TestEvidenceLengthExpressionIsDialectSafe(t *testing.T) {
	postgres := evidenceLengthExpression(dialect.Postgres, `"request_body"`)
	require.Contains(t, postgres, `OCTET_LENGTH(CAST("request_body" AS TEXT))`)
	require.NotContains(t, postgres, `LENGTH("request_body")`)

	sqlite := evidenceLengthExpression(dialect.SQLite, "`request_body`")
	require.Contains(t, sqlite, "LENGTH(CAST(`request_body` AS BLOB))")
	require.False(t, strings.Contains(sqlite, "OCTET_LENGTH"))

	mysqlAndTiDB := evidenceLengthExpression(dialect.MySQL, "`request_body`")
	require.Contains(t, mysqlAndTiDB, "OCTET_LENGTH(CAST(`request_body` AS CHAR))")
	require.NotContains(t, mysqlAndTiDB, " AS TEXT")

	require.Equal(t, "NULL", evidenceLengthExpression("unsupported", "request_body"))
}

func TestPullStopsOnCanceledContext(t *testing.T) {
	owner := &ent.User{ID: 2, IsOwner: true}
	ctx := authz.NewUserContext(context.Background(), owner.ID)
	ctx = contexts.WithUser(ctx, owner)
	ctx = contexts.WithProjectID(ctx, 7)
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	_, err := (&Service{}).Pull(ctx, PullRequest{
		Contract: ContractRequest{Name: ContractName, Major: ContractMajor, MinMinor: ContractMinor, MaxMinor: ContractMinor},
		Scope:    Scope{ProjectID: 7},
		Selector: Selector{Kind: "snapshot"},
		Include:  Include{Sections: []string{"health"}},
	})
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, "DIAGNOSTICS_CANCELED", serviceErr.Code)
	require.Equal(t, 499, serviceErr.Status)
}

func TestPullRejectsConcurrentHeavyExport(t *testing.T) {
	diagnosticsPullSlots <- struct{}{}
	t.Cleanup(func() { <-diagnosticsPullSlots })

	owner := &ent.User{ID: 2, IsOwner: true}
	ctx := authz.NewUserContext(context.Background(), owner.ID)
	ctx = contexts.WithUser(ctx, owner)
	ctx = contexts.WithProjectID(ctx, 7)
	_, err := (&Service{}).Pull(ctx, PullRequest{
		Contract: ContractRequest{Name: ContractName, Major: ContractMajor, MinMinor: ContractMinor, MaxMinor: ContractMinor},
		Scope:    Scope{ProjectID: 7},
		Selector: Selector{Kind: "snapshot"},
		Include:  Include{Sections: []string{"health"}},
	})
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, "DIAGNOSTICS_BUSY", serviceErr.Code)
	require.Equal(t, http.StatusTooManyRequests, serviceErr.Status)
	require.True(t, serviceErr.Retryable)
}

func TestDeadlineErrorIsRetryable(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.DeadlineExceeded)
	err := contextServiceError(ctx, context.DeadlineExceeded)
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, "DIAGNOSTICS_DEADLINE_EXCEEDED", serviceErr.Code)
	require.True(t, serviceErr.Retryable)
}
