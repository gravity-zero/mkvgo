//go:build linux && (amd64 || arm64)

package mp4

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// willNeedChunk is how much one posix_fadvise call asks for when the
// device's read-ahead size is unknown: the kernel reads at most that size per
// call and ignores the rest of a larger range, so a range is asked for piece
// by piece. 128 KiB is the kernel's default read-ahead.
const willNeedChunk = 128 << 10

// willNeed asks the kernel to start reading [off, off+n) of f now, in the
// background (posix_fadvise WILLNEED): reads issued one after the other then
// find their pages already coming in, instead of each waiting for the disk in
// turn. Advisory: any failure is ignored.
func willNeed(f *os.File, off, n int64) {
	const fadvWillNeed = 3
	chunk := readAheadBytes(f)
	for end := off + n; off < end; off += chunk {
		_, _, _ = syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(min(chunk, end-off)), fadvWillNeed, 0, 0)
	}
}

// fsByDev caches, per device, what the filesystem behind it is (fsInfo).
var fsByDev sync.Map

type fsInfo struct {
	network   bool
	readAhead int64
}

// fileFS is what f's filesystem is: a network one (statfs magic, a FUSE
// mount's subtype from mountinfo), and its device's read-ahead size.
func fileFS(f *os.File) fsInfo {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return fsInfo{network: true, readAhead: willNeedChunk}
	}
	if v, ok := fsByDev.Load(st.Dev); ok {
		return v.(fsInfo)
	}
	major := (st.Dev>>8)&0xfff | (st.Dev>>32)&^0xfff
	minor := st.Dev&0xff | (st.Dev>>12)&^0xff
	info := fsInfo{readAhead: deviceReadAhead(major, minor)}
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

// readAheadBytes is the read-ahead size of f's device, willNeedChunk when unknown.
func readAheadBytes(f *os.File) int64 { return fileFS(f).readAhead }

// deviceReadAhead reads /sys/dev/block/<major>:<minor>/queue/read_ahead_kb (a
// partition's is its disk's): the most one read-ahead call reads.
func deviceReadAhead(major, minor uint64) int64 {
	base := "/sys/dev/block/" + itoa(major) + ":" + itoa(minor)
	for _, p := range []string{base + "/queue/read_ahead_kb", base + "/../queue/read_ahead_kb"} {
		if b, err := os.ReadFile(p); err == nil {
			if kb := atoiTrim(b); kb > 0 {
				return int64(kb) << 10
			}
		}
	}
	return willNeedChunk
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

// atoiTrim parses a small decimal file's content, 0 when it is not one.
func atoiTrim(b []byte) int {
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
