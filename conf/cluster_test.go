package conf

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestClusterConfigDefaultsAndEnvironment(t *testing.T) {
	standalone, _, err := loadConfig(writeTestConfig(t, "log:\n  level: info\n"))
	require.NoError(t, err)
	require.False(t, standalone.Cluster.Enabled)
	t.Setenv("AXONHUB_CLUSTER_ENABLED", "true")
	t.Setenv("AXONHUB_CLUSTER_NODE_ID", "b")
	t.Setenv("AXONHUB_CLUSTER_CONTROL_NODE", "a")
	t.Setenv("AXONHUB_CLUSTER_NODES", `{"a":"http://127.0.0.1:8001","b":"http://127.0.0.1:8002"}`)
	t.Setenv("AXONHUB_CLUSTER_CHANNELS", `{"42":{"owner":"b","identity":"account-42"}}`)
	t.Setenv("AXONHUB_DB_DIALECT", "postgres")
	t.Setenv("AXONHUB_DB_DISABLE_AUTO_MIGRATION", "true")
	t.Setenv("AXONHUB_CACHE_MODE", "redis")
	cfg, _, err := loadConfig(writeTestConfig(t, "log:\n  level: info\n"))
	require.NoError(t, err)
	require.True(t, cfg.Cluster.Owns(42))
	require.False(t, cfg.Cluster.IsControl())
	t.Setenv("AXONHUB_SERVER_API_AUTH_ALLOW_NO_AUTH", "true")
	_, _, err = loadConfig(writeTestConfig(t, "log:\n  level: info\n"))
	require.ErrorContains(t, err, "authenticated API")
}
