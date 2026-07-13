package mp4

// composition_shift_test.go - a fragmented file cannot carry a negative
// composition offset: its decode clock (tfdt) is unsigned, so a reader that meets
// one answers it by pushing the WHOLE presentation forward by the deepest of them
// - the picture arriving one to two frames after the audio, on any source with
// B-frames. The offsets are therefore shifted non-negative before they are
// written, and the init's edit list takes that shift straight back out.

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// reorderedSource builds an MKV whose video blocks are stored in DECODE order
// with reordered presentation times (I P B B per GOP - one level of reordering,
// as any B-frame encoder produces), at 24fps.
func reorderedSource(t *testing.T, gops int) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	const fr = 42 // ~24fps
	var blocks []genBlock
	for g := 0; g < gops; g++ {
		base := int64(g) * 4 * fr
		blocks = append(blocks,
			genBlock{track: 1, pts: base, key: true, data: iframeTestFrame(g*4, true)},
			genBlock{track: 1, pts: base + 3*fr, key: false, data: iframeTestFrame(g*4+1, false)},
			genBlock{track: 1, pts: base + 1*fr, key: false, data: iframeTestFrame(g*4+2, false)},
			genBlock{track: 1, pts: base + 2*fr, key: false, data: iframeTestFrame(g*4+3, false)},
		)
	}
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
	}, blocks)
}

// segmentCTS returns every composition offset in a media segment's trun, and the
// presentation time of its first sample (dts+cts, the tfdt base included).
func segmentCTS(t *testing.T, seg []byte) (offsets []int32, firstPts int64) {
	t.Helper()
	var base uint64
	var walk func(b []byte)
	walk = func(b []byte) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b[0:4]))
			typ := string(b[4:8])
			if size < 8 || size > len(b) {
				return
			}
			body := b[8:size]
			switch typ {
			case "moof", "traf":
				walk(body)
			case "tfdt":
				if body[0] == 1 {
					base = binary.BigEndian.Uint64(body[4:12])
				} else {
					base = uint64(binary.BigEndian.Uint32(body[4:8]))
				}
			case "trun":
				flags := binary.BigEndian.Uint32(body[0:4]) & 0xFFFFFF
				if flags&0x800 == 0 {
					return // no composition offsets at all
				}
				n := int(binary.BigEndian.Uint32(body[4:8]))
				p := 8
				if flags&0x1 != 0 {
					p += 4
				}
				if flags&0x4 != 0 {
					p += 4
				}
				for i := 0; i < n; i++ {
					if flags&0x100 != 0 {
						p += 4
					}
					if flags&0x200 != 0 {
						p += 4
					}
					if flags&0x400 != 0 {
						p += 4
					}
					cts := int32(binary.BigEndian.Uint32(body[p:]))
					p += 4
					offsets = append(offsets, cts)
					if i == 0 {
						firstPts = int64(base) + int64(cts)
					}
				}
			}
			b = b[size:]
		}
	}
	walk(seg)
	return offsets, firstPts
}

// initEditList returns the media_time of the first edit-list entry in an init
// segment, and whether one is there at all.
func initEditList(t *testing.T, init []byte) (int64, bool) {
	t.Helper()
	var mediaTime int64
	var found bool
	var walk func(b []byte)
	walk = func(b []byte) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b[0:4]))
			typ := string(b[4:8])
			if size < 8 || size > len(b) {
				return
			}
			body := b[8:size]
			switch typ {
			case "moov", "trak", "edts":
				walk(body)
			case "elst":
				if found {
					return
				}
				if binary.BigEndian.Uint32(body[4:8]) == 0 {
					return
				}
				if body[0] == 1 {
					mediaTime = int64(binary.BigEndian.Uint64(body[16:24]))
				} else {
					mediaTime = int64(int32(binary.BigEndian.Uint32(body[12:16])))
				}
				found = true
			}
			b = b[size:]
		}
	}
	walk(init)
	return mediaTime, found
}

// TestFragmentedReorderPresentsFromZero is the regression: on a reordered source,
// no composition offset may be negative (a reader would re-time the whole
// presentation), the init must carry the edit list that cancels the shift, and the
// first frame must therefore be presented at the source's own time - zero - not a
// frame or two late.
func TestFragmentedReorderPresentsFromZero(t *testing.T) {
	src := reorderedSource(t, 8)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	seg, err := os.ReadFile(filepath.Join(dir, "seg00001.m4s"))
	if err != nil {
		t.Fatal(err)
	}

	offsets, firstPts := segmentCTS(t, seg)
	if len(offsets) == 0 {
		t.Fatal("no composition offsets in the segment: the fixture is not reordered")
	}
	for i, cts := range offsets {
		if cts < 0 {
			t.Fatalf("sample %d carries a negative composition offset (%d): a fragmented reader cannot honour it and shifts the whole presentation", i, cts)
		}
	}
	if firstPts != 42 {
		t.Errorf("first sample presents at %dms in media time, want 42 (the shift the edit list takes back out)", firstPts)
	}
	mediaTime, ok := initEditList(t, init)
	if !ok {
		t.Fatal("the init carries no edit list: nothing takes the composition shift back out, and the picture plays late")
	}
	if mediaTime != 42 {
		t.Errorf("edit list media_time = %d, want 42 (exactly the shift applied to the offsets)", mediaTime)
	}
}

// TestPlanAndFullPassAgreeOnTheShift pins the parity the shift must not break: the
// on-demand plan measures it from the opening frames alone (header-only), the full
// pass from its samples - and both must produce the same init, byte for byte, and
// the same segment timing.
func TestPlanAndFullPassAgreeOnTheShift(t *testing.T) {
	src := reorderedSource(t, 8)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	wantInit, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	wantSeg, err := os.ReadFile(filepath.Join(dir, "seg00001.m4s"))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.InitSegment(); !bytes.Equal(got, wantInit) {
		t.Errorf("plan init (%d bytes) != full-pass init (%d bytes): the two measure the composition shift differently", len(got), len(wantInit))
	}
	gotSeg, err := plan.Segment(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSeg, wantSeg) {
		t.Errorf("plan segment 0 (%d bytes) != full-pass segment (%d bytes)", len(gotSeg), len(wantSeg))
	}
}

// TestGrowingPlanCarriesTheShift is the regression for a trap the fix itself set:
// the growing (still-downloading) plan builds its own HLSPlan by hand and delegates
// segment building to the very same window timing. Give it no shift and that timing
// clamps the negative offsets to zero - every B-frame presented at its DECODE time,
// the display order destroyed. It must measure the shift like the others (from the
// blocks its scan already walks) and settle it before the init is ever published:
// the init is a byte a player fetches once and caches.
func TestGrowingPlanCarriesTheShift(t *testing.T) {
	src := reorderedSource(t, 8)
	ctx := context.Background()

	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	want, err := plan.Segment(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	g, err := PlanGrowingHLS(ctx, src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	g.Complete()
	got, err := g.Segment(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	offsets, firstPts := segmentCTS(t, got)
	zeros := 0
	for _, cts := range offsets {
		if cts < 0 {
			t.Fatalf("growing plan wrote a negative composition offset (%d)", cts)
		}
		if cts == 0 {
			zeros++
		}
	}
	if zeros == len(offsets) {
		t.Fatal("every composition offset is zero: the reordering was clamped away, and the B-frames now present at their decode time")
	}
	if firstPts != 42 {
		t.Errorf("first sample presents at %dms, want 42 (the shift the edit list takes back out)", firstPts)
	}
	if _, ok := initEditList(t, g.InitSegment()); !ok {
		t.Error("the growing plan's init carries no edit list: the picture would play late")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("growing segment (%d bytes) != plan segment (%d bytes): the two time the same window differently", len(got), len(want))
	}
}

// TestNoReorderNoEditList guards the other side: a source with no B-frames gets no
// shift and no edit list - its bytes are exactly what they were.
func TestNoReorderNoEditList(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 24; i++ {
		blocks = append(blocks, genBlock{
			track: 1, pts: int64(i) * 42, key: i%4 == 0, data: iframeTestFrame(i, i%4 == 0),
		})
	}
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
	}, blocks)

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := initEditList(t, init); ok {
		t.Error("an unreordered track gained an edit list: the shift must be zero and change nothing")
	}
}
