package ops

import (
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzPOCRenumber: a keyframe of any bytes behind any configuration record is renumbered or refused, never a panic.
func FuzzPOCRenumber(f *testing.F) {
	f.Add(true, testHVCC(), lp(testHEVCSlice(19, 0, nil)))
	f.Add(true, testHVCC(), lp(testHEVCSlice(21, 1, nil)))
	f.Add(false, avcCRecord(), lp(testAVCSlice(0x65, 7, 0, 0)))
	f.Fuzz(func(t *testing.T, hevc bool, record, sample []byte) {
		costguard.Fuzz(t, len(record)+len(sample), func() {
			if hevc {
				_, _ = newHEVCPOCRenumber(record).renumber(nil, sample, 4)
				return
			}
			_, _ = newAVCPOCRenumber(record).renumber(nil, sample, 4)
		})
	})
}
