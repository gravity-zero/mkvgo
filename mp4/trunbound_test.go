package mp4

import (
	"encoding/binary"
	"testing"
)

// TestTrunCountFitsTheFragmentData: a trun listing no per-sample field is its count and nothing else: twenty bytes
// declared 16M samples, 800 MB of sample records, in a file of a few hundred bytes. Its samples must fit the data the
// fragment holds, each taken as at least one byte.
func TestTrunCountFitsTheFragmentData(t *testing.T) {
	trun := func(flags, count uint32) memBox {
		p := binary.BigEndian.AppendUint32(nil, flags)
		return memBox{typ: "trun", payload: binary.BigEndian.AppendUint32(p, count)}
	}
	if err := trunsFitData([]memBox{trun(0, 1<<24)}, 0, 4096); err == nil {
		t.Error("16M empty samples in a 4 KiB fragment were accepted")
	}
	if err := trunsFitData([]memBox{trun(0, 100)}, 40, 4096); err != nil {
		t.Errorf("100 samples of 40 bytes in 4 KiB: %v", err)
	}
	if err := trunsFitData([]memBox{trun(trunSampleSize, 1<<24)}, 0, 4096); err != nil {
		t.Errorf("a trun with per-sample sizes is bounded by its own bytes, not here: %v", err)
	}
}
