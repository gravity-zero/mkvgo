//go:build linux && (amd64 || arm64)

package mp4

import (
	"os"
	"syscall"
)

// willNeed asks the kernel to start reading [off, off+n) of f now, in the
// background (posix_fadvise WILLNEED): reads a segment issues one after the
// other then find the pages coming in together instead of each waiting for
// the disk in turn. Advisory: any failure is ignored.
func willNeed(f *os.File, off, n int64) {
	if n > 0 {
		const fadvWillNeed = 3
		_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(n), fadvWillNeed, 0, 0)
	}
}

// readAhead brings [off, off+n) of f into the page cache and returns once it
// is there (readahead(2)): large sequential reads where the pages are missing,
// next to nothing where they are cached. Advisory: any failure is ignored.
func readAhead(f *os.File, off, n int64) {
	if n > 0 {
		_, _, _ = syscall.Syscall(syscall.SYS_READAHEAD, f.Fd(), uintptr(off), uintptr(n))
	}
}
