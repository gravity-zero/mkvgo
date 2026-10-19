package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// damagedSubtitleSource writes ten 1 s clusters, each a video keyframe, a PGS cue at c s + 100 ms cleared at + 600 ms
// (cluster 3's never cleared), a text cue at c s + 200 ms and an ASS one at + 300 ms, then zeroes cluster 4 and the head of cluster 5: the
// walk resumes on cluster 6. It returns the clean and the damaged paths and the clusters' offsets.
func damagedSubtitleSource(t *testing.T) (clean, bad string, clusters []int64) {
	t.Helper()
	w, h := uint32(320), uint32(240)
	var sets [][]mkv.Block
	for c := int64(0); c < 10; c++ {
		set := []mkv.Block{
			{TrackNumber: 1, Timecode: c * 1000, Keyframe: true, Data: append([]byte{0, 0, 1, 0x2D, 0x65}, make([]byte, 32<<10)...)},
			{TrackNumber: 2, Timecode: c*1000 + 100, Keyframe: true, Data: pgsShow(uint16(c), 100, 900, true)},
			{TrackNumber: 3, Timecode: c*1000 + 200, Keyframe: true, Duration: 300, Data: []byte(fmt.Sprintf("cue %d", c))},
			{TrackNumber: 4, Timecode: c*1000 + 300, Keyframe: true, Duration: 300, Data: []byte(fmt.Sprintf("%d,0,Default,,0,0,0,,cue %d", c, c))},
		}
		if c != 3 {
			set = append(set, mkv.Block{TrackNumber: 2, Timecode: c*1000 + 600, Keyframe: true, Data: pgsClear()})
		}
		sets = append(sets, set)
	}
	dir := t.TempDir()
	clean = buildMKVCuedBlocks(t, dir, "clean.mkv", []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
		{ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: "pgs"},
		{ID: 3, UID: 33, Type: mkv.SubtitleTrack, Codec: "srt"},
		{ID: 4, UID: 44, Type: mkv.SubtitleTrack, Codec: "ass", CodecPrivate: []byte("[Script Info]")},
	}, sets, 10000, nil)
	data, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; ; {
		i := bytes.Index(data[off:], []byte{0x1F, 0x43, 0xB6, 0x75})
		if i < 0 {
			break
		}
		clusters = append(clusters, int64(off+i))
		off += i + 4
	}
	if len(clusters) != 10 {
		t.Fatalf("fixture: %d clusters, want 10", len(clusters))
	}
	clear(data[clusters[4] : clusters[5]+100])
	bad = filepath.Join(dir, "bad.mkv")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return clean, bad, clusters
}

// wantDamagedSubtitleRange is the one region every read of damagedSubtitleSource reports: cluster 4 up to cluster 6,
// between the 3 s and the 6 s clusters.
func wantDamagedSubtitleRange(clusters []int64) mkv.DamagedRange {
	return mkv.DamagedRange{StartOffset: clusters[4], EndOffset: clusters[6], ApproxStartMs: 3000, ApproxEndMs: 6000}
}

// checkPGSPastDamage checks a PGS read delivered the cues at want, and closed cluster 3's cue at openEnd instead of
// stretching it to the next cue past the region.
func checkPGSPastDamage(t *testing.T, got []PGSCue, what string, want []int64, openEnd int64) {
	t.Helper()
	var starts []int64
	for _, c := range got {
		starts = append(starts, c.StartMs)
		if c.StartMs == 3100 && c.EndMs != openEnd {
			t.Errorf("%s: the cue the region left open ends at %d ms, want %d", what, c.EndMs, openEnd)
		}
	}
	if fmt.Sprint(starts) != fmt.Sprint(want) {
		t.Errorf("%s: cues at %v, want %v", what, starts, want)
	}
}

// onSkipInto returns Options whose OnSkip appends to got.
func onSkipInto(got *[]mkv.DamagedRange) mkv.Options {
	return mkv.Options{OnSkip: func(r mkv.DamagedRange) { *got = append(*got, r) }}
}

func checkSkips(t *testing.T, got []mkv.DamagedRange, want mkv.DamagedRange, what string) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s: OnSkip got %+v, want once %+v", what, got, want)
	}
}

// TestSubtitleReadsPassOverDamage: every subtitle read of a file zeroed over two clusters delivers the rest of the
// track, reports the region once through OnSkip, and still refuses it when DamageBytes is negative or too small.
func TestSubtitleReadsPassOverDamage(t *testing.T) {
	ctx := context.Background()
	clean, bad, clusters := damagedSubtitleSource(t)
	want := wantDamagedSubtitleRange(clusters)
	// A walk loses clusters 4 and 5 and knows cluster 3's time alone; an index of the clean file seats on cluster
	// 5's cue, whose block lies past the zeroed head, and on cluster 4's, timed 4 s.
	walked, seated := []int64{100, 1100, 2100, 3100, 6100, 7100, 8100, 9100}, []int64{100, 1100, 2100, 3100, 5100, 6100, 7100, 8100, 9100}
	defaultEnd := 3100 + int64(defaultSubDurationMs)
	pgsVia := func(read func(fn func(PGSCue) error, o mkv.Options) error) ([]PGSCue, []mkv.DamagedRange, error) {
		var cues []PGSCue
		var skips []mkv.DamagedRange
		err := read(func(c PGSCue) error { cues = append(cues, c); return nil }, onSkipInto(&skips))
		return cues, skips, err
	}

	t.Run("pgs walk", func(t *testing.T) {
		cues, skips, err := pgsVia(func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGS(ctx, bad, 2, fn, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		checkPGSPastDamage(t, cues, "walk", walked, defaultEnd)
		checkSkips(t, skips, want, "walk")
	})

	t.Run("text walk", func(t *testing.T) {
		var skips []mkv.DamagedRange
		var vtt bytes.Buffer
		if err := ExtractSubtitleWebVTT(ctx, bad, 3, &vtt, onSkipInto(&skips)); err != nil {
			t.Fatal(err)
		}
		for c := 0; c < 10; c++ {
			if has := strings.Contains(vtt.String(), fmt.Sprintf("cue %d\n", c)); has == (c == 4 || c == 5) {
				t.Errorf("cue %d delivered: %v", c, has)
			}
		}
		checkSkips(t, skips, want, "text walk")
		out := t.TempDir()
		if err := ExtractSubtitle(ctx, bad, 3, filepath.Join(out, "out.srt")); err != nil {
			t.Fatal(err)
		}
		if err := ExtractASS(ctx, bad, 4, filepath.Join(out, "out.ass")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("built index", func(t *testing.T) {
		var skips []mkv.DamagedRange
		ix, err := BuildSubtitleIndex(ctx, bad, nil, onSkipInto(&skips))
		if err != nil {
			t.Fatal(err)
		}
		checkSkips(t, skips, want, "build")
		cues, skips, err := pgsVia(func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGSFrom(ctx, bad, 2, ix, fn, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		checkPGSPastDamage(t, cues, "built index", walked, defaultEnd)
		if len(skips) != 0 {
			t.Errorf("an index built past the region holds nothing in it, yet OnSkip got %+v", skips)
		}
	})

	t.Run("index of the clean file", func(t *testing.T) {
		ix, err := BuildSubtitleIndex(ctx, clean, nil)
		if err != nil {
			t.Fatal(err)
		}
		cues, skips, err := pgsVia(func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGSFrom(ctx, bad, 2, ix, fn, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		checkPGSPastDamage(t, cues, "clean index", seated, 4000)
		if len(skips) != 1 || skips[0].EndOffset != clusters[6] {
			t.Errorf("clean index: OnSkip got %+v, want once a region ending at %d", skips, clusters[6])
		}
		var vtt bytes.Buffer
		if err := ExtractSubtitleWebVTTFrom(ctx, bad, 3, ix, &vtt); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(vtt.String(), "cue 4") || !strings.Contains(vtt.String(), "cue 6") {
			t.Errorf("clean index, text:\n%s", vtt.String())
		}
	})

	t.Run("index from cues", func(t *testing.T) {
		c, err := reader.OpenMetaWithFS(ctx, bad, nil, reader.WithCues())
		if err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(bad)
		ix, _, err := SubtitleIndexFromCues(c, st.Size(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var skips []mkv.DamagedRange
		if err := ix.Resolve(ctx, bad, onSkipInto(&skips)); err != nil {
			t.Fatal(err)
		}
		if len(skips) != 1 || skips[0].StartOffset != clusters[4] || skips[0].EndOffset != clusters[6] {
			t.Errorf("resolve: OnSkip got %+v, want once [%d, %d)", skips, clusters[4], clusters[6])
		}
		cues, _, err := pgsVia(func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGSFrom(ctx, bad, 2, ix, fn, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		checkPGSPastDamage(t, cues, "cues index", walked, defaultEnd)
	})

	t.Run("garbage at a cue", func(t *testing.T) {
		data, err := os.ReadFile(clean)
		if err != nil {
			t.Fatal(err)
		}
		for i := clusters[4]; i < clusters[4]+64; i++ {
			data[i] = 0xFF // not a download gap: the cue points at bytes that are no Cluster
		}
		odd := filepath.Join(t.TempDir(), "odd.mkv")
		if err := os.WriteFile(odd, data, 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := reader.OpenMetaWithFS(ctx, odd, nil, reader.WithCues())
		if err != nil {
			t.Fatal(err)
		}
		ix, _, err := SubtitleIndexFromCues(c, int64(len(data)), nil)
		if err != nil {
			t.Fatal(err)
		}
		var skips []mkv.DamagedRange
		if err := ix.Resolve(ctx, odd, onSkipInto(&skips)); err == nil {
			t.Errorf("a cue on bytes that are no Cluster resolved as damage (OnSkip %+v)", skips)
		}
	})

	t.Run("zeroed tail", func(t *testing.T) {
		data, err := os.ReadFile(clean)
		if err != nil {
			t.Fatal(err)
		}
		clear(data[clusters[8]+50:]) // a download whose last pieces never came
		tail := filepath.Join(t.TempDir(), "tail.mkv")
		if err := os.WriteFile(tail, data, 0o644); err != nil {
			t.Fatal(err)
		}
		cues, skips, err := pgsVia(func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGS(ctx, tail, 2, fn, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(cues) != 8 || len(skips) != 1 || skips[0].StartOffset <= clusters[8] || skips[0].EndOffset != int64(len(data)) {
			t.Errorf("zeroed tail: %d cues, OnSkip %+v; want 8 cues and once from inside cluster 8 (%d) to %d", len(cues), skips, clusters[8], len(data))
		}
		var vtt bytes.Buffer
		if err := ExtractSubtitleWebVTT(ctx, tail, 3, &vtt); err != nil || !strings.Contains(vtt.String(), "cue 7") {
			t.Errorf("text over a zeroed tail: %v\n%s", err, vtt.String())
		}
	})

	t.Run("before any cluster", func(t *testing.T) {
		data, err := os.ReadFile(clean)
		if err != nil {
			t.Fatal(err)
		}
		clear(data[clusters[0] : clusters[0]+100]) // the walk meets the damage before it entered any Cluster
		head := filepath.Join(t.TempDir(), "head.mkv")
		if err := os.WriteFile(head, data, 0o644); err != nil {
			t.Fatal(err)
		}
		cues, skips, err := pgsVia(func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGS(ctx, head, 2, fn, o)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(skips) != 1 || skips[0].ApproxStartMs < 0 || skips[0].ApproxEndMs < skips[0].ApproxStartMs {
			t.Errorf("%d cues, OnSkip %+v: want one region timed from 0 at the earliest", len(cues), skips)
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, n := range []int64{-1, 1000} {
			err := ForEachSubtitlePGS(ctx, bad, 2, func(PGSCue) error { return nil }, mkv.Options{DamageBytes: n})
			if !errors.Is(err, reader.ErrDamagedRegion) {
				t.Errorf("DamageBytes %d: got %v, want ErrDamagedRegion", n, err)
			}
		}
	})
}

// TestTextCueClosedAtDamage: a text cue without BlockDuration just before a damaged region ends where the region
// starts (the index read knows where), else once read, not at the first cue past it, on the walk and on an index read.
func TestTextCueClosedAtDamage(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var sets [][]mkv.Block
	for c := int64(0); c < 10; c++ {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: c * 1000, Keyframe: true, Data: append([]byte{0, 0, 1, 0x2D, 0x65}, make([]byte, 32<<10)...)},
			{TrackNumber: 3, Timecode: c*1000 + 200, Keyframe: true, Data: []byte(fmt.Sprintf("cue %d", c))},
		})
	}
	dir := t.TempDir()
	clean := buildMKVCuedBlocks(t, dir, "c.mkv", []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
		{ID: 3, UID: 33, Type: mkv.SubtitleTrack, Codec: "srt"},
	}, sets, 10000, nil)
	data, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	var cl []int64
	for off := 0; ; {
		i := bytes.Index(data[off:], []byte{0x1F, 0x43, 0xB6, 0x75})
		if i < 0 {
			break
		}
		cl = append(cl, int64(off+i))
		off += i + 4
	}
	clear(data[cl[4] : cl[8]+100]) // the walk resumes on the 9 s cluster
	bad := filepath.Join(dir, "b.mkv")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ix, err := BuildSubtitleIndex(context.Background(), clean, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The walk knows the region starts past cluster 3 (3 s), the index read at cluster 4 (4 s).
	want := map[string]string{"walk": "00:00:03.200 --> 00:00:04.200\ncue 3", "index": "00:00:03.200 --> 00:00:04.000\ncue 3"}
	for name, read := range map[string]func(*bytes.Buffer) error{
		"walk":  func(b *bytes.Buffer) error { return ExtractSubtitleWebVTT(context.Background(), bad, 3, b) },
		"index": func(b *bytes.Buffer) error { return ExtractSubtitleWebVTTFrom(context.Background(), bad, 3, ix, b) },
	} {
		var vtt bytes.Buffer
		if err := read(&vtt); err != nil {
			t.Fatal(name, err)
		}
		if !strings.Contains(vtt.String(), want[name]) {
			t.Errorf("%s: cue 3 is not closed where the region starts:\n%s", name, vtt.String())
		}
	}
}
