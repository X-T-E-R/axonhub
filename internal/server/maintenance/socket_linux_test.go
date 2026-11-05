//go:build linux

package maintenance

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaintenancePrivateSocketLiveStaleAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.sock")
	local := &Local{Gate: NewGate(), Drain: func(context.Context) (DrainResult, error) { return DrainResult{WritersStopped: true}, nil }}
	closeServer, err := local.Listen(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	status, err := Call(context.Background(), path, "pause")
	require.NoError(t, err)
	require.True(t, status.Paused)
	_, err = local.Listen(path)
	require.Error(t, err, "a live owned socket must not be replaced")
	require.NoError(t, closeServer())
	// A crash leaves an owned socket inode but no accepting process.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	stale.SetUnlinkOnClose(false)
	require.NoError(t, os.Chmod(path, 0600))
	require.NoError(t, stale.Close())
	fresh := &Local{Gate: NewGate()}
	closeFresh, err := fresh.Listen(path)
	require.NoError(t, err)
	require.NoError(t, closeFresh())
	require.NoError(t, os.WriteFile(path, []byte("not a socket"), 0600))
	_, err = fresh.Listen(path)
	require.Error(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "not a socket", string(data))
}
