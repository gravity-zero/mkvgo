package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// A frame that is no keyframe carries the timestamp of the keyframe after it, and two frames that present before it
// follow it in decode order, in the cluster before the keyframe's - how a muxer that opens a cluster on each keyframe
// stores them. All three belong to the segment they decode in, and the next one opens on its keyframe. Cut on time
// alone, the full pass opened the next segment on the tied frame, and a window served cold lost the two after it.
func TestSegmentOpensOnItsKeyframeWhenAFrameSharesItsTimestamp(t *testing.T) {
	w, h := uint32(320), uint32(240)
	payload := func(i int) []byte { return []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x7E, byte(i), 0x7E, byte(i), 0x7E} }
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	m := writer.NewMKVWriter(f)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 74*40); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 3; g++ { // one cluster a GOP of 25 frames, 40 ms each
		var blks []mkv.Block
		for i := g * 25; i < g*25+25; i++ {
			pts := int64(i) * 40
			switch i { // decode order: 47 stamped like keyframe 50, then 48 and 49 presenting before it
			case 47:
				pts = 2000
			case 48:
				pts = 1880
			case 49:
				pts = 1920
			}
			blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: pts, Keyframe: i%25 == 0, Data: payload(i)})
		}
		if err := m.WriteClusterWithCues(int64(g)*1000, scale, blks); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, path, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		plan, err := PlanHLS(ctx, path, Options{SegmentMs: 2000}) // a fresh plan: the window is served cold
		if err != nil {
			t.Fatal(err)
		}
		if plan.NumSegments() != 2 {
			t.Fatalf("plan has %d segments, want 2", plan.NumSegments())
		}
		full, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
		if err != nil {
			t.Fatal(err)
		}
		od, err := plan.Segment(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range []int{47, 48, 49, 50} {
			want := (i < 50) == (n == 0)
			if got := bytes.Contains(full, payload(i)[4:]); got != want {
				t.Errorf("full pass: segment %d holds frame %d = %v, want %v", n, i, got, want)
			}
			if got := bytes.Contains(od, payload(i)[4:]); got != want {
				t.Errorf("on demand: segment %d holds frame %d = %v, want %v", n, i, got, want)
			}
		}
		if !bytes.Equal(od, full) {
			t.Errorf("segment %d: on demand differs from the full pass (%d vs %d bytes)", n, len(od), len(full))
		}
	}
}

// The last frame of a GOP is stamped two seconds back, as a muxer's stray timestamp, inside its own segment or in the
// span of the segment before: both paths give every sample the same times, and no composition offset goes below zero.
func TestStrayTimestampSameOnBothPaths(t *testing.T) {
	for _, stamp := range []int64{2010, 1970} {
		t.Run(fmt.Sprintf("stamped %d", stamp), func(t *testing.T) { strayTimestampParity(t, stamp) })
	}
}

func strayTimestampParity(t *testing.T, stamp int64) {
	w, h := uint32(320), uint32(240)
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	m := writer.NewMKVWriter(f)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 149*40); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 6; g++ { // one cluster a GOP of 25 frames, 40 ms each
		var blks []mkv.Block
		for i := g * 25; i < g*25+25; i++ {
			pts := int64(i) * 40
			if i == 99 {
				pts = stamp // the frame before the keyframe at 4 s
			}
			blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: pts, Keyframe: i%25 == 0, Data: []byte{0, 0, 0, 1, 0x41, byte(i)}})
		}
		if err := m.WriteClusterWithCues(int64(g)*1000, scale, blks); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, path, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		plan, err := PlanHLS(ctx, path, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		full, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
		if err != nil {
			t.Fatal(err)
		}
		od, err := plan.Segment(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(od, full) {
			t.Errorf("segment %d: on demand differs from the full pass (%d vs %d bytes)", n, len(od), len(full))
		}
		if low := lowestTrunOffset(full); low < 0 {
			t.Errorf("segment %d: a composition offset of %d ms for a stray timestamp", n, low)
		}
	}
}

// lowestTrunOffset is the lowest composition offset of a segment's first trun (0 when it carries none).
func lowestTrunOffset(seg []byte) int64 {
	i := bytes.Index(seg, []byte("trun"))
	if i < 0 || i+16 > len(seg) {
		return 0
	}
	flags := binary.BigEndian.Uint32(seg[i+4:i+8]) & 0xFFFFFF
	n := int(binary.BigEndian.Uint32(seg[i+8 : i+12]))
	p := i + 12
	if flags&1 != 0 {
		p += 4
	}
	if flags&4 != 0 {
		p += 4
	}
	var low int64
	for k := 0; k < n; k++ {
		for _, f := range []uint32{0x100, 0x200, 0x400} {
			if flags&f != 0 {
				p += 4
			}
		}
		if flags&0x800 != 0 {
			if p+4 > len(seg) {
				return low
			}
			low = min(low, int64(int32(binary.BigEndian.Uint32(seg[p:p+4]))))
			p += 4
		}
	}
	return low
}

// Options.SignedCompositionOffsets: a B-pyramid's offsets go below zero around a first frame that presents when it
// decodes, the init carries no edit list for the video, and both paths write the same bytes. Without the option the
// same source gets its edit list and no negative offset.
func TestSignedCompositionOffsets(t *testing.T) {
	w, h := uint32(320), uint32(240)
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	m := writer.NewMKVWriter(f)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 149*40); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 6; g++ { // one cluster a GOP: a keyframe, then groups decoded P B b b
		base := g * 25
		order := []int{0}
		for k := 1; k < 25; k += 4 {
			order = append(order, k+3, k+1, k, k+2)
		}
		var blks []mkv.Block
		for _, i := range order {
			blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: int64(base+i) * 40, Keyframe: i == 0, Data: []byte{0, 0, 0, 1, 0x41, byte(base + i)}})
		}
		if err := m.WriteClusterWithCues(int64(g)*1000, scale, blks); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, signed := range []bool{false, true} {
		o := Options{SegmentMs: 2000, SignedCompositionOffsets: signed}
		dir := t.TempDir()
		if err := RemuxToHLS(ctx, path, dir, o); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanHLS(ctx, path, o)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(plan.InitSegment(), init) {
			t.Errorf("signed=%v: the plan's init differs from the full pass", signed)
		}
		if got := bytes.Contains(init, []byte("elst")); got == signed {
			t.Errorf("signed=%v: init carries an edit list = %v", signed, got)
		}
		lowest := int64(0)
		for n := 0; n < plan.NumSegments(); n++ {
			full, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
			if err != nil {
				t.Fatal(err)
			}
			od, err := plan.Segment(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(od, full) {
				t.Errorf("signed=%v segment %d: on demand differs from the full pass", signed, n)
			}
			lowest = min(lowest, lowestTrunOffset(full))
		}
		if want := map[bool]int64{false: 0, true: -80}[signed]; lowest != want {
			t.Errorf("signed=%v: lowest composition offset %d, want %d", signed, lowest, want)
		}
	}
}
