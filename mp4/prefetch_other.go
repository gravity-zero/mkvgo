//go:build !(linux && (amd64 || arm64))

package mp4

import "os"

// willNeed is a no-op where posix_fadvise is not called (see prefetch_linux.go).
func willNeed(*os.File, int64, int64) {}

// onNetwork is false where the filesystem is not looked up (see prefetch_linux.go).
func onNetwork(*os.File) bool { return false }

// readAheadBytes is the kernel's default read-ahead, 128 KiB, where the device is not looked up (see prefetch_linux.go).
func readAheadBytes(*os.File) int64 { return 128 << 10 }

// roundUpTo is v rounded up to a multiple of unit.
func roundUpTo(v, unit int64) int64 {
	if r := v % unit; r != 0 {
		return v + unit - r
	}
	return v
}
