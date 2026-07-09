package mp4

// singlefile_mut_test.go — targeted kills for mutation-testing survivors in
// singlefile.go. Byte-range and sidx math is verified against exact numbers
// (never just "no error"), and the DASH/HLS-only conditionals are verified by
// choosing inputs that force each guard's actual boundary (present vs nil,
// zero vs positive) rather than merely "with vs without metadata".

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// singlefileBuildSource returns a small two-track (video+audio) fixture with
// video keyframes exactly on 1000ms boundaries, so Options{SegmentMs: 1000}
// cuts four clean, equal-length segments ([0,1000) .. [3000,4000)) - every
// duration/byte-range computed from it has an exact, predictable value. The
// audio SampleRate is chosen equal to the movie timescale (1000) so its sidx
// ticks are plain milliseconds too, just like the video's.
func singlefileBuildSource(t testing.TB) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 1000.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 100; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 200; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}
	return buildMKV(t, tracks, gblocks)
}

// singlefileSidxDurations locates the sidx box in a single-file rendition
// (immediately follows the init segment: ftyp+moov, no gap) and returns each
// reference's subsegment_duration field. The box layout (buildSidx): 8-byte
// box header, 4-byte version+flags, then trackID/timescale/earliest/
// first_offset (16 bytes) and reserved+count (4 bytes) = 32 bytes before the
// first 12-byte reference (size, duration, SAP info).
func singlefileSidxDurations(t testing.TB, data []byte, count int) []uint32 {
	t.Helper()
	idx := bytes.Index(data, []byte("sidx"))
	if idx < 4 {
		t.Fatalf("sidx box not found")
	}
	base := int64(idx-4) + 32
	out := make([]uint32, count)
	for i := 0; i < count; i++ {
		off := base + int64(i)*12 + 4 // skip the reference's 4-byte size field
		out[i] = binary.BigEndian.Uint32(data[off : off+4])
	}
	return out
}

// singlefileSidxBoxSize returns the sidx box's own declared size (its first
// 4 bytes), independent of anything singlefile.go computes.
func singlefileSidxBoxSize(t testing.TB, data []byte) int64 {
	t.Helper()
	idx := bytes.Index(data, []byte("sidx"))
	if idx < 4 {
		t.Fatalf("sidx box not found")
	}
	return int64(binary.BigEndian.Uint32(data[idx-4 : idx]))
}

// TestSingleFileSidxDurationsExact pins singlefile.go:102 (segDur[i] =
// scale(endMs) - scale(segStart)): with four clean 1000ms segments, every
// sidx subsegment_duration must be exactly 1000, in every segment - not just
// the first (where segStart == 0 happens to make endMs+segStart == endMs-
// segStart, hiding an ARITHMETIC_BASE flip). ARITHMETIC_BASE (- -> +) or
// INVERT_NEGATIVES on this line changes segments 2-4 to 3000/5000/7000.
func TestSingleFileSidxDurationsExact(t *testing.T) {
	src := singlefileBuildSource(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, SingleFile: true}); err != nil {
		t.Fatal(err)
	}
	stream, err := os.ReadFile(filepath.Join(dir, "stream.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	got := singlefileSidxDurations(t, stream, 4)
	want := []uint32{1000, 1000, 1000, 1000}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment %d sidx subsegment_duration = %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestSingleFileSidxEndByteRange pins singlefile.go:126 (r.sidxEnd =
// r.initLen + len(sidx)): the EXT-X-MAP BYTERANGE length must equal the
// init segment length plus the sidx box's own declared size, computed here
// independently of anything in singlefile.go. ARITHMETIC_BASE (+ -> -) would
// shrink sidxEnd by 2x the sidx size instead of growing it.
func TestSingleFileSidxEndByteRange(t *testing.T) {
	src := singlefileBuildSource(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, SingleFile: true}); err != nil {
		t.Fatal(err)
	}
	stream, err := os.ReadFile(filepath.Join(dir, "stream.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	initIdx := bytes.Index(stream, []byte("sidx"))
	if initIdx < 4 {
		t.Fatal("sidx box not found")
	}
	initLen := int64(initIdx - 4)
	sidxSize := singlefileSidxBoxSize(t, stream)
	wantSidxEnd := initLen + sidxSize

	pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`BYTERANGE="(\d+)@0"`).FindSubmatch(pl)
	if m == nil {
		t.Fatalf("EXT-X-MAP BYTERANGE not found:\n%s", pl)
	}
	gotSidxEnd, _ := strconv.ParseInt(string(m[1]), 10, 64)
	if gotSidxEnd != wantSidxEnd {
		t.Errorf("EXT-X-MAP BYTERANGE length = %d, want initLen(%d)+sidxSize(%d) = %d",
			gotSidxEnd, initLen, sidxSize, wantSidxEnd)
	}
}

// TestSingleFileCrossModeParity pins singlefile.go:103 (segBytes), :105
// (durSec) and :119 (sizes[k]) by comparing the single-file rendition against
// the segmented rendition built from the same source and options: both paths
// share the same segStart/endMs/head+dataLen arithmetic (writeSegments uses
// the identical formulas inline), so any divergence in duration, per-segment
// byte-range length, or DASH bandwidth pinpoints a broken computation on the
// single-file side.
func TestSingleFileCrossModeParity(t *testing.T) {
	src := singlefileBuildSource(t)
	segDir, sfDir := t.TempDir(), t.TempDir()
	if err := RemuxToHLS(context.Background(), src, segDir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(context.Background(), src, sfDir, Options{SegmentMs: 1000, SingleFile: true}); err != nil {
		t.Fatal(err)
	}

	segPl, err := os.ReadFile(filepath.Join(segDir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	sfPl, err := os.ReadFile(filepath.Join(sfDir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}

	extinfRe := regexp.MustCompile(`#EXTINF:([0-9.]+),`)
	segDurs := extinfRe.FindAllSubmatch(segPl, -1)
	sfDurs := extinfRe.FindAllSubmatch(sfPl, -1)
	if len(segDurs) != len(sfDurs) {
		t.Fatalf("segment count differs: segmented=%d single-file=%d", len(segDurs), len(sfDurs))
	}
	if len(segDurs) == 0 {
		t.Fatal("no EXTINF entries found")
	}
	for i := range segDurs {
		if string(segDurs[i][1]) != string(sfDurs[i][1]) {
			t.Errorf("segment %d EXTINF duration: segmented=%s single-file=%s", i, segDurs[i][1], sfDurs[i][1])
		}
	}

	// Per-segment byte-range length must equal the actual fragment file size.
	brRe := regexp.MustCompile(`#EXT-X-BYTERANGE:(\d+)@(\d+)`)
	sfRanges := brRe.FindAllSubmatch(sfPl, -1)
	if len(sfRanges) != len(segDurs) {
		t.Fatalf("byte-range entries=%d, want %d", len(sfRanges), len(segDurs))
	}
	for i := range sfRanges {
		wantLen := int64(0)
		fi, err := os.Stat(filepath.Join(segDir, fmt.Sprintf("seg%05d.m4s", i+1)))
		if err != nil {
			t.Fatal(err)
		}
		wantLen = fi.Size()
		gotLen, _ := strconv.ParseInt(string(sfRanges[i][1]), 10, 64)
		if gotLen != wantLen {
			t.Errorf("segment %d byte-range length = %d, want fragment file size %d", i, gotLen, wantLen)
		}
	}

	// DASH bandwidth (peakBandwidth over segBytes/durSec) must match too.
	segMpd, err := os.ReadFile(filepath.Join(segDir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	sfMpd, err := os.ReadFile(filepath.Join(sfDir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	bwRe := regexp.MustCompile(`id="v" bandwidth="(\d+)"`)
	segBW := bwRe.FindSubmatch(segMpd)
	sfBW := bwRe.FindSubmatch(sfMpd)
	if segBW == nil || sfBW == nil {
		t.Fatalf("bandwidth attribute missing (seg found=%v, sf found=%v)", segBW != nil, sfBW != nil)
	}
	if string(segBW[1]) != string(sfBW[1]) {
		t.Errorf("video bandwidth: segmented=%s single-file=%s", segBW[1], sfBW[1])
	}
}

// singlefileFailClose wraps a WriteSeekCloser so Close always returns an
// error after actually closing the underlying writer.
type singlefileFailClose struct {
	mkv.WriteSeekCloser
}

func (w *singlefileFailClose) Close() error {
	_ = w.WriteSeekCloser.Close()
	return errors.New("singlefile_mut: injected close failure")
}

// TestSingleFileCloseErrorPropagates pins singlefile.go:149 (if cerr :=
// out.Close(); werr == nil { werr = cerr }): the video rendition's file
// Close fails while nothing else went wrong (werr is nil up to that point),
// so the close error must become the returned error. CONDITIONALS_NEGATION
// (== -> !=) would only assign werr on a *pre-existing* error, so a lone
// Close failure would be silently dropped and RemuxToHLS would report
// success.
func TestSingleFileCloseErrorPropagates(t *testing.T) {
	src := singlefileBuildSource(t)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	mem := mkv.NewMemFS()
	mem.Put("in.mkv", raw)
	real := mem.FS()
	fsys := &mkv.FS{
		Open: real.Open,
		Create: func(p string) (mkv.WriteSeekCloser, error) {
			w, cerr := real.Create(p)
			if cerr != nil {
				return nil, cerr
			}
			if p == "hls/stream.mp4" {
				return &singlefileFailClose{w}, nil
			}
			return w, nil
		},
		OpenFile:  real.OpenFile,
		Stat:      real.Stat,
		MkdirAll:  real.MkdirAll,
		WriteFile: real.WriteFile,
		Remove:    real.Remove,
		Rename:    real.Rename,
	}

	err = RemuxToHLS(context.Background(), "in.mkv", "hls", Options{SegmentMs: 1000, SingleFile: true, FS: fsys})
	if err == nil {
		t.Fatal("expected the injected close failure on stream.mp4 to propagate as an error")
	}
}

// TestSingleFileByteRangePlaylistExact pins singlefile.go:165 (if d > max)
// and :171 (int64(max+0.999)) by calling buildByteRangePlaylist directly and
// comparing the whole output byte-for-byte. All three durations are
// positive and distinct, so CONDITIONALS_NEGATION on "d > max" (which would
// leave max at its zero value throughout, since no d is <= 0) and
// ARITHMETIC_BASE on "+0.999" (which would floor instead of ceiling) both
// change the rendered TARGETDURATION.
func TestSingleFileByteRangePlaylistExact(t *testing.T) {
	o := &Options{}
	maxDur := 3.2
	durs := []float64{1.0, maxDur, 2.7}
	rend := &sfRendition{
		name:    "stream.mp4",
		sidxEnd: 5000,
		ranges:  [][2]int64{{5000, 111}, {5111, 222}, {5333, 333}},
	}
	got := string(buildByteRangePlaylist(o, durs, rend))

	want := "#EXTM3U\n#EXT-X-VERSION:7\n" +
		fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int64(maxDur+0.999)) +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		`#EXT-X-MAP:URI="stream.mp4",BYTERANGE="5000@0"` + "\n" +
		"#EXTINF:1.000,\n#EXT-X-BYTERANGE:111@5000\nstream.mp4\n" +
		"#EXTINF:3.200,\n#EXT-X-BYTERANGE:222@5111\nstream.mp4\n" +
		"#EXTINF:2.700,\n#EXT-X-BYTERANGE:333@5333\nstream.mp4\n" +
		"#EXT-X-ENDLIST\n"

	if got != want {
		t.Errorf("buildByteRangePlaylist mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestSingleFileDASHManifestDuration pins singlefile.go:190 (if d > maxDur):
// with three distinct positive durations, minBufferTime must reflect the
// true maximum; CONDITIONALS_NEGATION would leave maxDur at 0 (no duration
// is <= 0), rendering "PT0.000S" instead.
func TestSingleFileDASHManifestDuration(t *testing.T) {
	o := &Options{}
	video := &outTrack{spec: codecSpec{video: true}, mkv: mkv.Track{}}
	fts := []*fragTrack{{outTrack: video}}
	rends := []sfRendition{{name: "stream.mp4"}}
	mpd := string(buildDASHManifestSingle(o, fts, nil, []float64{1.5, 3.0, 2.0}, 0, rends))

	mustContain(t, mpd, `mediaPresentationDuration="PT6.500S"`)
	mustContain(t, mpd, `minBufferTime="PT3.000S"`)
}

// TestSingleFileDASHManifestSegmentBase pins singlefile.go:229 (indexRange
// end r.sidxEnd-1 and Initialization range end r.initLen-1): ARITHMETIC_BASE
// or INVERT_NEGATIVES on either "-1" shifts the printed byte offsets by 2.
func TestSingleFileDASHManifestSegmentBase(t *testing.T) {
	o := &Options{}
	video := &outTrack{spec: codecSpec{video: true}, mkv: mkv.Track{}}
	fts := []*fragTrack{{outTrack: video}}
	rends := []sfRendition{{name: "stream.mp4", initLen: 500, sidxEnd: 900}}
	mpd := string(buildDASHManifestSingle(o, fts, nil, []float64{1.0}, 0, rends))

	mustContain(t, mpd, `indexRange="500-899"`)
	mustContain(t, mpd, `Initialization range="0-499"`)
}

// TestSingleFileDASHVideoAttrs pins singlefile.go:205 (the Width/Height
// nil-and-positive guard) and :208 (FrameRate positive guard). Each nil-check
// mutation (205:15, 205:34) is exercised with the OTHER field present and
// positive, so a negated check does not short-circuit before dereferencing
// the still-nil field (a nil pointer deref then panics, failing the test).
// Each positive-value mutation (205:53, 205:70, 208:42) is exercised with a
// present-but-zero pointer, where only the strict ">" leaves the attribute
// out.
func TestSingleFileDASHVideoAttrs(t *testing.T) {
	o := &Options{}
	rends := []sfRendition{{name: "stream.mp4"}}
	run := func(t *testing.T, v mkv.Track, wantWidth, wantHeight, wantFrameRate bool) {
		fts := []*fragTrack{{outTrack: &outTrack{spec: codecSpec{video: true}, mkv: v}}}
		mpd := string(buildDASHManifestSingle(o, fts, nil, []float64{1.0}, 0, rends))
		if strings.Contains(mpd, ` width="`) != wantWidth {
			t.Errorf("width attribute present=%v, want %v:\n%s", strings.Contains(mpd, ` width="`), wantWidth, mpd)
		}
		if strings.Contains(mpd, ` height="`) != wantHeight {
			t.Errorf("height attribute present=%v, want %v:\n%s", strings.Contains(mpd, ` height="`), wantHeight, mpd)
		}
		if strings.Contains(mpd, `frameRate=`) != wantFrameRate {
			t.Errorf("frameRate attribute present=%v, want %v:\n%s", strings.Contains(mpd, `frameRate=`), wantFrameRate, mpd)
		}
	}

	t.Run("all-present", func(t *testing.T) {
		run(t, mkv.Track{Width: u32(1280), Height: u32(720), FrameRate: f64p(30)}, true, true, true)
	})
	t.Run("width-nil-height-present", func(t *testing.T) {
		run(t, mkv.Track{Width: nil, Height: u32(720)}, false, false, false)
	})
	t.Run("width-present-height-nil", func(t *testing.T) {
		run(t, mkv.Track{Width: u32(1280), Height: nil}, false, false, false)
	})
	t.Run("width-zero-height-present", func(t *testing.T) {
		run(t, mkv.Track{Width: u32(0), Height: u32(720)}, false, false, false)
	})
	t.Run("width-present-height-zero", func(t *testing.T) {
		run(t, mkv.Track{Width: u32(1280), Height: u32(0)}, false, false, false)
	})
	t.Run("framerate-zero", func(t *testing.T) {
		run(t, mkv.Track{FrameRate: f64p(0)}, false, false, false)
	})
}

// TestSingleFileDASHAudioAttrs pins singlefile.go:213 (Language != ""),
// :217 (the SampleRate nil-and-positive guard) and :221 (codecs != "").
func TestSingleFileDASHAudioAttrs(t *testing.T) {
	o := &Options{}
	rends := []sfRendition{{name: "stream_a1.mp4"}}
	run := func(t *testing.T, a mkv.Track) string {
		fts := []*fragTrack{{outTrack: &outTrack{spec: codecSpec{video: false}, mkv: a}}}
		return string(buildDASHManifestSingle(o, fts, nil, []float64{1.0}, 0, rends))
	}

	t.Run("language-present", func(t *testing.T) {
		mustContain(t, run(t, mkv.Track{Language: "fra"}), ` lang="fra"`)
	})
	t.Run("language-absent", func(t *testing.T) {
		mustNotContain(t, run(t, mkv.Track{Language: ""}), ` lang=`)
	})
	t.Run("samplerate-present", func(t *testing.T) {
		mustContain(t, run(t, mkv.Track{SampleRate: f64p(48000)}), `audioSamplingRate="48000"`)
	})
	t.Run("samplerate-nil", func(t *testing.T) {
		mustNotContain(t, run(t, mkv.Track{SampleRate: nil}), `audioSamplingRate=`)
	})
	t.Run("samplerate-zero", func(t *testing.T) {
		mustNotContain(t, run(t, mkv.Track{SampleRate: f64p(0)}), `audioSamplingRate=`)
	})
	t.Run("codecs-present", func(t *testing.T) {
		mustContain(t, run(t, mkv.Track{Codec: "aac", CodecPrivate: fakeASC}), `codecs="mp4a.40.`)
	})
	t.Run("codecs-absent", func(t *testing.T) {
		mustNotContain(t, run(t, mkv.Track{Codec: ""}), `codecs=`)
	})
}
