package gql

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
)

func TestChannelKeyMonitoringEventsQuery_CurrentFrontendSelectionCompiles(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	defer client.Close()

	server := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: &Resolver{client: client},
	}))

	reqBody := map[string]any{
		"query": `
			query ChannelKeyMonitoringEvents($first: Int) {
				channelKeyMonitoringEvents(first: $first) {
					edges {
						cursor
						node {
							id
							createdAt
							updatedAt
							channelID
							channelName
							keyID
							maskedKey
							ruleID
							ruleName
							trigger
							source
							success
							skipped
							reason
							statusCode
							balance
							currency
							available
							probe
							matchedPolicy
							action
							nextCheckAt
							backoffAttempt
							checkedAt
						}
					}
					pageInfo {
						hasNextPage
						hasPreviousPage
						startCursor
						endCursor
					}
					totalCount
				}
			}
		`,
		"variables": map[string]any{"first": 1},
	}

	payload, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(authz.WithTestBypass(ent.NewContext(context.Background(), client)))

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Errors)
	require.Contains(t, resp.Data, "channelKeyMonitoringEvents")
}
