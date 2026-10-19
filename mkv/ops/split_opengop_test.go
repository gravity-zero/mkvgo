package ops

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitOpenGOPPartsOpenOnTheirKeyframe pins a cut on an open-GOP keyframe: the next part opens on it at 0 without
// the leading pictures stored after it (they decode only with the GOP before), the part before takes the sound stored
// among them and holds its last picture to the cut, so no sound is lost and a join keeps the timeline.
func TestSplitOpenGOPPartsOpenOnTheirKeyframe(t *testing.T) {
	c := matrix.Case{OpenGOP: true, Audio: true, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	cut := c.ExpectedCuts()[0]
	p1, p2 := trackBlocks(collectAllBlocks(t, parts[0])), trackBlocks(collectAllBlocks(t, parts[1]))
	if v := p2[matrix.VideoTrack]; v[0].Timecode != 0 || !v[0].Keyframe || v[1].Timecode <= 0 {
		t.Errorf("part 2 opens on %s, want its keyframe at 0 and no picture before it", blockSeq(v[:2]))
	}
	if n := len(p1[matrix.AudioTrack]) + len(p2[matrix.AudioTrack]); n != len(c.Blocks()[matrix.AudioTrack]) {
		t.Errorf("the parts hold %d audio frames, the source %d", n, len(c.Blocks()[matrix.AudioTrack]))
	}
	last := p1[matrix.VideoTrack][0]
	for _, b := range p1[matrix.VideoTrack] {
		if b.Timecode > last.Timecode {
			last = b
		}
	}
	if last.Timecode+last.Duration != cut {
		t.Errorf("part 1's last picture %d+%d ms ends short of the cut at %d", last.Timecode, last.Duration, cut)
	}
}

// TestSplitOpensOnTheCueAtItsZero pins that a cue on screen across a cut opens the next part at its zero, even when
// sound muxed ahead of the keyframe puts that zero a few ms before the first picture.
func TestSplitOpensOnTheCueAtItsZero(t *testing.T) {
	c := matrix.Case{OpenGOP: true, Audio: true, NTSC: true, Subtitle: matrix.CueOverCut, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	mustNil(t, err)
	cue := c.Blocks()[matrix.SubtitleTrack][0]
	b := trackBlocks(collectAllBlocks(t, parts[1]))
	video, subs := b[matrix.VideoTrack], b[matrix.SubtitleTrack]
	if len(subs) != 1 || subs[0].Timecode != 0 || subs[0].Timecode+subs[0].Duration != video[0].Timecode+cue.Timecode+cue.Duration-c.ExpectedCuts()[0] {
		t.Errorf("part 2 subtitles %s with its picture at %d ms, want the cue from 0 to where it ends", blockSeq(subs), video[0].Timecode)
	}
}

func trackBlocks(blocks []mkv.Block) map[uint64][]mkv.Block {
	out := map[uint64][]mkv.Block{}
	for _, b := range blocks {
		out[b.TrackNumber] = append(out[b.TrackNumber], b)
	}
	return out
}

func blockSeq(blocks []mkv.Block) string {
	s := ""
	for _, b := range blocks {
		s += fmt.Sprintf("%d/%v/%d/%x ", b.Timecode, b.Keyframe, b.Duration, b.Data)
	}
	return s
}

// TestSplitHoldsTheLastPictureWhenTheSoundBehindTheCutRollsTheCluster pins the hold to the cut when the sound read
// past the cut keyframe starts a new output cluster: the last picture, written in the cluster before, already reaches
// the cut, so a join places the next part at it, not the length of the leading pictures early.
func TestSplitHoldsTheLastPictureWhenTheSoundBehindTheCutRollsTheCluster(t *testing.T) {
	type stored struct {
		dts int64
		b   mkv.Block
	}
	var all []stored
	video := make([]mkv.Block, 100) // 25 fps, keyframes at 0 and 2040, the second ahead of the 2 pictures shown before it
	for j := range video {
		video[j] = mkv.Block{TrackNumber: 1, Timecode: int64(j) * 40, Keyframe: j%51 == 0, Data: []byte{0, 0, 0, 1, 0x65, byte(j)}}
	}
	video[49], video[50], video[51] = video[51], video[49], video[50]
	for j, b := range video {
		all = append(all, stored{int64(j) * 40, b})
	}
	for k := int64(0); k < 200; k++ {
		all = append(all, stored{k*20 + 1, mkv.Block{TrackNumber: 2, Timecode: k * 20, Keyframe: true, Data: []byte{0xAA, byte(k)}}})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].dts < all[b].dts })
	blocks := make([]mkv.Block, len(all))
	for i, s := range all {
		blocks[i] = s.b
	}
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 4000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{EndMs: 2000}, {StartMs: 2000}}})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(context.Background(), parts, joined))
	for _, b := range trackBlocks(collectAllBlocks(t, joined))[1] {
		if b.Keyframe && b.Timecode > 0 && b.Timecode != 2040 {
			t.Errorf("the joined keyframe of the second part sits at %d ms, want 2040", b.Timecode)
		}
	}
}

// TestJoinOfPartsWithSoundMuxedFarAhead pins the linked seam on a source whose sound is stored 1.2 s ahead of its
// picture: the part before the cut ends its sound that much past its picture, the next one starts its sound there,
// and the join places the next part at its picture's cut, not at the end of the sound before it.
func TestJoinOfPartsWithSoundMuxedFarAhead(t *testing.T) {
	type stored struct {
		dts int64
		b   mkv.Block
	}
	var all []stored
	for j := int64(0); j < 100; j++ {
		all = append(all, stored{j * 40, mkv.Block{TrackNumber: 1, Timecode: j * 40, Keyframe: j%25 == 0, Data: []byte{0, 0, 0, 1, 0x65, byte(j)}}})
	}
	for k := int64(0); k < 200; k++ { // two sound tracks, muxed 1.2 s and 1.3 s ahead
		all = append(all, stored{k*20 - 1200, mkv.Block{TrackNumber: 2, Timecode: k * 20, Keyframe: true, Data: []byte{0xAA, byte(k)}}})
		all = append(all, stored{k*20 - 1300, mkv.Block{TrackNumber: 3, Timecode: k * 20, Keyframe: true, Data: []byte{0xBB, byte(k)}}})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].dts < all[b].dts })
	blocks := make([]mkv.Block, len(all))
	for i, s := range all {
		blocks[i] = s.b
	}
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2), audioTrack(3)}, blocks, 4000)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{EndMs: 2000}, {StartMs: 2000}}})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(context.Background(), parts, joined))
	got := trackBlocks(collectAllBlocks(t, joined))
	for _, b := range got[1] {
		if b.Keyframe && b.Timecode%1000 != 0 {
			t.Errorf("a joined keyframe sits at %d ms, the source's are on whole seconds", b.Timecode)
			break
		}
	}
	if n, m := len(got[2]), len(got[3]); n != 200 || m != 200 {
		t.Errorf("the join holds %d and %d audio frames, the source 200 each", n, m)
	}
}

// TestSplitOfACutSourceSaysSo pins that a split running into the end of a source cut short (a download not
// finished) is refused as ErrSourceTruncated, saying where the file stops and what to do, not as a bare EOF.
func TestSplitOfACutSourceSaysSo(t *testing.T) {
	c := matrix.Case{Audio: true, Cuts: 1}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mustNil(t, c.BuildMKV(src))
	data, err := os.ReadFile(src)
	mustNil(t, err)
	mustNil(t, os.WriteFile(src, data[:len(data)*6/10], 0o644))
	_, err = Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: c.Ranges()})
	if !errors.Is(err, ErrSourceTruncated) || !strings.Contains(err.Error(), "once the file is complete") {
		t.Errorf("err = %v, want ErrSourceTruncated saying to split once the file is complete", err)
	}
}

// TestJoinOfMillisecondRoundedFramesIsExact pins a split then join of 23.976 fps video stamped to the millisecond
// (frames 41 and 42 ms apart): every part's last picture ends exactly at its cut, so each seam lands on the source's
// keyframe, not a millisecond off on either side, which a film of a hundred chapters accumulated.
func TestJoinOfMillisecondRoundedFramesIsExact(t *testing.T) {
	var blocks []mkv.Block
	for j := 0; j < 240; j++ { // 10 s, a keyframe every 24 frames (1001 ms)
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(math.Round(float64(j) * 1001 / 24)), Keyframe: j%24 == 0, Data: []byte{0, 0, 0, 1, 0x65, byte(j)}})
	}
	// Stored as an HEVC B-pyramid is: each keyframe, then every 4 pictures the 4th, 2nd, 1st and 3rd, so the picture
	// shown last before a cut is stored a few ahead of it, an output cluster boundary possibly between.
	var stored []mkv.Block
	for g := 0; g < 240; g += 24 {
		stored = append(stored, blocks[g:g+4]...)
		for j := g + 4; j < g+24; j += 4 { // the last group ends on the picture shown just before the next keyframe
			stored = append(stored, blocks[j+3], blocks[j+1], blocks[j], blocks[j+2])
		}
	}
	// Sound muxed a second ahead of the picture, as on a real 2160p episode: its blocks roll the output cluster
	// over at almost every one.
	var muxed []mkv.Block
	k := int64(0)
	for i, b := range stored {
		for ; k*32 <= int64(float64(i)*1001/24)+1000 && k < 320; k++ {
			muxed = append(muxed, mkv.Block{TrackNumber: 2, Timecode: k * 32, Keyframe: true, Data: []byte{0xAA, byte(k)}})
		}
		muxed = append(muxed, b)
	}
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, muxed, 10240)
	var ranges []mkv.TimeRange
	for s := int64(0); s < 9000; s += 1000 {
		ranges = append(ranges, mkv.TimeRange{StartMs: s, EndMs: s + 1000})
	}
	ranges = append(ranges, mkv.TimeRange{StartMs: 9000})
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: ranges})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(context.Background(), parts, joined))
	got := trackBlocks(collectAllBlocks(t, joined))[1]
	if len(got) != len(stored) {
		t.Fatalf("%d pictures joined, %d in the source", len(got), len(stored))
	}
	for i := range stored {
		if got[i].Timecode != stored[i].Timecode {
			t.Fatalf("picture %d at %d ms, the source has it at %d", i, got[i].Timecode, stored[i].Timecode)
		}
	}
}

// TestSplitHoldsTheLastPictureWrittenBeforeTheCut pins the hold when the picture shown last before a cut was already
// written: sound muxed ahead of the picture opens a new output cluster between it and the cut keyframe (seen on a
// 2160p episode), so the part's last cluster no longer holds it; a join then lands a millisecond off at each seam.
func TestSplitHoldsTheLastPictureWrittenBeforeTheCut(t *testing.T) {
	var blocks []mkv.Block
	for j := 0; j < 48; j++ { // 23.976 fps stamped to the ms: 959 is followed by 1001, 41 ms the smallest gap
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(math.Round(float64(j) * 1001 / 24)), Keyframe: j%24 == 0, Data: []byte{0, 0, 0, 1, 0x65, byte(j)}})
		if j == 23 { // a block of sound a second ahead, stored between the last picture and the keyframe
			blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: 1000, Keyframe: true, Data: []byte{0xAA}})
		}
	}
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1), audioTrack(2)}, blocks, 2002)
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{EndMs: 1000}, {StartMs: 1000}}})
	mustNil(t, err)
	joined := filepath.Join(dir, "j.mkv")
	mustNil(t, Join(context.Background(), parts, joined))
	for _, b := range trackBlocks(collectAllBlocks(t, joined))[1] {
		if b.Keyframe && b.Timecode > 0 && b.Timecode != 1001 {
			t.Errorf("the second part's keyframe joined at %d ms, the source has it at 1001", b.Timecode)
		}
	}
}
