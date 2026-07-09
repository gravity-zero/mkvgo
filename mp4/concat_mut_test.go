package mp4

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// concat_mut_test.go targets concat.go survivors outside the master
// EXT-X-STREAM-INF assembly (already covered by concat_attrs_test.go): the
// per-part playlist assembly (buildConcatPlaylist/buildConcatSubPlaylist),
// subtitle windowing/shifting (shiftWindowVTT) and the audio/subtitle
// rendition NAME/LANGUAGE/DEFAULT metadata in buildConcatMaster.

// concatMutSource builds a minimal synthetic "episode" with one video track,
// the given audio tracks and the given subtitle tracks, for exercising the
// concat master's per-rendition attribute logic with controlled metadata.
func concatMutSource(t *testing.T, audios []mkv.Track, subs []mkv.Track) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}
	var blocks []genBlock
	for i := 0; i < 50; i++ {
		blocks = append(blocks, genBlock{track: video.ID, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for _, a := range audios {
		for i := 0; i < 100; i++ {
			blocks = append(blocks, genBlock{track: a.ID, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
		}
	}
	for _, s := range subs {
		blocks = append(blocks, genBlock{track: s.ID, pts: 0, key: true, data: []byte("cue text")})
	}
	sortGenBlocks(blocks)
	tracks := append([]mkv.Track{video}, audios...)
	tracks = append(tracks, subs...)
	return buildMKV(t, tracks, blocks)
}

// TestConcatMasterAudioRenditionNames kills concat.go:348,351,355,358 - the
// audio rendition NAME/LANGUAGE/DEFAULT fallbacks in buildConcatMaster.
func TestConcatMasterAudioRenditionNames(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	ch := uint8(2)

	t.Run("no-name-no-language", func(t *testing.T) {
		mk := func() string {
			a := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}
			return concatMutSource(t, []mkv.Track{a}, nil)
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 500}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, `NAME="Audio 1"`)
		mustNotContain(t, master, `LANGUAGE=`)
		// A single audio rendition with no explicit default must still be
		// selected default (the !hasDefaultAudio && j==0 fallback).
		mustContain(t, master, `DEFAULT=YES`)
	})

	t.Run("language-only-falls-back-to-name", func(t *testing.T) {
		mk := func() string {
			a := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "spa"}
			return concatMutSource(t, []mkv.Track{a}, nil)
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 500}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, `NAME="spa"`)
		mustContain(t, master, `LANGUAGE="spa"`)
	})

	t.Run("explicit-default-on-second-audio-only", func(t *testing.T) {
		mk := func() string {
			a1 := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"}
			a2 := mkv.Track{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre", IsDefault: true}
			return concatMutSource(t, []mkv.Track{a1, a2}, nil)
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 500}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		lines := strings.Split(master, "\n")
		defaults := 0
		defaultIsFre := false
		for _, l := range lines {
			if strings.HasPrefix(l, "#EXT-X-MEDIA:TYPE=AUDIO") && strings.Contains(l, "DEFAULT=YES") {
				defaults++
				if strings.Contains(l, `LANGUAGE="fre"`) {
					defaultIsFre = true
				}
			}
		}
		if defaults != 1 {
			t.Errorf("want exactly one DEFAULT=YES audio rendition, got %d:\n%s", defaults, master)
		}
		if !defaultIsFre {
			t.Errorf("the explicit IsDefault audio (fre) must be the one carrying DEFAULT=YES:\n%s", master)
		}
	})
}

// TestConcatMasterSubtitleRenditionNames kills concat.go:369,372,376 - the
// subtitle rendition NAME/LANGUAGE fallbacks in buildConcatMaster.
func TestConcatMasterSubtitleRenditionNames(t *testing.T) {
	ctx := context.Background()
	sr := 48000.0
	ch := uint8(2)
	audio := func() mkv.Track {
		return mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}
	}

	t.Run("no-name-no-language", func(t *testing.T) {
		mk := func() string {
			s := mkv.Track{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt"}
			return concatMutSource(t, []mkv.Track{audio()}, []mkv.Track{s})
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 500}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, `NAME="Subtitles 1"`)
		lines := strings.Split(master, "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "#EXT-X-MEDIA:TYPE=SUBTITLES") && strings.Contains(l, "LANGUAGE=") {
				t.Errorf("subtitle rendition must not carry LANGUAGE when the source has none: %s", l)
			}
		}
	})

	t.Run("language-only-falls-back-to-name", func(t *testing.T) {
		mk := func() string {
			s := mkv.Track{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "jpn"}
			return concatMutSource(t, []mkv.Track{audio()}, []mkv.Track{s})
		}
		dir := t.TempDir()
		if err := RemuxConcatToHLS(ctx, []string{mk(), mk()}, dir, Options{SegmentMs: 500}); err != nil {
			t.Fatal(err)
		}
		master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
		mustContain(t, master, `NAME="jpn"`)
		mustContain(t, master, `LANGUAGE="jpn"`)
	})
}

// TestShiftWindowVTTHalfOpenBoundary kills concat.go:234 (the c.EndMs >
// segStart half-open window boundary) and concat.go:240 (the WriteWebVTT
// error check - a NEGATION mutant there always returns nil data instead of
// the rendered VTT, even on a normal, error-free call).
func TestShiftWindowVTTHalfOpenBoundary(t *testing.T) {
	cues := []subtitle.Cue{
		{StartMs: 0, EndMs: 1000, Text: "ends-exactly-at-segStart"},    // EndMs == segStart -> excluded
		{StartMs: 1000, EndMs: 1001, Text: "ends-just-after-segStart"}, // EndMs > segStart -> included
		{StartMs: 1999, EndMs: 2500, Text: "starts-just-before-segEnd"},
	}
	data, err := shiftWindowVTT(cues, 1000, 2000, 0)
	if err != nil {
		t.Fatalf("shiftWindowVTT: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("shiftWindowVTT must return the rendered VTT bytes on success, got empty")
	}
	s := string(data)
	if strings.Contains(s, "ends-exactly-at-segStart") {
		t.Errorf("a cue ending exactly at segStart (half-open window) must be excluded:\n%s", s)
	}
	if !strings.Contains(s, "ends-just-after-segStart") {
		t.Errorf("a cue ending just after segStart must be included:\n%s", s)
	}
	if !strings.Contains(s, "starts-just-before-segEnd") {
		t.Errorf("a cue starting just before segEnd must be included:\n%s", s)
	}
}

// TestBuildConcatPlaylistTargetDurationAndDiscontinuity kills concat.go:256
// (the running max-duration comparison across every part/segment),
// concat.go:263 (the +0.999 ceil arithmetic) and concat.go:266 (the k>0
// EXT-X-DISCONTINUITY guard - a part 0 must never get one).
func TestBuildConcatPlaylistTargetDurationAndDiscontinuity(t *testing.T) {
	// The overall max duration (3.5s) sits in the middle part, not the first
	// or the largest-looking early value, so the running max must actually
	// track across every part and every segment within it.
	durs := [][]float64{
		{2.0},
		{3.5},
		{1.0},
	}
	mapURIs := []string{"", "", ""}
	segName := func(part, i int) string { return "seg.m4s" }
	b := buildConcatPlaylist(&Options{}, durs, mapURIs, segName)
	s := string(b)

	// int64(3.5+0.999) == 4; a "-" mutant would give int64(3.5-0.999) == 2.
	if !strings.Contains(s, "#EXT-X-TARGETDURATION:4\n") {
		t.Errorf("want TARGETDURATION:4 (ceil of the true max 3.5s), got:\n%s", s)
	}
	if n := strings.Count(s, "#EXT-X-DISCONTINUITY"); n != 2 {
		t.Errorf("3 parts must produce exactly 2 DISCONTINUITY markers (never before part 0), got %d:\n%s", n, s)
	}
	if strings.Index(s, "#EXT-X-DISCONTINUITY") < strings.Index(s, "#EXTINF:2.000") {
		t.Errorf("part 0's segment must not be preceded by a DISCONTINUITY:\n%s", s)
	}
}

// TestBuildConcatSubPlaylistSegmentNaming kills concat.go:325 - the j+1/i+1
// 1-based numbering in the concatenated subtitle segment filenames.
func TestBuildConcatSubPlaylistSegmentNaming(t *testing.T) {
	results := []*hlsResult{
		{durs: []float64{1.0}},
		{durs: []float64{1.0}},
	}
	// j=1 (0-based) is the *second* subtitle rendition -> "sub2"; i=0 is the
	// first segment -> "_00001".
	b := buildConcatSubPlaylist(&Options{}, results, 1)
	s := string(b)
	if !strings.Contains(s, "p0/sub2_00001.vtt") {
		t.Errorf("want p0/sub2_00001.vtt (j+1=2, i+1=1), got:\n%s", s)
	}
	if !strings.Contains(s, "p1/sub2_00001.vtt") {
		t.Errorf("want p1/sub2_00001.vtt, got:\n%s", s)
	}
}

// TestWriteConcatSubtitlesPropagatesWriteError kills concat.go:478 - a
// NEGATION mutant on `err != nil` would swallow a sub{j+1}.m3u8 write
// failure and return success anyway.
func TestWriteConcatSubtitlesPropagatesWriteError(t *testing.T) {
	sr := 48000.0
	ch := uint8(2)
	a := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}
	s := mkv.Track{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"}
	mk := func() string { return concatMutSource(t, []mkv.Track{a}, []mkv.Track{s}) }

	dir := t.TempDir()
	fs := &mkv.FS{WriteFile: func(path string, data []byte, perm os.FileMode) error {
		if strings.HasSuffix(path, "sub1.m3u8") {
			return errors.New("injected: sub1.m3u8 write failure")
		}
		return os.WriteFile(path, data, perm)
	}}
	err := RemuxConcatToHLS(context.Background(), []string{mk(), mk()}, dir, Options{SegmentMs: 500, FS: fs})
	if err == nil {
		t.Fatal("expected the injected sub1.m3u8 write failure to propagate as an error")
	}
}
