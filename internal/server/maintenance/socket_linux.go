//go:build linux

package maintenance

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || int(st.Uid) != os.Geteuid() {
		return errors.New("maintenance path is not an owned private socket")
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return errors.New("maintenance socket already in use")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("maintenance socket ownership remains unknown")
	}
	again, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, again) {
		return errors.New("maintenance socket identity changed")
	}
	return os.Remove(path)
}
