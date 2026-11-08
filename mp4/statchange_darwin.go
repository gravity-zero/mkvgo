//go:build darwin

package mp4

import (
	"os"
	"syscall"
)

// changeTime is fi's inode change time in nanoseconds (ctime), ok when the system reports one.
func changeTime(fi os.FileInfo) (int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int64(st.Ctimespec.Sec)*1_000_000_000 + int64(st.Ctimespec.Nsec), true
}
