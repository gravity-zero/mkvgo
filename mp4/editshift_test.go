package mp4

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// editShiftFixture builds a real MP4 (via the remux) with one video and one
// audio track, both starting at 0.
func editShiftFixture(t *testing.T, fastStart bool) string {
	t.Helper()
	src := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))
	dst := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), src, dst, Options{FastStart: fastStart}); err != nil {
		t.Fatal(err)
	}
	return dst
}

// measuredAudioDelayMs measures the MP4's real presentation delay of the
// audio track through independent machinery: remux back to Matroska (the
// demux applies the edit list to block times) and read the first audio block
// time against the first video block time from the block stream.
func measuredAudioDelayMs(t *testing.T, mp4Path string) int64 {
	t.Helper()
	mkvPath := filepath.Join(t.TempDir(), "back.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, mkvPath); err != nil {
		t.Fatalf("remux back: %v", err)
	}
	f, err := os.Open(mkvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	firstVideo, firstAudio := int64(-1), int64(-1)
	for firstVideo < 0 || firstAudio < 0 {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch b.TrackNumber {
		case 1:
			if firstVideo < 0 {
				firstVideo = b.Timecode
			}
		case 2:
			if firstAudio < 0 {
				firstAudio = b.Timecode
			}
		}
	}
	if firstVideo < 0 || firstAudio < 0 {
		t.Fatal("block stream misses a track")
	}
	return firstAudio - firstVideo
}

// TestMP4RetimeTracks: delaying then cancelling a track's presentation
// round-trips through real parsers, on both moov layouts (tail rewrite and
// the faststart append-and-retire path).
func TestMP4RetimeTracks(t *testing.T) {
	for _, fastStart := range []bool{false, true} {
		name := "moov-at-tail"
		if fastStart {
			name = "faststart-append-retire"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := editShiftFixture(t, fastStart)

			// Delay the audio by 300ms: an empty edit appears.
			if err := RetimeTracks(ctx, path, map[uint64]int64{2: 300_000_000}); err != nil {
				t.Fatal(err)
			}
			if got := measuredAudioDelayMs(t, path); got != 300 {
				t.Errorf("after +300ms: measured delay = %dms, want 300", got)
			}
			// The repaired file still parses head-only with both tracks.
			c, _, err := OpenMeta(ctx, path)
			if err != nil {
				t.Fatalf("repaired file does not parse: %v", err)
			}
			if len(c.Tracks) != 2 {
				t.Fatalf("tracks = %d, want 2", len(c.Tracks))
			}

			// Cancel it: the presentation is back at 0.
			if err := RetimeTracks(ctx, path, map[uint64]int64{2: -300_000_000}); err != nil {
				t.Fatal(err)
			}
			if got := measuredAudioDelayMs(t, path); got != 0 {
				t.Errorf("after cancelling: measured delay = %dms, want 0", got)
			}
		})
	}
}

// TestMP4RetimeTracksRefusals: the explicit refusals - presenting before the
// start, unknown track, zero shift.
func TestMP4RetimeTracksRefusals(t *testing.T) {
	ctx := context.Background()
	path := editShiftFixture(t, false)

	err := RetimeTracks(ctx, path, map[uint64]int64{2: -100_000_000})
	if err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("presenting before 0 must refuse with the reason, got %v", err)
	}
	if err := RetimeTracks(ctx, path, map[uint64]int64{9: 100_000_000}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown track must refuse, got %v", err)
	}
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: 0}); err == nil {
		t.Error("a zero shift must refuse")
	}
	if err := RetimeTracks(ctx, path, nil); err == nil {
		t.Error("an empty shift map must refuse")
	}

	// The refusals left the file byte-identical.
	if got := measuredAudioDelayMs(t, path); got != 0 {
		t.Errorf("refused operations must not modify the file (delay %dms)", got)
	}
}

// TestMP4RetimeTracksFastStartRetiresOldMoov: the faststart path appends the
// new moov and retires the old one to a free box, growing the file by
// exactly the new moov.
func TestMP4RetimeTracksFastStartRetiresOldMoov(t *testing.T) {
	ctx := context.Background()
	path := editShiftFixture(t, true)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: 250_000_000}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Fatalf("faststart repair must append the new moov (size %d -> %d)", len(before), len(after))
	}
	// The prefix is untouched except the old moov's 4-byte type flip.
	diffs := 0
	for i := range before {
		if before[i] != after[i] {
			diffs++
		}
	}
	if diffs != 4 {
		t.Errorf("the existing bytes must change by exactly the 4-byte moov->free flip, got %d differing bytes", diffs)
	}
}

// TestMP4RetimeTracksMemFS: the op honours the FS port (no OS filesystem).
func TestMP4RetimeTracksMemFS(t *testing.T) {
	ctx := context.Background()
	path := editShiftFixture(t, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := mkv.NewMemFS()
	m.Put("in.mp4", data)
	if err := RetimeTracks(ctx, "in.mp4", map[uint64]int64{2: 300_000_000}, Options{FS: m.FS()}); err != nil {
		t.Fatal(err)
	}
	c, _, err := OpenMeta(ctx, "in.mp4", Options{FS: m.FS()})
	if err != nil || len(c.Tracks) != 2 {
		t.Fatalf("repaired in-memory file does not parse: %v (%d tracks)", err, len(c.Tracks))
	}
}
