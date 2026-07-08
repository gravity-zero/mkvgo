package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// buildConcatSource builds one synthetic MKV "episode": video (h264, 40ms
// frames, keyframe every 25th frame) + two audio renditions (eng/fre) + one
// text subtitle cue at subCueMs. durMs must be a multiple of 40 so the video's
// total presentation duration lands exactly on durMs (no rounding), which is
// what the cue-shift test relies on.
func buildConcatSource(t testing.TB, durMs int64, subLang, subCue string, subCueMs int64) string {
	t.Helper()
	if durMs%40 != 0 {
		t.Fatalf("durMs %d must be a multiple of 40", durMs)
	}
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	var gblocks []genBlock
	nv := int(durMs / 40)
	for i := 0; i < nv; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	na := int(durMs / 20)
	for i := 0; i < na; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
		gblocks = append(gblocks, genBlock{track: 3, pts: int64(i) * 20, key: true, data: []byte{0xBB, byte(i)}})
	}
	gblocks = append(gblocks, genBlock{track: 4, pts: subCueMs, key: true, data: []byte(subCue)})
	sortGenBlocks(gblocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"},
			{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
			{ID: 4, Type: mkv.SubtitleTrack, Codec: "srt", Language: subLang},
		},
		gblocks)
}

// buildConcatSourceNoSubs is buildConcatSource without the subtitle track,
// for a mismatched-layout fixture.
func buildConcatSourceNoSubs(t testing.TB, durMs int64) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	var gblocks []genBlock
	nv := int(durMs / 40)
	for i := 0; i < nv; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	na := int(durMs / 20)
	for i := 0; i < na; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
		gblocks = append(gblocks, genBlock{track: 3, pts: int64(i) * 20, key: true, data: []byte{0xBB, byte(i)}})
	}
	sortGenBlocks(gblocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"},
			{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
		},
		gblocks)
}

// buildConcatSourceAV1 is buildConcatSource's video track re-encoded as av1,
// for the codec-incompatibility test.
func buildConcatSourceAV1(t testing.TB, durMs int64) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	var gblocks []genBlock
	nv := int(durMs / 40)
	for i := 0; i < nv; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x12, byte(i)}})
	}
	na := int(durMs / 20)
	for i := 0; i < na; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
		gblocks = append(gblocks, genBlock{track: 3, pts: int64(i) * 20, key: true, data: []byte{0xBB, byte(i)}})
	}
	sortGenBlocks(gblocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: fakeAV1C, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"},
			{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
		},
		gblocks)
}

// walkFiles lists every regular file under dir, relative to dir, slash-joined.
func walkFiles(t testing.TB, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	return out
}

// TestRemuxConcatToHLS checks the full-pass structural output: a single
// master/playlist/audio1/audio2/sub1 spanning both parts, with
// EXT-X-DISCONTINUITY at the part boundary and no re-timestamped media.
func TestRemuxConcatToHLS(t *testing.T) {
	src0 := buildConcatSource(t, 4000, "eng", "part0 cue", 500)
	src1 := buildConcatSource(t, 2000, "eng", "part1 cue", 300)
	dir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}

	master, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#EXT-X-STREAM-INF:", "playlist.m3u8", "AUDIO=\"aud\"", "SUBTITLES=\"subs\"",
		`URI="audio1.m3u8"`, `URI="sub1.m3u8"`} {
		if !bytes.Contains(master, []byte(want)) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}
	if n := bytes.Count(master, []byte("#EXT-X-STREAM-INF:")); n != 1 {
		t.Errorf("concat master must declare exactly one variant, got %d", n)
	}

	pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pl, []byte("#EXT-X-VERSION:6")) {
		t.Errorf("concatenated media playlist must be VERSION 6:\n%s", pl)
	}
	if n := bytes.Count(pl, []byte("#EXT-X-DISCONTINUITY")); n != 1 {
		t.Errorf("two parts must produce exactly 1 DISCONTINUITY, got %d:\n%s", n, pl)
	}
	if n := bytes.Count(pl, []byte("#EXT-X-MAP:URI=\"p0/")); n != 1 {
		t.Errorf("expected one p0/ MAP:\n%s", pl)
	}
	if n := bytes.Count(pl, []byte("#EXT-X-MAP:URI=\"p1/")); n != 1 {
		t.Errorf("expected one p1/ MAP:\n%s", pl)
	}
	if !bytes.Contains(pl, []byte("#EXT-X-ENDLIST")) {
		t.Error("missing ENDLIST")
	}
	// The MAP for part 1 must come right after the DISCONTINUITY.
	di := bytes.Index(pl, []byte("#EXT-X-DISCONTINUITY"))
	mi := bytes.Index(pl, []byte("#EXT-X-MAP:URI=\"p1/"))
	if di < 0 || mi < di {
		t.Errorf("part 1's MAP must follow its DISCONTINUITY:\n%s", pl)
	}

	// The video/audio segments must be exactly each part's own packaging: no
	// mdat rewriting happens for concat, only the playlists change.
	if _, err := os.Stat(filepath.Join(dir, "p0", "init.mp4")); err != nil {
		t.Error("p0/init.mp4 missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "p1", "init.mp4")); err != nil {
		t.Error("p1/init.mp4 missing")
	}

	// Subtitles: the whole-presentation VTT carries both cues, part 1's
	// shifted by exactly part 0's duration (4000 ms).
	whole, err := os.ReadFile(filepath.Join(dir, "sub1.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(whole, []byte("part0 cue")) || !bytes.Contains(whole, []byte("part1 cue")) {
		t.Errorf("sub1.vtt missing a cue:\n%s", whole)
	}
	wantShifted := subtitle.FormatVTTTime(4000 + 300)
	if !bytes.Contains(whole, []byte(wantShifted)) {
		t.Errorf("part 1's cue must be shifted by part 0's duration (want %s):\n%s", wantShifted, whole)
	}
	wantUnshifted := subtitle.FormatVTTTime(500)
	if !bytes.Contains(whole, []byte(wantUnshifted)) {
		t.Errorf("part 0's cue must be unshifted (want %s):\n%s", wantUnshifted, whole)
	}
}

// TestRemuxConcatToHLS_AudioPlaylistsAligned checks that both audio
// renditions concatenate at the same part boundary as the video playlist.
func TestRemuxConcatToHLS_AudioPlaylistsAligned(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSource(t, 1000, "eng", "b", 100)
	dir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, dir, Options{SegmentMs: 500}); err != nil {
		t.Fatal(err)
	}
	video, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	a1, err := os.ReadFile(filepath.Join(dir, "audio1.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := os.ReadFile(filepath.Join(dir, "audio2.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	vSegs := bytes.Count(video, []byte("#EXTINF:"))
	a1Segs := bytes.Count(a1, []byte("#EXTINF:"))
	a2Segs := bytes.Count(a2, []byte("#EXTINF:"))
	if a1Segs != vSegs || a2Segs != vSegs {
		t.Errorf("segment counts differ: video=%d audio1=%d audio2=%d", vSegs, a1Segs, a2Segs)
	}
	for _, b := range [][]byte{a1, a2} {
		if n := bytes.Count(b, []byte("#EXT-X-DISCONTINUITY")); n != 1 {
			t.Errorf("audio playlist must carry 1 DISCONTINUITY, got %d:\n%s", n, b)
		}
	}
}

// A mismatched subtitle layout across parts drops subtitles from the
// concatenated presentation (reported via OnDrop) instead of failing.
func TestRemuxConcatToHLS_MismatchedSubsDropped(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSourceNoSubs(t, 1000) // no subtitle track at all
	dir := t.TempDir()
	var dropped []DroppedTrack
	err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, dir, Options{SegmentMs: 500,
		OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) == 0 || !strings.Contains(dropped[len(dropped)-1].Reason, "subtitle") {
		t.Fatalf("expected an OnDrop reason mentioning subtitles, got %+v", dropped)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub1.m3u8")); !os.IsNotExist(err) {
		t.Error("sub1.m3u8 must not be written when layouts mismatch")
	}
	master, _ := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if bytes.Contains(master, []byte("SUBTITLES=")) {
		t.Errorf("master must not advertise subtitles when dropped:\n%s", master)
	}
}

// Incompatible video codecs across parts are refused before anything is
// written.
func TestRemuxConcatToHLS_IncompatibleCodecsRefused(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSourceAV1(t, 1000)
	dir := t.TempDir()
	err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, dir, Options{SegmentMs: 500})
	if err == nil {
		t.Fatal("expected an incompatibility error")
	}
	if !strings.Contains(err.Error(), "not compatible") {
		t.Errorf("error should explain the incompatibility: %v", err)
	}
	if files := walkFiles(t, dir); len(files) != 0 {
		t.Errorf("nothing should be written on refusal, found: %v", files)
	}
}

// Options this v1 slice does not support are refused explicitly.
func TestRemuxConcatToHLS_Refusals(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSource(t, 1000, "eng", "b", 100)
	dir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, dir, Options{Encrypt: &HLSEncryption{Key: make([]byte, 16)}}); err == nil {
		t.Error("Encrypt should be refused")
	}
	if err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, dir, Options{SingleFile: true}); err == nil {
		t.Error("SingleFile should be refused")
	}
	if err := RemuxConcatToHLS(context.Background(), []string{src0}, dir, Options{}); err == nil {
		t.Error("a single source should be rejected")
	}
}

// Segments must be byte-identical to each part's own standalone RemuxToHLS
// output: concat never re-timestamps media.
func TestRemuxConcatToHLS_SegmentsMatchStandalone(t *testing.T) {
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSource(t, 3000, "eng", "b", 100)

	concatDir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), []string{src0, src1}, concatDir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	standaloneDir0 := t.TempDir()
	if err := RemuxToHLS(context.Background(), src0, standaloneDir0, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	standaloneDir1 := t.TempDir()
	if err := RemuxToHLS(context.Background(), src1, standaloneDir1, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}

	compare := func(concatName, standaloneDir, standaloneName string) {
		got, err := os.ReadFile(filepath.Join(concatDir, concatName))
		if err != nil {
			t.Fatalf("read %s: %v", concatName, err)
		}
		want, err := os.ReadFile(filepath.Join(standaloneDir, standaloneName))
		if err != nil {
			t.Fatalf("read standalone %s: %v", standaloneName, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the standalone pass's %s (%d vs %d bytes)", concatName, standaloneName, len(got), len(want))
		}
	}
	compare("p0/init.mp4", standaloneDir0, "init.mp4")
	compare("p0/seg00001.m4s", standaloneDir0, "seg00001.m4s")
	compare("p0/init_a1.mp4", standaloneDir0, "init_a1.mp4")
	compare("p0/seg_a1_00001.m4s", standaloneDir0, "seg_a1_00001.m4s")
	compare("p1/init.mp4", standaloneDir1, "init.mp4")
	compare("p1/seg00001.m4s", standaloneDir1, "seg00001.m4s")
}
