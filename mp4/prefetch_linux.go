//go:build linux && (amd64 || arm64)

package mp4

import (
	"os"
	"syscall"
)

// willNeedChunk is how much one posix_fadvise call asks for: the kernel reads
// at most its read-ahead size per call (128 KiB by default) and ignores the
// rest of a larger range, so a range is asked for piece by piece.
const willNeedChunk = 128 << 10

// willNeed asks the kernel to start reading [off, off+n) of f now, in the
// background (posix_fadvise WILLNEED): reads issued one after the other then
// find their pages already coming in, instead of each waiting for the disk in
// turn. Advisory: any failure is ignored.
func willNeed(f *os.File, off, n int64) {
	const fadvWillNeed = 3
	for end := off + n; off < end; off += willNeedChunk {
		_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(min(willNeedChunk, end-off)), fadvWillNeed, 0, 0)
	}
}
