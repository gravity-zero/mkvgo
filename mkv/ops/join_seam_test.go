package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildLinkedMKV writes a single-cluster MKV carrying an explicit segment
// identity, so a test can say whether two files are consecutive parts of one
// timeline or two files that merely follow each other.
func buildLinkedMKV(t *testing.T, dir, name string, tracks []mkv.Track, blocks []mkv.Block, durationMs int64, uid, prev, next []byte, chapters ...mkv.Chapter) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{
		TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test",
		SegmentUID: uid, PrevUID: prev, NextUID: next,
	}, Chapters: chapters}
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// firstTimecodeAfter returns the first timecode of the given track past afterMs:
// the instant the second source resumes on, i.e. the seam.
func firstTimecodeAfter(t *testing.T, path string, track uint64, afterMs int64) int64 {
	t.Helper()
	for _, tc := range collectTimecodes(t, path, 1_000_000, track) {
		if tc > afterMs {
			return tc
		}
	}
	t.Fatalf("%s: track %d has nothing past %d ms", filepath.Base(path), track, afterMs)
	return 0
}

// A cut runs down the file at a video keyframe, so it is exact on the picture
// and on nothing else: interleaving leaves the part before it holding sound
// from AFTER that keyframe, and the part after it opening on sound from before
// it. Joined back on the latest measured end, every seam gains that overlap and
// the film drifts a little further out at each one - 83 ms a seam on a real
// film, 909 ms over eleven of them.
//
// The overlap has two halves and the seam has to give both back. Sound stored
// AHEAD of the picture leaves the first part ending after its own last frame;
// sound stored BEHIND it makes the second part's zero stand for an instant
// before its first frame. Either way the picture is the one thing the cut is
// exact on, so the seam is where the picture continues.
func TestJoin_SeamOfLinkedPartsFollowsThePicture(t *testing.T) {
	for _, tc := range []struct {
		name string
		// audioAt maps a video timecode to the timecode of the audio block
		// stored right after it.
		audioAt func(int64) int64
	}{
		// The first part keeps the sound of 6000 while its picture stops at
		// 5500: its measured end overshoots the cut.
		{"sound stored ahead of the picture", func(tc int64) int64 { return tc + 500 }},
		// The second part opens on the sound of 5900, below the keyframe at
		// 6000 that starts it: its zero stands for an earlier instant than its
		// first frame.
		{"sound stored behind the picture", func(tc int64) int64 { return max0(tc - 100) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()

			var blocks []mkv.Block
			for v := int64(0); v < 10000; v += 500 {
				blocks = append(blocks,
					mkv.Block{TrackNumber: 1, Timecode: v, Keyframe: v%2000 == 0, Data: []byte("v")},
					mkv.Block{TrackNumber: 2, Timecode: tc.audioAt(v), Keyframe: true, Data: []byte("a")},
				)
			}
			src := buildMinimalMKV(t, dir, "src.mkv",
				[]mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 10500)
			srcVideo := collectTimecodes(t, src, 1_000_000, 1)
			srcAudio := collectTimecodes(t, src, 1_000_000, 2)

			parts, err := Split(ctx, mkv.SplitOptions{
				SourcePath: src,
				OutputDir:  filepath.Join(dir, "parts"),
				Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 5000}, {StartMs: 5000, EndMs: 0}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(parts) != 2 {
				t.Fatalf("expected 2 parts, got %d", len(parts))
			}

			joined := filepath.Join(dir, "joined.mkv")
			if err := Join(ctx, parts, joined); err != nil {
				t.Fatal(err)
			}
			// Not "close to": the parts hold every block of the source once, so
			// put back end to end they ARE the source's timeline.
			assertTimecodes(t, "joined video", collectTimecodes(t, joined, 1_000_000, 1), srcVideo)
			assertTimecodes(t, "joined audio", collectTimecodes(t, joined, 1_000_000, 2), srcAudio)
		})
	}
}

// The same seam must NOT move for files that are merely appended. Two unrelated
// files - episodes, recorder chunks - have no overlap to give back: aligning the
// second one's picture on the first one's would lay its sound over whatever tail
// the first one still had to play.
func TestJoin_UnlinkedSourcesKeepTheMeasuredSeam(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	// Video stops at 1500 (ends 2000), audio runs on to 2000 (ends 2500).
	blocks := func() []mkv.Block {
		var bs []mkv.Block
		for tc := int64(0); tc <= 1500; tc += 500 {
			bs = append(bs, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc == 0, Data: []byte("v")})
		}
		for tc := int64(0); tc <= 2000; tc += 500 {
			bs = append(bs, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
		}
		return bs
	}

	uidA := bytes.Repeat([]byte{0xA1}, 16)
	uidB := bytes.Repeat([]byte{0xB2}, 16)

	for _, tc := range []struct {
		name     string
		prevOfB  []byte
		wantSeam int64
	}{
		// B follows A in one timeline: the picture continues at A's video end.
		{"linked", uidA, 2000},
		// B is just the next file: nothing says its sound may cover A's tail.
		{"unlinked", nil, 2500},
		// A chain that points somewhere else - parts 1 and 3 of a split - is not
		// a chain.
		{"chained elsewhere", bytes.Repeat([]byte{0xCC}, 16), 2500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			a := buildLinkedMKV(t, sub, "a.mkv", tracks, blocks(), 2500, uidA, nil, uidB)
			b := buildLinkedMKV(t, sub, "b.mkv", tracks, blocks(), 2500, uidB, tc.prevOfB, nil)

			dst := filepath.Join(sub, "joined.mkv")
			if err := Join(ctx, []string{a, b}, dst); err != nil {
				t.Fatal(err)
			}
			if seam := firstTimecodeAfter(t, dst, 1, 1500); seam != tc.wantSeam {
				t.Errorf("second file's picture resumes at %d ms, want %d", seam, tc.wantSeam)
			}
		})
	}
}

// The picture rule closes the interleaving a cut ran through - a fraction of a
// second - and nothing else. A file whose sound genuinely outlives its picture
// by seconds is not a slice with an overlap to give back, whatever its Info
// claims, and burying those seconds under the next file is worse than the gap.
func TestJoin_LinkedSeamWillNotSwallowARealTail(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	var blocks []mkv.Block
	for tc := int64(0); tc <= 1500; tc += 500 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc == 0, Data: []byte("v")})
	}
	// The sound plays on for three more seconds after the last frame.
	for tc := int64(0); tc <= 5000; tc += 500 {
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
	}

	uidA := bytes.Repeat([]byte{0xA1}, 16)
	uidB := bytes.Repeat([]byte{0xB2}, 16)
	a := buildLinkedMKV(t, dir, "a.mkv", tracks, blocks, 5500, uidA, nil, uidB)
	b := buildLinkedMKV(t, dir, "b.mkv", tracks, blocks, 5500, uidB, uidA, nil)

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{a, b}, dst); err != nil {
		t.Fatal(err)
	}
	// 5500 is the measured end of the sound; 2000 would be the picture rule
	// applied past the reordering it is meant to close.
	if seam := firstTimecodeAfter(t, dst, 1, 1500); seam != 5500 {
		t.Errorf("second file's picture resumes at %d ms, want 5500 - the tail of the first file was covered", seam)
	}
}

// A linked chain says the files are slices of one timeline; it does not make
// their numbers trustworthy. A forged (or damaged) successor whose picture
// starts later than everything the previous file holds would pull the seam
// BEFORE that file's own start - and a block at the successor's zero would then
// land on a negative output timecode, silent corruption. The seam never backs
// past where the previous source began: such a pair falls back to the ordinary
// measured seam.
func TestJoin_LinkedSeamNeverBacksPastThePreviousStart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	// A short linked source: everything ends by 800 ms.
	blocksA := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
		{TrackNumber: 1, Timecode: 400, Keyframe: false, Data: []byte("v")},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a")},
		{TrackNumber: 2, Timecode: 400, Keyframe: true, Data: []byte("a")},
	}
	// Its "successor": sound at zero, but a picture starting at 950 ms - later
	// than everything A holds, so the picture rule would put A's whole timeline
	// behind this file's zero (seam = 800 - 950 < 0) while the one-cluster
	// tolerance (950 <= 1000) does not catch it.
	blocksB := []mkv.Block{
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a")},
		{TrackNumber: 2, Timecode: 400, Keyframe: true, Data: []byte("a")},
		{TrackNumber: 1, Timecode: 950, Keyframe: true, Data: []byte("v")},
		{TrackNumber: 1, Timecode: 1350, Keyframe: false, Data: []byte("v")},
	}

	uidA := bytes.Repeat([]byte{0xA1}, 16)
	uidB := bytes.Repeat([]byte{0xB2}, 16)
	a := buildLinkedMKV(t, dir, "a.mkv", tracks, blocksA, 800, uidA, nil, uidB)
	b := buildLinkedMKV(t, dir, "b.mkv", tracks, blocksB, 1750, uidB, uidA, nil)

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{a, b}, dst); err != nil {
		t.Fatal(err)
	}
	// B rides at the ordinary seam (A's measured end, 800): its audio resumes
	// there, nothing lands before A's own blocks, and no timecode is negative.
	for _, track := range []uint64{1, 2} {
		for _, tc := range collectTimecodes(t, dst, 1_000_000, track) {
			if tc < 0 {
				t.Fatalf("track %d has a block at %d ms", track, tc)
			}
		}
	}
	audio := collectTimecodes(t, dst, 1_000_000, 2)
	if len(audio) != 4 || audio[2] != 800 {
		t.Errorf("audio timecodes = %v, want the second file's zero at 800 (the measured seam)", audio)
	}
}

// Each part of a split is a segment of its own and says so: a shared SegmentUID
// (which is what copying the source's Info left behind) has twelve files all
// claiming to be the file they came from, and no way to tell a chain of parts
// from twelve copies of one.
func TestSplit_PartsCarryTheirOwnLinkedIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	var blocks []mkv.Block
	for tc := int64(0); tc < 9000; tc += 500 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%1500 == 0, Data: []byte("v")})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 9000)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges: []mkv.TimeRange{
			{StartMs: 0, EndMs: 3000}, {StartMs: 3000, EndMs: 6000}, {StartMs: 6000, EndMs: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var uids [][]byte
	var prevs [][]byte
	var nexts [][]byte
	for _, p := range parts {
		c, err := reader.Open(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Info.SegmentUID) != 16 {
			t.Fatalf("%s: SegmentUID is %d bytes, want 16", filepath.Base(p), len(c.Info.SegmentUID))
		}
		uids = append(uids, c.Info.SegmentUID)
		prevs = append(prevs, c.Info.PrevUID)
		nexts = append(nexts, c.Info.NextUID)
	}
	for i := range uids {
		for j := i + 1; j < len(uids); j++ {
			if bytes.Equal(uids[i], uids[j]) {
				t.Errorf("parts %d and %d share one SegmentUID", i+1, j+1)
			}
		}
	}
	if len(prevs[0]) != 0 || len(nexts[len(nexts)-1]) != 0 {
		t.Errorf("the chain runs off its ends: prev of part 1 = %x, next of the last = %x", prevs[0], nexts[len(nexts)-1])
	}
	for i := 1; i < len(uids); i++ {
		if !bytes.Equal(prevs[i], uids[i-1]) {
			t.Errorf("part %d points back at %x, want part %d (%x)", i+1, prevs[i], i, uids[i-1])
		}
		if !bytes.Equal(nexts[i-1], uids[i]) {
			t.Errorf("part %d points forward at %x, want part %d (%x)", i, nexts[i-1], i+1, uids[i])
		}
	}

	// Splitting the same source twice writes the same identities: an output that
	// changes every run cannot be compared byte for byte, which is how the ops
	// in this package are checked.
	again, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "again"),
		Ranges: []mkv.TimeRange{
			{StartMs: 0, EndMs: 3000}, {StartMs: 3000, EndMs: 6000}, {StartMs: 6000, EndMs: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range again {
		c, err := reader.Open(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c.Info.SegmentUID, uids[i]) {
			t.Errorf("part %d got a different SegmentUID on a second run", i+1)
		}
	}
}

// Splitting a source that is ITSELF a slice of a larger timeline: the chain's
// ends keep the source's own links. The first part still succeeds the source's
// predecessor and the last still precedes its successor - those statements
// stayed true, and dropping them cut the part chain out of the larger one.
func TestSplit_KeepsTheSourcesOwnLinksAtTheChainEnds(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPrev := bytes.Repeat([]byte{0xD4}, 16)
	srcNext := bytes.Repeat([]byte{0xE5}, 16)
	var blocks []mkv.Block
	for tc := int64(0); tc < 6000; tc += 500 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")})
	}
	src := buildLinkedMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 6000,
		bytes.Repeat([]byte{0xF6}, 16), srcPrev, srcNext)

	parts, err := Split(ctx, mkv.SplitOptions{
		SourcePath: src,
		OutputDir:  filepath.Join(dir, "parts"),
		Ranges:     []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 4000}, {StartMs: 4000, EndMs: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := reader.Open(ctx, parts[0])
	if err != nil {
		t.Fatal(err)
	}
	last, err := reader.Open(ctx, parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Info.PrevUID, srcPrev) {
		t.Errorf("part 1 PrevUID = %x, want the source's predecessor %x", first.Info.PrevUID, srcPrev)
	}
	if !bytes.Equal(last.Info.NextUID, srcNext) {
		t.Errorf("last part NextUID = %x, want the source's successor %x", last.Info.NextUID, srcNext)
	}
}

// A joined file is a new segment: it is not the first part wearing its name,
// and it must not send a player looking for the second part - which is inside
// it now.
func TestJoin_OutputDropsThePartsIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1)}
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}}

	uidA := bytes.Repeat([]byte{0xA1}, 16)
	uidB := bytes.Repeat([]byte{0xB2}, 16)
	a := buildLinkedMKV(t, dir, "a.mkv", tracks, blocks, 500, uidA, nil, uidB)
	b := buildLinkedMKV(t, dir, "b.mkv", tracks, blocks, 500, uidB, uidA, nil)

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{a, b}, dst); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Info.SegmentUID) != 0 || len(c.Info.PrevUID) != 0 || len(c.Info.NextUID) != 0 {
		t.Errorf("joined file carries the first part's identity: uid %x prev %x next %x",
			c.Info.SegmentUID, c.Info.PrevUID, c.Info.NextUID)
	}
}
