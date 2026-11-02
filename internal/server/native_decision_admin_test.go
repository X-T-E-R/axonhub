package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/stretchr/testify/require"
)

// This is a real authenticated management HTTP test. Node executes the production
// RHF handlers, GraphQL hooks/client, viewers and action callbacks, not a mock DB.
func TestNativeDecisionAdminUI(t *testing.T) {
	node := os.Getenv("AXON_NATIVE_DECISION_NODE")
	if node == "" {
		t.Skip("set AXON_NATIVE_DECISION_NODE to Node with the existing frontend dependencies installed")
	}
	f := newNativeDecisionFixture(t)
	dir := t.TempDir()
	storage := f.db.DataStorage.Create().SetName("native-evidence-fs").SetDescription("synthetic blob evidence").SetType(datastorage.TypeFs).SetSettings(&objects.DataStorageSettings{Directory: &dir}).SetStatus(datastorage.StatusActive).SaveX(f.ctx)
	require.NoError(t, f.system.SetDefaultDataStorageID(f.ctx, storage.ID))
	var ids []string
	expected := make(map[string]map[string]string)
	for _, spec := range []struct{ path, body string }{
		{"/v1/systemone", nativeSystemBody("direct", "")},
		{"/v1/decisions", nativeOpenAIBody("")},
	} {
		status, _, body := f.post(t, spec.path, spec.body, f.key.Key, nil)
		require.Equal(t, 200, status, string(body))
		row := f.db.Request.Query().Order(ent.Desc(request.FieldID)).FirstX(f.ctx)
		require.Equal(t, storage.ID, row.DataStorageID)
		saved, err := f.requests.LoadRequestBody(f.ctx, row)
		require.NoError(t, err)
		require.Contains(t, string(saved), "9007199254740993")
		id := fmt.Sprintf("gid://axonhub/Request/%d", row.ID)
		ids = append(ids, id)
		response, err := f.requests.LoadResponseBody(f.ctx, row)
		require.NoError(t, err)
		expected[id] = map[string]string{"requestBody": string(saved), "responseBody": string(response)}
		execution := row.QueryExecutions().OnlyX(f.ctx)
		execRequest, err := f.requests.LoadRequestExecutionRequestBody(f.ctx, execution)
		require.NoError(t, err)
		execResponse, err := f.requests.LoadRequestExecutionResponseBody(f.ctx, execution)
		require.NoError(t, err)
		expected[fmt.Sprintf("gid://axonhub/RequestExecution/%d", execution.ID)] = map[string]string{"requestBody": string(execRequest), "responseBody": string(execResponse)}
	}
	encoded, err := json.Marshal(ids)
	require.NoError(t, err)
	expectedJSON, err := json.Marshal(expected)
	require.NoError(t, err)
	script, err := filepath.Abs(filepath.Join("..", "..", "integration_test", "native_decisions", "native_decisions_admin_client.mjs"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script, f.server.URL)
	cmd.Env = append(os.Environ(), "AXON_NATIVE_ADMIN_TOKEN="+f.adminToken, "AXON_NATIVE_REQUEST_IDS="+string(encoded), "AXON_NATIVE_EXPECTED_BODIES="+string(expectedJSON))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Log(string(output))
	require.EqualValues(t, 2, f.calls.Load(), "management configuration and evidence inspection must not call model providers")
}
