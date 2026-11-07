//go:build linux && (amd64 || arm64)

package mp4

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The read-ahead asked per call is the most the kernel reads for one: the larger
// of the file's device's read_ahead_kb and max_sectors_kb (its disk's, for a
// partition), 128 KiB when the device has neither. Measured on 6.6: with
// read_ahead_kb 128 and max_sectors_kb 1280, calls of 1 MiB read 1 MiB each,
// calls of 2 MiB 1,280 KiB each, the rest of the range left unread.
func TestReadAheadFollowsTheDevice(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	major := (st.Dev>>8)&0xfff | (st.Dev>>32)&^0xfff
	minor := st.Dev&0xff | (st.Dev>>12)&^0xff
	var want int64
	base := "/sys/dev/block/" + strconv.FormatUint(uint64(major), 10) + ":" + strconv.FormatUint(uint64(minor), 10)
	for _, name := range []string{"read_ahead_kb", "max_sectors_kb"} {
		for _, p := range []string{base + "/queue/" + name, base + "/../queue/" + name} {
			if b, err := os.ReadFile(p); err == nil {
				if kb, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && kb > 0 {
					want = max(want, int64(kb)<<10)
					break
				}
			}
		}
	}
	if want == 0 {
		want = willNeedChunk
	}
	if got := readAheadBytes(f); got != want {
		t.Errorf("read-ahead %d, want %d", got, want)
	}
}

// A read-ahead request is asked for in pieces cut on the file's chunk boundaries, never widened: a window read
// ahead to a boundary and the next window's start then share it, one read of the disk (measured on a fragmented
// MP4: one read too many a window, 1,561 reads against 1,104 for the file read in order); widened at both ends, a
// window asked for alone read up to a chunk too many each side (random access 14 % slower, measured).
func TestWillNeedCutsOnTheFileChunks(t *testing.T) {
	const chunk = 1280 << 10
	for _, c := range []struct{ off, end, cut int64 }{
		{0, 100, 100},
		{0, 2 * chunk, chunk},
		{chunk + 7, 3 * chunk, 2 * chunk},
		{chunk - 1, chunk + 1, chunk},
		{3*chunk + 100, 3*chunk + 200, 3*chunk + 200},
	} {
		if cut := willNeedCut(c.off, c.end, chunk); cut != c.cut {
			t.Errorf("[%d, %d): first piece ends at %d, want %d", c.off, c.end, cut, c.cut)
		}
	}
	if roundUpTo(chunk+1, chunk) != 2*chunk || roundUpTo(2*chunk, chunk) != 2*chunk {
		t.Error("roundUpTo")
	}
}
