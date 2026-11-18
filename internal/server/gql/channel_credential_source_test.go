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
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestGraphQLCredentialSourceCreateAndClear(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer db.Close()
	svc := biz.NewChannelServiceForTest(db)
	defer svc.Stop()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	source := db.Channel.Create().SetName("holder").SetType(channel.TypeOpenai).
		SetSupportedModels([]string{"holder-model"}).SetDefaultTestModel("holder-model").
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).SaveX(ctx)
	server := handler.NewDefaultServer(NewExecutableSchema(Config{Resolvers: &Resolver{client: db, channelService: svc}}))

	run := func(query string, variables map[string]any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		var result struct {
			Data   map[string]map[string]any
			Errors []struct{ Message string }
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &result))
		require.Empty(t, result.Errors)
		return result.Data["channel"]
	}

	created := run(`mutation ($input: CreateChannelInput!) {
		channel: createChannel(input: $input) { id settings { credentialSourceChannelId disableRetries } }
	}`, map[string]any{"input": map[string]any{
		"name": "dependent", "type": "openai_responses", "baseURL": "https://example.invalid/v1",
		"supportedModels": []string{"dependent-model"}, "defaultTestModel": "dependent-model",
		"credentials": map[string]any{}, "settings": map[string]any{"credentialSourceChannelId": source.ID},
	}})
	require.Equal(t, float64(source.ID), created["settings"].(map[string]any)["credentialSourceChannelId"])
	const update = `mutation ($id: ID!, $input: UpdateChannelInput!) {
		channel: updateChannel(id: $id, input: $input) { id settings { credentialSourceChannelId disableRetries } }
	}`
	settings := map[string]any{"credentialSourceChannelId": source.ID, "disableRetries": true}
	updated := run(update, map[string]any{"id": created["id"], "input": map[string]any{"settings": settings}})
	require.Equal(t, settings["disableRetries"], updated["settings"].(map[string]any)["disableRetries"])
	require.Equal(t, float64(source.ID), updated["settings"].(map[string]any)["credentialSourceChannelId"])
	settings["credentialSourceChannelId"] = nil
	updated = run(update, map[string]any{"id": created["id"], "input": map[string]any{"settings": settings}})
	require.Nil(t, updated["settings"].(map[string]any)["credentialSourceChannelId"])
}
