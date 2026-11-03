package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Run only on explicit request, using the in-memory Ent/JWT fixture. The browser
// operates synthetic channels; this never opens a real provider or database.
func TestRawHTTPBrowserFixture(t *testing.T) {
	path := os.Getenv("AXON_RAW_BROWSER_FIXTURE")
	if path == "" {
		t.Skip("set AXON_RAW_BROWSER_FIXTURE to an isolated fixture JSON path")
	}
	f := newNativeDecisionFixture(t)
	data, err := json.Marshal(map[string]string{"baseURL": f.server.URL, "token": f.adminToken})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, data, 0600))
	t.Cleanup(func() { _ = os.Remove(path); _ = os.Remove(path + ".done") })
	deadline := time.NewTimer(8 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("browser fixture was not closed")
		case <-ticker.C:
			if _, err := os.Stat(path + ".done"); err == nil {
				require.Zero(t, f.calls.Load(), "management UI must never probe a provider")
				return
			}
		}
	}
}
