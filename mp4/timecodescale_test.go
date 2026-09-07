package mp4

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// nonDefaultScale is a timebase seen in the wild: roughly 1/48000 s, so 48
// timecode units to the millisecond. Nothing here covered a file whose timebase
// is not the 1 ms default, which is how cue times reached the segment planner
// as raw units and multiplied every announced duration by 48.
const nonDefaultScale = 20832

// buildCuedFixture writes a video-only file with one keyframe per second and a
// cue on each, exactly as a correct rewrite leaves it.
func buildCuedFixture(t *testing.T, dir, name string, scale int64, clusters int) string {
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
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "test", WritingApp: "test"}}
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{{
		ID: 1, Type: mkv.VideoTrack, Codec: "V_MPEG4/ISO/AVC",
		Width: &w, Height: &h, DefaultDurationNs: 40_000_000,
		CodecPrivate: []byte{0x01, 0x64, 0x00, 0x1E, 0xFF, 0xE1, 0x00, 0x04,
			0x67, 0x64, 0x00, 0x1E, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	}}
	if err := mw.WriteMetadata(c, tracks, int64(clusters)*1000); err != nil {
		t.Fatal(err)
	}
	for i := range clusters {
		ts := int64(i) * 1000
		blocks := []mkv.Block{{
			TrackNumber: 1, Timecode: ts, Keyframe: true,
			Data: []byte{0x00, 0x00, 0x00, 0x02, 0x09, 0x10},
		}}
		if err := mw.WriteClusterWithCues(ts, scale, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlanHLSIgnoresTimecodeScale pins that the segment plan is a property of
// the media, not of the file's timebase. Reading cue times as milliseconds when
// they were raw units broke this twice over: the boundary test compared units
// against a millisecond target, so every cue became a boundary and SegmentMs
// was ignored, and every announced duration came out 48x too long. This is the
// guard that stands between that class of defect and a player.
func TestPlanHLSIgnoresTimecodeScale(t *testing.T) {
	ctx := t.Context()
	const clusters = 30

	type plan struct {
		segments int
		total    float64 // seconds the playlist announces
		longest  float64
	}
	got := map[int64]plan{}
	for _, scale := range []int64{mkv.DefaultTimecodeScale, nonDefaultScale} {
		dir := t.TempDir()
		src := buildCuedFixture(t, dir, "src.mkv", scale, clusters)
		p, err := PlanHLS(ctx, src, Options{SegmentMs: 4000})
		if err != nil {
			t.Fatalf("scale %d: PlanHLS: %v", scale, err)
		}
		pl := plan{segments: p.NumSegments()}
		for _, line := range strings.Split(string(p.MediaPlaylist()), "\n") {
			if !strings.HasPrefix(line, "#EXTINF:") {
				continue
			}
			d, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			if err != nil {
				t.Fatalf("scale %d: unreadable %q: %v", scale, line, err)
			}
			pl.total += d
			pl.longest = max(pl.longest, d)
		}
		got[scale] = pl
	}

	ref, odd := got[mkv.DefaultTimecodeScale], got[nonDefaultScale]
	if ref.segments != odd.segments {
		t.Errorf("segment count moved with the timebase: %d at the 1ms default, %d at %d ns/unit",
			ref.segments, odd.segments, nonDefaultScale)
	}
	if odd.segments == clusters {
		t.Errorf("%d segments for %d one-second clusters at 4000ms each - SegmentMs was ignored, "+
			"every cue cleared a millisecond threshold it was compared against in raw units",
			odd.segments, clusters)
	}
	// The playlist must describe the media that is there. Exact equality is too
	// strong: a coarser timebase quantises cue times a millisecond or two low,
	// so a boundary can legitimately fall on the next keyframe. What must never
	// happen is the duration scaling with the timebase - 30s announced as 1440s.
	for scale, p := range got {
		if p.total < float64(clusters)-1 || p.total > float64(clusters)+1 {
			t.Errorf("scale %d: playlist announces %.3fs of media, the file holds %ds",
				scale, p.total, clusters)
		}
		if p.longest > 6 {
			t.Errorf("scale %d: longest segment %.3fs for a 4000ms target", scale, p.longest)
		}
	}
}
