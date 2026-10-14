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
	if n <= 0 || onNetwork(f) {
		return
	}
	for end := off + n; off < end; off += willNeedChunk {
		_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(min(willNeedChunk, end-off)), fadvWillNeed, 0, 0)
	}
}

// networkFS are the filesystem magics of network mounts (NFS, SMB, CIFS,
// SMB2, 9p, FUSE, Ceph, AFS): read-ahead asked there is read across the
// network ahead of need, a round trip and bytes the read itself does not want.
var networkFS = map[int64]bool{0x6969: true, 0x517B: true, 0xFF534D42: true, 0xFE534D42: true, 0x01021997: true, 0x65735546: true, 0x00C36400: true, 0x5346414F: true}

// onNetwork reports whether f sits on a network mount.
func onNetwork(f *os.File) bool {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return true
	}
	return networkFS[int64(st.Type)]
}
