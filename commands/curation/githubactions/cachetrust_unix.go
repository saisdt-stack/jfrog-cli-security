//go:build linux || darwin

package githubactions

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// osPathLocked reports whether the current user can neither write path nor make it writable. A
// symlink is never locked: whatever it points at lies outside what is checked. The owner of a path
// can always chmod it back, so a path the effective user owns is never locked, whatever its mode.
// Writability is the kernel's own access(2) answer, which honours ACLs; only a refusal (EACCES, or
// EROFS for a read-only mount) counts, and a path that cannot be examined is not locked.
func osPathLocked(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == os.Geteuid() {
		return false
	}
	err = unix.Access(path, unix.W_OK)
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EROFS)
}
