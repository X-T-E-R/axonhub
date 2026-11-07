package gql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/httpclient"
)

// Exercises the generated GraphQL handler and real Ent query, not a projection
// copied into a mock server. Interleaved owners expose page-before-filter bugs.
func TestChannelPlacementGraphQL(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	seed := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	config := cluster.Config{Enabled: true, NodeID: "control", ControlNode: "control",
		Nodes:    map[string]string{"control": "http://control", "lag": "http://lag", "tencent": "http://tencent", "empty": "http://empty"},
		Channels: map[int]cluster.ChannelBinding{}}
	for i := 1; i <= 31; i++ {
		status := channel.StatusEnabled
		if i%5 == 0 {
			status = channel.StatusDisabled
		}
		model := "other"
		if i%3 != 0 {
			model = "model-a"
		}
		ch := client.Channel.Create().SetName(fmt.Sprintf("fixture-%02d", i)).SetType(channel.TypeOpenai).
			SetStatus(status).SetCredentials(objects.ChannelCredentials{APIKey: fmt.Sprintf("synthetic-%d", i)}).
			SetSupportedModels([]string{model}).SetDefaultTestModel(model).SetTags([]string{"fixture"}).SaveX(seed)
		owner := "lag"
		if i%2 == 0 {
			owner = "tencent"
		}
		config.Channels[ch.ID] = cluster.ChannelBinding{Owner: owner, Identity: fmt.Sprintf("account-%d", i)}
	}
	svc := biz.NewChannelService(biz.ChannelServiceParams{Node: config, Ent: client,
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, SystemService: &biz.SystemService{}, HttpClient: httpclient.NewHttpClient()})
	defer svc.Stop()
	server := handler.NewDefaultServer(NewExecutableSchema(Config{Resolvers: &Resolver{client: client, channelService: svc}}))
	reader := contexts.WithUser(ent.NewContext(context.Background(), client), &ent.User{ID: 1, Scopes: []string{string(scopes.ScopeReadChannels)}})
	reader, err := authz.WithPrincipal(reader, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)

	type response struct {
		Data struct {
			Inventory struct {
				ClusterEnabled bool
				Machines       []string
			} `json:"channelExecutionInventory"`
			Channels struct {
				Edges []struct {
					Node struct {
						Name             string
						ExecutionMachine *string
					}
					Cursor string
				}
				PageInfo struct {
					HasNextPage     bool
					HasPreviousPage bool
					StartCursor     string
					EndCursor       string
				}
				TotalCount int
			} `json:"queryChannels"`
		}
		Errors []struct{ Message string }
	}
	do := func(ctx context.Context, input map[string]any) response {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"query": `query Placement($input: QueryChannelInput!) {
			channelExecutionInventory { clusterEnabled machines }
			queryChannels(input: $input) { totalCount edges { cursor node { name executionMachine } }
			pageInfo { hasNextPage hasPreviousPage startCursor endCursor } }
		}`, "variables": map[string]any{"input": input}})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/admin/query", bytes.NewReader(payload)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var result response
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result), rec.Body.String())
		return result
	}
	base := func() map[string]any {
		return map[string]any{"first": 3, "orderBy": map[string]any{"field": "NAME", "direction": "ASC"}, "paginateModelMatches": true}
	}
	t.Run("registered inventory and all machine pages", func(t *testing.T) {
		input := base()
		seen := map[string]bool{}
		for {
			out := do(reader, input)
			require.Empty(t, out.Errors)
			require.True(t, out.Data.Inventory.ClusterEnabled)
			require.Equal(t, []string{"control", "empty", "lag", "tencent"}, out.Data.Inventory.Machines)
			require.Equal(t, 31, out.Data.Channels.TotalCount)
			for _, edge := range out.Data.Channels.Edges {
				require.False(t, seen[edge.Node.Name])
				seen[edge.Node.Name] = true
				require.NotNil(t, edge.Node.ExecutionMachine)
			}
			if !out.Data.Channels.PageInfo.HasNextPage {
				break
			}
			input["after"] = out.Data.Channels.PageInfo.EndCursor
		}
		require.Len(t, seen, 31)
	})
	t.Run("machine search status tag model before forward and backward pagination", func(t *testing.T) {
		input := base()
		input["executionMachine"] = "tencent"
		input["where"] = map[string]any{"statusIn": []string{"enabled"}, "nameContainsFold": "fixture"}
		input["hasTag"] = "fixture"
		input["model"] = "model-a"
		var names []string
		for {
			out := do(reader, input)
			require.Empty(t, out.Errors)
			require.Equal(t, 8, out.Data.Channels.TotalCount)
			for _, edge := range out.Data.Channels.Edges {
				require.Equal(t, "tencent", *edge.Node.ExecutionMachine)
				names = append(names, edge.Node.Name)
			}
			if !out.Data.Channels.PageInfo.HasNextPage {
				delete(input, "first")
				delete(input, "after")
				input["last"] = 3
				input["before"] = out.Data.Channels.PageInfo.StartCursor
				previous := do(reader, input)
				require.Empty(t, previous.Errors)
				require.Equal(t, "fixture-14", previous.Data.Channels.Edges[0].Node.Name)
				break
			}
			input["after"] = out.Data.Channels.PageInfo.EndCursor
		}
		require.Equal(t, []string{"fixture-02", "fixture-04", "fixture-08", "fixture-14", "fixture-16", "fixture-22", "fixture-26", "fixture-28"}, names)
	})
	t.Run("empty machine and unknown machine", func(t *testing.T) {
		input := base()
		input["executionMachine"] = "empty"
		out := do(reader, input)
		require.Empty(t, out.Errors)
		require.Empty(t, out.Data.Channels.Edges)
		require.Zero(t, out.Data.Channels.TotalCount)
		input["executionMachine"] = "invented"
		require.NotEmpty(t, do(reader, input).Errors)
	})
	t.Run("legacy model callers retain unpaginated result", func(t *testing.T) {
		input := base()
		delete(input, "paginateModelMatches")
		input["model"] = "model-a"
		out := do(reader, input)
		require.Empty(t, out.Errors)
		require.Len(t, out.Data.Channels.Edges, 21)
	})
	t.Run("permission denied", func(t *testing.T) {
		out := do(ent.NewContext(context.Background(), client), base())
		require.NotEmpty(t, out.Errors)
	})
}
