package reader

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// simpleBlock builds a SimpleBlock element for a 1-byte-VINT track number with
// the given relative timecode, keyframe flag and a padding payload.
func simpleBlock(track uint64, relTC int16, keyframe bool, pad int) []byte {
	var b bytes.Buffer
	b.WriteByte(0x80 | byte(track)) // track number as a 1-byte VINT
	b.WriteByte(byte(relTC >> 8))
	b.WriteByte(byte(relTC))
	var flags byte
	if keyframe {
		flags |= 0x80
	}
	b.WriteByte(flags)
	b.Write(make([]byte, pad))
	return masterElem(mkv.IDSimpleBlock, b.Bytes())
}

// blockGroup builds a BlockGroup holding one Block; with a ReferenceBlock it is a
// referenced (non-key) frame, without one it is a keyframe.
func blockGroup(track uint64, relTC int16, hasRef bool, pad int) []byte {
	var blk bytes.Buffer
	blk.WriteByte(0x80 | byte(track))
	blk.WriteByte(byte(relTC >> 8))
	blk.WriteByte(byte(relTC))
	blk.WriteByte(0) // flags (a Block's keyframe bit is unused)
	blk.Write(make([]byte, pad))
	children := [][]byte{masterElem(mkv.IDBlock, blk.Bytes())}
	if hasRef {
		children = append(children, uintElem(mkv.IDReferenceBlock, 1, 1))
	}
	return masterElem(mkv.IDBlockGroup, children...)
}

// cluster builds a Cluster with the given Timestamp and blocks.
func cluster(ts uint64, blocks ...[]byte) []byte {
	children := [][]byte{uintElem(mkv.IDTimestamp, ts, 2)}
	children = append(children, blocks...)
	return masterElem(mkv.IDCluster, children...)
}

// TestSampledKeyframes covers the Cues-less keyframe index. Crucially, each
// emitted point must be a real video keyframe - NOT the Cluster start, which is
// not keyframe-aligned for every muxer.
func TestSampledKeyframes(t *testing.T) {
	const v, a = 1, 2 // video / audio track numbers (see tracksElem)
	children := [][]byte{
		infoElem(), tracksElem(),
		cluster(0, simpleBlock(v, 0, true, 120)), // keyframe at 0
		// First video block is NOT a keyframe: the real keyframe is the second,
		// at 1000+40. Emitting the Cluster Timestamp (1000) would be wrong.
		cluster(1000, simpleBlock(v, 0, false, 120), simpleBlock(v, 40, true, 120)),
		// Audio "keyframe" must be ignored; the video keyframe (2000+10) wins.
		cluster(2000, simpleBlock(a, 0, true, 120), simpleBlock(v, 10, true, 120)),
		// No video keyframe here: this Cluster must be skipped, not emitted as 3000.
		cluster(3000, simpleBlock(v, 0, false, 120)),
		cluster(4000, simpleBlock(v, 0, true, 120)), // keyframe at 4000
		// BlockGroups: the referenced one (ReferenceBlock present) is not a
		// keyframe; the unreferenced one at 5000+20 is.
		cluster(5000, blockGroup(v, 0, true, 120), blockGroup(v, 20, false, 120)),
	}
	file := segmentMKV(children...)

	// Default: no Cues → no Keyframes.
	c, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Keyframes) != 0 {
		t.Errorf("without the option, Keyframes = %v, want none", c.Keyframes)
	}

	// Opted in: each point is a real video keyframe time.
	c, err = ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv", WithSampledKeyframes(200))
	if err != nil {
		t.Fatalf("ReadMeta sampled: %v", err)
	}
	want := []int64{0, 1040, 2010, 4000, 5020}
	if !reflect.DeepEqual(c.Keyframes, want) {
		t.Errorf("sampled Keyframes = %v, want %v (real keyframes, Cluster 3000 skipped)", c.Keyframes, want)
	}

	// A file that already has Cues must not be sampled: the option is a no-op, so
	// the keyframe index is identical with and without it. (Cues precede Tracks so
	// the metadata pass reads them inline rather than skipping past them.)
	withCues := segmentMKV(infoElem(), cuesElem(3), tracksElem(), cluster(0, simpleBlock(v, 0, true, 50)))
	base, err := ReadMeta(context.Background(), bytes.NewReader(withCues), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta base: %v", err)
	}
	opt, err := ReadMeta(context.Background(), bytes.NewReader(withCues), "x.mkv", WithSampledKeyframes(200))
	if err != nil {
		t.Fatalf("ReadMeta opt: %v", err)
	}
	if len(base.Keyframes) == 0 {
		t.Fatal("the Cues should have produced a keyframe index")
	}
	if !reflect.DeepEqual(base.Keyframes, opt.Keyframes) {
		t.Errorf("with Cues present the option must not change Keyframes: base=%v opt=%v", base.Keyframes, opt.Keyframes)
	}
}
