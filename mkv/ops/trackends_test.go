package ops

// trackends_test.go pins how a track's real end is established - statistics
// when they describe the file, a bounded tail walk otherwise - and what
// Diagnose makes of an audio track that dies before the picture.

import (
	"context"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// endsFixture is 5 s clusters of picture (keyframed, cued) to videoMs and
// sound to audioMs, declared declaredMs long. Audio blocks carry no duration,
// the track a 20 ms default one, so its end is the last block plus 20 ms.
func endsFixture(t *testing.T, dir, name string, videoMs, audioMs, declaredMs int64, tags []mkv.Tag) string {
	t.Helper()
	a := audioTrack(2)
	a.DefaultDurationNs = 20_000_000
	tracks := []mkv.Track{videoTrack(1), a}
	var sets [][]mkv.Block
	for ts := int64(0); ts < videoMs || ts < audioMs; ts += 5000 {
		var blocks []mkv.Block
		if ts < videoMs {
			blocks = append(blocks, fullCluster(ts, true)[:5]...)
		}
		if ts < audioMs {
			blocks = append(blocks, audioOnlyCluster(ts)...)
		}
		sets = append(sets, blocks)
	}
	if tags != nil {
		// Tags in the head: write through the tag-aware builder, cueing every
		// keyframe at a made-up position is fine, statistics need no walk.
		var cues []mkv.CuePoint
		for _, s := range sets {
			if s[0].TrackNumber == 1 {
				cues = append(cues, mkv.CuePoint{TimeMs: s[0].Timecode, Track: 1, ClusterPos: 100})
			}
		}
		return buildMKVWithCuesTags(t, dir, name, tracks, sets, cues, declaredMs, tags)
	}
	return buildMKVProbeFixture(t, dir, name, tracks, sets, declaredMs, func(mkv.Block) bool { return true })
}

func endOf(r *TrackEndsReport, track uint64) mkv.TrackEnd {
	for _, e := range r.Ends {
		if e.Track == track {
			return e
		}
	}
	return mkv.TrackEnd{}
}

func TestTrackEndsFromStatistics(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tags := append(videoStatsTags(1, "test", "00:05:00.000000000", 0), videoStatsTags(2, "test", "00:02:30.000000000", 0)...)
	path := endsFixture(t, dir, "stats.mkv", 300_000, 300_000, 300_000, tags)

	r, err := TrackEnds(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if v := endOf(r, 1); v.Source != "statistics" || v.EndMs != 300_000 {
		t.Errorf("video end = %+v, want 300000 from statistics", v)
	}
	if a := endOf(r, 2); a.Source != "statistics" || a.EndMs != 150_000 {
		t.Errorf("audio end = %+v, want 150000 from statistics", a)
	}
	if r.VideoEndMs != 300_000 || r.AudioShortfallMs != 150_000 || r.ShortAudioTrack != 2 {
		t.Errorf("report = %+v, want the audio 150 s short of the picture", r)
	}
}

// TestTrackEndsWalksTheTail: no statistics, so the ends come from the tail
// walk - the first window sees the video's last block, the widened one finds
// the audio's, 100 s in on a 300 s file.
func TestTrackEndsWalksTheTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := endsFixture(t, dir, "walk.mkv", 300_000, 100_000, 300_000, nil)

	r, err := TrackEnds(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// The last video block sits at 299 s (keyframe at 295 s plus four
	// 1 s frames) and carries no duration nor default: it ends where it starts.
	if v := endOf(r, 1); v.Source != "walk" || v.EndMs != 299_000 {
		t.Errorf("video end = %+v, want 299000 from the walk", v)
	}
	// The last audio block sits at 99 s and plays 20 ms.
	if a := endOf(r, 2); a.Source != "walk" || a.EndMs != 99_020 {
		t.Errorf("audio end = %+v, want 99020 from the walk", a)
	}
	if r.AudioShortfallMs != 299_000-99_020 || r.ShortAudioTrack != 2 {
		t.Errorf("shortfall = %d on track %d, want 199980 on track 2", r.AudioShortfallMs, r.ShortAudioTrack)
	}

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	f := hasFinding(d, "audio-short")
	if f == nil || f.Track != 2 || !strings.Contains(f.Detail, "ends 200s before the picture") || !strings.HasPrefix(f.Remedy, "re-acquire the source") {
		t.Errorf("Diagnose finding = %+v, want audio-short on track 2, 200 s, remedy the source", f)
	}
	if d.TrackEnds == nil || d.TrackEnds.AudioShortfallMs != r.AudioShortfallMs {
		t.Errorf("Diagnosis.TrackEnds = %+v, want the report attached", d.TrackEnds)
	}
}

// TestTrackEndsBoundsASilentTrack: the audio dies at 50 s of a 1000 s file,
// before even the widest window - its end is reported as a bound, the
// shortfall as at least that.
func TestTrackEndsBoundsASilentTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := endsFixture(t, dir, "bound.mkv", 1_000_000, 50_000, 1_000_000, nil)

	r, err := TrackEnds(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if a := endOf(r, 2); a.Source != "walk-bound" || a.EndMs != 100_000 {
		t.Errorf("audio end = %+v, want a 100000 bound (the 900 s window's start)", a)
	}
	if r.AudioShortfallMs != 999_000-100_000 {
		t.Errorf("shortfall = %d, want the lower bound 899000", r.AudioShortfallMs)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, "audio-short"); f == nil || !strings.Contains(f.Detail, "ends at least 899s before") {
		t.Errorf("Diagnose finding = %+v, want the bound spelled out", f)
	}
}

// TestTrackEndsHealthyFileHasNoShortfall: sound outlasting picture is the
// ordinary shape and not a finding; the report still says where each ends.
func TestTrackEndsHealthyFileHasNoShortfall(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := endsFixture(t, dir, "fine.mkv", 200_000, 240_000, 240_000, nil)

	r, err := TrackEnds(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if r.AudioShortfallMs != 0 || r.VideoEndMs != 199_000 {
		t.Errorf("report = %+v, want no shortfall and the picture ending at 199 s", r)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, "audio-short"); f != nil {
		t.Errorf("unexpected finding %+v", f)
	}
}
