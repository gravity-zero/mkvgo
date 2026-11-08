//go:build linux

package mp4

import (
	"os"
	"syscall"
)

// changeTime is fi's inode change time in nanoseconds (ctime), ok when the system reports one: it moves on a
// write in place that keeps the size and the modification time, which size and mtime alone would not show.
func changeTime(fi os.FileInfo) (int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int64(st.Ctim.Sec)*1_000_000_000 + int64(st.Ctim.Nsec), true // 32-bit fields on arm
}
