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

// The read-ahead asked per call is the file's device's read_ahead_kb (its
// disk's, for a partition), 128 KiB when the device has none to read.
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
	want := int64(willNeedChunk)
	base := "/sys/dev/block/" + strconv.FormatUint(uint64(major), 10) + ":" + strconv.FormatUint(uint64(minor), 10)
	for _, p := range []string{base + "/queue/read_ahead_kb", base + "/../queue/read_ahead_kb"} {
		if b, err := os.ReadFile(p); err == nil {
			if kb, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && kb > 0 {
				want = int64(kb) << 10
				break
			}
		}
	}
	if got := readAheadBytes(f); got != want {
		t.Errorf("read-ahead %d, want %d", got, want)
	}
}
