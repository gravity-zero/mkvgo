package ops

// cuehealth_tail_test.go pins WHAT the tail of the video cue coverage is
// measured against. The shapes are the ones measured on a real library: four
// files in five condemned as "index-sparse" were cued every 1-10 s and their
// only "hole" was the declared duration - an audio track's end - lying 31 to
// 108 s past the last frame of picture; the fifth had 771 keyframes and 771
// cues, and its 50-65 s holes were picture missing from the stream.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// videoStatsTags is a statistics tag set as mkvmerge writes one, targeting track uid.
func videoStatsTags(uid uint64, app, duration string, frames int64) []mkv.Tag {
	st := []mkv.SimpleTag{
		{Name: "DURATION", Value: duration},
		{Name: "_STATISTICS_WRITING_APP", Value: app},
		{Name: "_STATISTICS_TAGS", Value: "BPS DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES"},
	}
	if frames > 0 {
		st = append(st, mkv.SimpleTag{Name: "NUMBER_OF_FRAMES", Value: strconv.FormatInt(frames, 10)})
	}
	return []mkv.Tag{{TargetID: uid, SimpleTags: st}}
}

// TestCueHealthTailIsSoundOutlastingPicture is the 48-minute episode cued every
// 2 s whose audio runs 44 s past the last frame: with the picture's own end
// stated, the tail is one GOP and the index is healthy. The same file with no
// statistics is judged against the declared end - and 44 s on 49 minutes is
// what an outlasting track accounts for, so it stays healthy, tail reported.
func TestCueHealthTailIsSoundOutlastingPicture(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	const declaredMs = 2_932_704     // the audio's end
	cues := cueSpread(1, 1445, 2002) // last cue at 2,890,888 ms

	t.Run("picture end stated", func(t *testing.T) {
		path := buildMKVWithCuesTags(t, dir, "exact.mkv", tracks, cueHealthFixtureSets(4), cues, declaredMs,
			videoStatsTags(1, "test", "00:48:12.680000000", 0))
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if !r.VideoEndExact || r.VideoEndMs != 2_892_680 {
			t.Fatalf("VideoEndMs = %d exact=%v, want the DURATION tag 2892680 exact", r.VideoEndMs, r.VideoEndExact)
		}
		if r.TailGapMs != 2_892_680-2_890_888 {
			t.Errorf("TailGapMs = %d, want last cue to the picture's end (1792)", r.TailGapMs)
		}
		if !r.Healthy || r.MaxVideoGapMs != 2002 {
			t.Errorf("Healthy = %v, MaxVideoGapMs = %d (%s): the cues run to the last GOP", r.Healthy, r.MaxVideoGapMs, r.Reason)
		}
	})
	t.Run("no statistics", func(t *testing.T) {
		path := buildMKVWithCuesDur(t, dir, "inexact.mkv", tracks, cueHealthFixtureSets(4), cues, declaredMs)
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if r.VideoEndExact || r.VideoEndMs != declaredMs {
			t.Fatalf("VideoEndMs = %d exact=%v, want the declared duration, inexact", r.VideoEndMs, r.VideoEndExact)
		}
		if r.TailGapMs != declaredMs-2_890_888 {
			t.Errorf("TailGapMs = %d, want last cue to the declared end (41816), reported even when it does not count", r.TailGapMs)
		}
		if !r.Healthy || r.MaxVideoGapMs != 2002 {
			t.Errorf("Healthy = %v, MaxVideoGapMs = %d (%s): 42 s on 49 min is an outlasting track, not a hole", r.Healthy, r.MaxVideoGapMs, r.Reason)
		}
	})
}

// TestCueHealthHalfIndexedStaysSparse keeps the rule the tail exists for: cues
// that stop while the picture goes on. With the picture's end stated, ANY tail
// past the threshold counts; without it, a tail this long is far past what an
// outlasting track accounts for. Both name the tail, and where it starts.
func TestCueHealthHalfIndexedStaysSparse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	cues := cueSpread(1, 60, 1000) // dense for a minute, then nothing of a 10-minute file

	for _, tc := range []struct {
		name string
		tags []mkv.Tag
		end  string
	}{
		{"picture end stated", videoStatsTags(1, "test", "00:10:00.000000000", 0), "the picture ends (00:10:00)"},
		{"no statistics", nil, "the declared end (00:10:00)"},
	} {
		path := buildMKVWithCuesTags(t, dir, tc.name+".mkv", tracks, cueHealthFixtureSets(4), cues, 600_000, tc.tags)
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if r.Healthy || r.MaxVideoGapMs != 541_000 || r.MaxVideoGapAtMs != 59_000 {
			t.Errorf("%s: Healthy=%v MaxVideoGapMs=%d at %d, want the 541 s tail from the last cue at 59 s", tc.name, r.Healthy, r.MaxVideoGapMs, r.MaxVideoGapAtMs)
		}
		if !strings.Contains(r.Reason, "stop at 00:00:59, 541s before "+tc.end) {
			t.Errorf("%s: Reason = %q, want the tail named with its start and what it is measured to", tc.name, r.Reason)
		}
	}
}

// TestCueHealthStaleStatisticsAreNotTrusted covers the tag that no longer
// describes the file: copied through a remux by another application, or stating
// a picture longer than the file. Either way the picture's end is unknown and
// the verdict falls back to the declared duration.
func TestCueHealthStaleStatisticsAreNotTrusted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	cues := cueSpread(1, 300, 1000)

	for _, tc := range []struct {
		name string
		tags []mkv.Tag
	}{
		{"other writing app", videoStatsTags(1, "mkvmerge v61.0.0 ('So') 64-bit", "00:05:00.000000000", 0)},
		{"duration past the file", videoStatsTags(1, "test", "01:30:00.000000000", 0)},
		{"unparseable duration", videoStatsTags(1, "test", "9999999999999:99:99.999999999", 0)},
	} {
		path := buildMKVWithCuesTags(t, dir, tc.name+".mkv", tracks, cueHealthFixtureSets(4), cues, 320_000, tc.tags)
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if r.VideoEndExact || r.VideoEndMs != 320_000 {
			t.Errorf("%s: VideoEndMs = %d exact=%v, want the declared 320000, inexact", tc.name, r.VideoEndMs, r.VideoEndExact)
		}
		if !r.Healthy {
			t.Errorf("%s: Healthy = false (%s)", tc.name, r.Reason)
		}
	}
}

// TestCueHealthHoleIsLocated: a hole in the middle is named by where it opens,
// and the remedy is the reindex - nothing says the picture is missing.
func TestCueHealthHoleIsLocated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	// Every second to 100 s, nothing to 200 s, every second to 300 s.
	cues := cueSpread(1, 101, 1000)
	for ms := int64(200_000); ms <= 300_000; ms += 1000 {
		cues = append(cues, mkv.CuePoint{TimeMs: ms, Track: 1, ClusterPos: 100})
	}
	path := buildMKVWithCuesTags(t, dir, "hole.mkv", tracks, cueHealthFixtureSets(4), cues, 300_000,
		videoStatsTags(1, "test", "00:05:00.000000000", 0))
	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Healthy || r.MaxVideoGapMs != 100_000 || r.MaxVideoGapAtMs != 100_000 {
		t.Fatalf("Healthy=%v MaxVideoGapMs=%d at %d (%s), want the 100 s hole at 100 s", r.Healthy, r.MaxVideoGapMs, r.MaxVideoGapAtMs, r.Reason)
	}
	if r.TailGapMs != 0 || r.VideoShortfallMs != 0 {
		t.Errorf("TailGapMs=%d VideoShortfallMs=%d, want 0: the cues reach the end and no statistics say frames are missing", r.TailGapMs, r.VideoShortfallMs)
	}
	if !strings.Contains(r.Reason, "100s hole at 00:01:40") || !strings.HasSuffix(r.Reason, "run mkvgo reindex") {
		t.Errorf("Reason = %q, want the hole located and the reindex as remedy", r.Reason)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, "index-sparse"); f == nil || f.Remedy != "mkvgo reindex" {
		t.Errorf("Diagnose finding = %+v, want index-sparse with the reindex as remedy", f)
	}
}

// TestCueHealthPictureMissingFromStream is the episode whose cues match its
// keyframes one for one and whose 50-65 s holes are frames absent from the
// video track - the statistics say so head-only (NUMBER_OF_FRAMES falls short
// of the duration at the frame rate). A reindex leaves the hole where it is,
// and the verdict must not send anyone to rebuild the index, let alone
// re-encode: the source is what is missing.
func TestCueHealthPictureMissingFromStream(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	v := videoTrack(1)
	v.DefaultDurationNs = 40_000_000 // 25 fps
	tracks := []mkv.Track{v, audioTrack(2)}
	// Cues every 4 s except a 60 s hole at 200 s; the picture is 300 s at
	// 25 fps = 7500 frames, the track states 6000: 60 s of picture absent.
	var cues []mkv.CuePoint
	for ms := int64(0); ms <= 300_000; ms += 4000 {
		if ms > 200_000 && ms < 260_000 {
			continue
		}
		cues = append(cues, mkv.CuePoint{TimeMs: ms, Track: 1, ClusterPos: 100})
	}
	path := buildMKVWithCuesTags(t, dir, "missing.mkv", tracks, cueHealthFixtureSets(4), cues, 300_000,
		videoStatsTags(1, "test", "00:05:00.000000000", 6000))
	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if r.VideoShortfallMs != 60_000 {
		t.Fatalf("VideoShortfallMs = %d, want 60000 (7500 frames expected, 6000 stated, at 25 fps)", r.VideoShortfallMs)
	}
	if r.Healthy || r.MaxVideoGapMs != 60_000 || r.MaxVideoGapAtMs != 200_000 {
		t.Fatalf("Healthy=%v MaxVideoGapMs=%d at %d (%s)", r.Healthy, r.MaxVideoGapMs, r.MaxVideoGapAtMs, r.Reason)
	}
	if !strings.Contains(r.Reason, "60s hole at 00:03:20") || !strings.Contains(r.Reason, "missing there") || strings.Contains(r.Reason, "run mkvgo reindex") {
		t.Errorf("Reason = %q, want the hole located, the picture named missing, and no reindex as remedy", r.Reason)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, "index-sparse"); f == nil || !strings.HasPrefix(f.Remedy, "re-acquire the source") {
		t.Errorf("Diagnose finding = %+v, want index-sparse whose remedy is the source, not a reindex", f)
	}
}
