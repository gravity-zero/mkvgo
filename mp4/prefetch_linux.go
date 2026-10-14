//go:build linux && (amd64 || arm64)

package mp4

import (
	"os"
	"sync"
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

// fsByDev caches, per device, what the filesystem behind it is (fsInfo).
var fsByDev sync.Map

type fsInfo struct {
	network bool
}

// fileFS is what f's filesystem is: a network one (statfs magic, a FUSE
// mount's subtype from mountinfo) or not.
func fileFS(f *os.File) fsInfo {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return fsInfo{network: true}
	}
	if v, ok := fsByDev.Load(st.Dev); ok {
		return v.(fsInfo)
	}
	major := (st.Dev>>8)&0xfff | (st.Dev>>32)&^0xfff
	minor := st.Dev&0xff | (st.Dev>>12)&^0xff
	var info fsInfo
	var sfs syscall.Statfs_t
	switch err := syscall.Fstatfs(int(f.Fd()), &sfs); {
	case err != nil:
		info.network = true
	case sfs.Type == fuseMagic:
		mi, _ := os.ReadFile("/proc/self/mountinfo")
		info.network = networkFSType(mountFSType(mi, uint64(major), uint64(minor)))
	default:
		info.network = networkFS[int64(sfs.Type)]
	}
	fsByDev.Store(st.Dev, info)
	return info
}

const fuseMagic = 0x65735546

// networkFS are the statfs magics of network mounts (NFS, SMB, CIFS, SMB2,
// 9p, Ceph, AFS): read-ahead asked there is read across the network ahead of
// need, a round trip and bytes the read itself does not want. FUSE is told by
// its subtype (mountinfo.go).
var networkFS = map[int64]bool{0x6969: true, 0x517B: true, 0xFF534D42: true, 0xFE534D42: true, 0x01021997: true, 0x00C36400: true, 0x5346414F: true}

// onNetwork reports whether f sits on a network mount.
func onNetwork(f *os.File) bool { return fileFS(f).network }
