package reader

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// lacedCluster holds 10 SimpleBlocks of each lacing - Xiph, EBML, fixed - of
// 3 frames of a few KiB (fixed: equal sizes).
func lacedCluster() []byte {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	for b := 0; b < 30; b++ {
		frames := [][]byte{bytes.Repeat([]byte{byte(b)}, 3000+b), bytes.Repeat([]byte{0xBB}, 2500), bytes.Repeat([]byte{0xCC}, 1000+7*b)}
		payload := []byte{0x81, 0x00, byte(b)}
		switch b % 3 {
		case 0: // Xiph
			payload = append(payload, 0x02, 0x02)
			for _, f := range frames[:2] {
				n := len(f)
				for ; n >= 255; n -= 255 {
					payload = append(payload, 0xFF)
				}
				payload = append(payload, byte(n))
			}
		case 1: // EBML: first size, then the signed difference
			payload = append(payload, 0x06, 0x02)
			payload = append(payload, 0x40|byte(len(frames[0])>>8), byte(len(frames[0])))
			diff := len(frames[1]) - len(frames[0]) + 8191 // 2-byte signed VINT bias
			payload = append(payload, 0x40|byte(diff>>8), byte(diff))
		case 2: // fixed
			payload = append(payload, 0x04, 0x02)
			frames = [][]byte{frames[1], frames[1], frames[1]}
		}
		for _, f := range frames {
			payload = append(payload, f...)
		}
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(payload)))
		cluster.Write(payload)
	}
	return cluster.Bytes()
}

// A header-only walk times and places every laced frame exactly as a full
// read does - size, data offset, timecode - from the lace header alone,
// without reading or allocating the frames.
func TestHeaderOnlyLacedBlocksSkipPayload(t *testing.T) {
	cluster := lacedCluster()
	walk := func(headerOnly bool) ([]mkv.Block, uint64) {
		br, err := NewBlockReader(buildBlockReaderInput(cluster), 1000000)
		if err != nil {
			t.Fatal(err)
		}
		br.SetHeaderOnly(headerOnly)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		before := ms.TotalAlloc
		var out []mkv.Block
		for {
			b, err := br.Next()
			if err != nil {
				break
			}
			out = append(out, b)
		}
		runtime.ReadMemStats(&ms)
		return out, ms.TotalAlloc - before
	}
	full, _ := walk(false)
	head, alloc := walk(true)
	if len(full) != 90 || len(head) != len(full) {
		t.Fatalf("%d frames read, %d header-only; want 90", len(full), len(head))
	}
	for i := range full {
		f, h := full[i], head[i]
		if h.Data != nil || h.Size != f.Size || h.DataOffset != f.DataOffset || h.Timecode != f.Timecode || int64(len(f.Data)) != f.Size {
			t.Fatalf("frame %d: header-only size %d off %d tc %d data %d, full size %d off %d tc %d", i, h.Size, h.DataOffset, h.Timecode, len(h.Data), f.Size, f.DataOffset, f.Timecode)
		}
	}
	if alloc > 64<<10+3*7000 { // the reader's read-ahead window, not the 30 blocks' 200 KiB of frames
		t.Errorf("header-only walk allocated %d bytes", alloc)
	}
}
