package ops

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestJoin_CodecMismatch covers the codec validation: joining tracks that line up
// by count and type but differ in codec (or codec-private) must be rejected, not
// silently produce a broken file.
func TestJoin_CodecMismatch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	src1 := buildMinimalMKV(t, dir, "a.mkv", []mkv.Track{videoTrack(1)}, testBlocks(1), 200)

	hevc := videoTrack(1)
	hevc.Codec = "hevc" // same id/type, different codec
	src2 := buildMinimalMKV(t, dir, "b.mkv", []mkv.Track{hevc}, testBlocks(1), 200)
	if err := Join(ctx, []string{src1, src2}, filepath.Join(dir, "o1.mkv")); err == nil || !strings.Contains(err.Error(), "codec") {
		t.Fatalf("codec mismatch should be rejected, got %v", err)
	}

	other := videoTrack(1)
	other.CodecPrivate = []byte{0x02} // same codec, different configuration
	src3 := buildMinimalMKV(t, dir, "c.mkv", []mkv.Track{other}, testBlocks(1), 200)
	if err := Join(ctx, []string{src1, src3}, filepath.Join(dir, "o2.mkv")); err == nil || !strings.Contains(err.Error(), "codec configuration") {
		t.Fatalf("codec-private mismatch should be rejected, got %v", err)
	}
}

// TestJoin_SparseTrackStaysWithItsFile is the case a real episode exposed and
// the fixtures above are too short to reach: a subtitle track carrying a couple
// of forced cues near the start and nothing for the next 40 seconds.
//
// Rebased on its own end, such a track came back almost at the beginning of the
// next file - tens of seconds ahead of the video it belongs to. Past ~32 s the
// block no longer fits SimpleBlock's int16 timecode relative to the cluster the
// video opened, so the join did not even produce a file; below that threshold it
// produced a silently desynchronised one, which is worse.
func TestJoin_SparseTrackStaysWithItsFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2), subtitleTrack(3, "srt")}

	const (
		lastMs   = 40000 // 40 s of A/V ...
		stepMs   = 100
		subCueMs = 100 // ... and one subtitle cue, at the very start
	)
	// One cluster per second, like a real file: a single cluster spanning 40 s
	// could not hold its own blocks (int16 again), and the subject here is the
	// join, not the fixture.
	var sets [][]mkv.Block
	for base := int64(0); base <= lastMs; base += 1000 {
		var cluster []mkv.Block
		for tc := base; tc < base+1000 && tc <= lastMs; tc += stepMs {
			cluster = append(cluster,
				mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")},
				mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
			if tc == subCueMs {
				cluster = append(cluster, mkv.Block{TrackNumber: 3, Timecode: tc, Keyframe: true, Data: []byte("sub")})
			}
		}
		sets = append(sets, cluster)
	}
	src := buildMultiClusterMKV(t, dir, "part.mkv", tracks, sets, lastMs+stepMs)

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{src, src}, dst); err != nil {
		t.Fatalf("join failed: %v - a sparse track dragged out of its cluster's int16 range", err)
	}

	jf, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer jf.Close()
	br, err := reader.NewBlockReader(jf, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	var firstVideoOf2nd, subs []int64
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case blk.TrackNumber == 3:
			subs = append(subs, blk.Timecode)
		case blk.TrackNumber == 1 && blk.Timecode > lastMs && firstVideoOf2nd == nil:
			firstVideoOf2nd = append(firstVideoOf2nd, blk.Timecode)
		}
	}
	if len(subs) != 2 || len(firstVideoOf2nd) != 1 {
		t.Fatalf("subtitle blocks = %v, second source's first video block = %v", subs, firstVideoOf2nd)
	}
	// The cue sits 100 ms into its own file, before and after the join.
	if want := firstVideoOf2nd[0] + subCueMs; subs[1] != want {
		t.Errorf("second file's cue at %d ms, want %d (its video resumes at %d): the subtitle track "+
			"slid %d ms away from the picture it belongs to",
			subs[1], want, firstVideoOf2nd[0], want-subs[1])
	}
}

// TestJoin_SeamKeepsTracksAligned covers the concatenation seam when tracks end
// at different times. Two properties have to hold at once, and they pull in
// opposite directions:
//
//   - every track shifts by the SAME amount, so the alignment the tracks had in
//     the source survives the join. Rebasing each track on its own end (which
//     this used to do) buys a contiguous track at the price of desynchronising
//     it from the others - the more sparse the track, the bigger the slide;
//   - that shared amount is the last frame's END, measured, not the container's
//     DECLARED duration, so a container that declares more than it holds does
//     not open a hole at the seam.
func TestJoin_SeamKeepsTracksAligned(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Video (track 1) ends at 50; audio (track 2) ends at 120 - different ends.
	blocks := func() []mkv.Block {
		bs := []mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 1, Timecode: 50, Keyframe: true, Data: []byte("v")},
		}
		for tc := int64(0); tc <= 120; tc += 20 {
			bs = append(bs, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")})
		}
		return bs
	}
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	src1 := buildMinimalMKV(t, dir, "a.mkv", tracks, blocks(), 150)
	src2 := buildMinimalMKV(t, dir, "b.mkv", tracks, blocks(), 150)

	dst := filepath.Join(dir, "joined.mkv")
	if err := Join(ctx, []string{src1, src2}, dst); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	var video, audio []int64
	for {
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch blk.TrackNumber {
		case 1:
			video = append(video, blk.Timecode)
		case 2:
			audio = append(audio, blk.Timecode)
		}
	}
	if len(video) != 4 {
		t.Fatalf("video blocks = %d, want 4 (%v)", len(video), video)
	}

	// Both tracks start their second copy at the same instant: that instant IS
	// the seam. Per-track rebasing put video at 50 and audio at 140 - the two
	// tracks of one source torn 90 ms apart.
	videoSeam := video[2]
	seamIdx := len(audio) - 7 // the second source contributes 7 audio blocks (0..120 by 20)
	audioSeam := audio[seamIdx]
	if videoSeam != audioSeam {
		t.Errorf("seam: video resumes at %d ms but audio at %d ms - the join desynchronised the "+
			"tracks by %d ms (video %v, audio %v)", videoSeam, audioSeam, audioSeam-videoSeam, video, audio)
	}

	// ... and the seam is the measured end (audio's last frame, 120 + 20 = 140),
	// NOT the 150 ms the containers declare - otherwise a padded declaration
	// would insert dead time at every join.
	if videoSeam != 140 {
		t.Errorf("seam at %d ms, want 140 (last frame's end); 150 means the DECLARED duration was "+
			"used and the seam holds a hole", videoSeam)
	}
}
