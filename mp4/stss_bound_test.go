package mp4

import (
	"encoding/binary"
	"testing"
)

// TestStssBoundedBySampleCount: the sync table was sized and filled from the count it declares, whatever the
// track's sample count - a one-sample track could carry a 64M-entry map - and a table cut short read as "every
// sample is a keyframe". Only numbers within the track count, and a short table holds what it holds.
func TestStssBoundedBySampleCount(t *testing.T) {
	stss := func(count uint32, entries ...uint32) []memBox {
		p := make([]byte, 8, 8+4*len(entries))
		binary.BigEndian.PutUint32(p[4:], count)
		for _, e := range entries {
			p = binary.BigEndian.AppendUint32(p, e)
		}
		return []memBox{{typ: "stss", payload: p}}
	}
	if set := parseStssN(stss(3, 1, 30, 5), 10); len(set) != 2 || !set[1] || !set[5] {
		t.Errorf("entries 1, 30, 5 for 10 samples: %v, want {1, 5}", set)
	}
	if set := parseStssN(stss(5, 1, 4), 10); set == nil || len(set) != 2 {
		t.Errorf("a table declaring 5 entries holding 2: %v, want the 2 it holds, not every sample", set)
	}
}
