//go:build unix

// Package fileid names an OS file beyond its size and mtime.
package fileid

import (
	"fmt"
	"os"
	"syscall"
)

// Of names an OS file by device and inode: what a file renamed over it does not keep; empty when fi is not the OS's own stat.
func Of(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%x:%x", st.Dev, st.Ino)
	}
	return ""
}
