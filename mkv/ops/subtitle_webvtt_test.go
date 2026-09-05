package ops

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestExtractSubtitleWebVTT_SRT(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{subtitleTrack(1, "srt")}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 1000, Duration: 1000, Data: []byte("Hello")},
		{TrackNumber: 1, Timecode: 3000, Duration: 2000, Data: []byte("World")},
	}
	path := buildMinimalMKV(t, dir, "sub.mkv", tracks, blocks, 5000)

	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &b); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	got := b.String()
	for _, want := range []string{
		"WEBVTT",
		"00:00:01.000 --> 00:00:02.000\nHello",
		"00:00:03.000 --> 00:00:05.000\nWorld",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestExtractSubtitleWebVTT_ASS(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{subtitleTrack(1, "ass")}
	// S_TEXT/ASS block framing: ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 1000, Duration: 1000, Data: []byte(`0,0,Default,,0,0,0,,{\i1}Styled{\i0}`)},
	}
	path := buildMinimalMKV(t, dir, "ass.mkv", tracks, blocks, 3000)

	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &b); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	if !strings.Contains(b.String(), "00:00:01.000 --> 00:00:02.000\nStyled") {
		t.Errorf("ASS override tags not flattened:\n%s", b.String())
	}
}

func TestExtractSubtitleWebVTT_Errors(t *testing.T) {
	dir := t.TempDir()
	path := buildMinimalMKV(t, dir, "x.mkv",
		[]mkv.Track{subtitleTrack(1, "pgs")}, // bitmap subtitle (not text)
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Data: []byte{1, 2}}}, 1000)

	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &b); err == nil {
		t.Error("expected error for a non-text subtitle codec")
	}
	if err := ExtractSubtitleWebVTT(context.Background(), path, 99, &b); err == nil {
		t.Error("expected error for a missing track")
	}
}

// countingOpenFS counts, per opened handle, the bytes actually read from the
// source - the probe that tells a reader-side track filter apart from one
// applied after the payloads have already been read.
type countingOpenFS struct {
	reads []*countingHandle
}

type countingHandle struct {
	mkv.ReadSeekCloser
	n int64
}

func (h *countingHandle) Read(p []byte) (int, error) {
	n, err := h.ReadSeekCloser.Read(p)
	h.n += int64(n)
	return n, err
}

func (c *countingOpenFS) fs() *mkv.FS {
	return &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		h := &countingHandle{ReadSeekCloser: f}
		c.reads = append(c.reads, h)
		return h, nil
	}}
}

// buildHeavyTrackFixture writes a source whose video payloads are larger than
// the reader's read-ahead window - the regime where a reader-side track filter
// turns into real seeks - with a sparse subtitle track beside them.
func buildHeavyTrackFixture(t *testing.T, dir, name, codec string, cueText func(i int) string) string {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "und"},
		subtitleTrack(2, codec),
	}
	heavy := make([]byte, 512<<10)
	for i := range heavy {
		heavy[i] = byte(i)
	}
	var blocks []mkv.Block
	for i := 0; i < 16; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i * 40), Keyframe: true, Data: heavy})
		if i%8 == 0 {
			blocks = append(blocks, mkv.Block{
				TrackNumber: 2, Timecode: int64(i * 40), Duration: 500,
				Data: []byte(cueText(i)),
			})
		}
	}
	return buildMinimalMKV(t, dir, name, tracks, blocks, 2000)
}

// Every subtitle extractor must filter in the reader, not after the fact:
// dropping the KeepTracks call makes the walk read every byte of the other
// tracks' payloads. This does not pin the whole benefit - on a real source most
// payloads are smaller than the read window and are dropped from it for free,
// where the saving is the payload copy rather than the read.
func TestExtractSubtitle_DoesNotReadOtherTracks(t *testing.T) {
	cases := []struct {
		name    string
		codec   string
		cueText func(i int) string
		run     func(t *testing.T, dir, path string, o mkv.Options) (entries int)
	}{
		{
			name:    "WebVTT",
			codec:   "srt",
			cueText: func(i int) string { return "cue " + strconv.Itoa(i) },
			run: func(t *testing.T, dir, path string, o mkv.Options) int {
				var b strings.Builder
				if err := ExtractSubtitleWebVTT(context.Background(), path, 2, &b, o); err != nil {
					t.Fatalf("ExtractSubtitleWebVTT: %v", err)
				}
				return strings.Count(b.String(), " --> ")
			},
		},
		{
			name:    "SRT",
			codec:   "srt",
			cueText: func(i int) string { return "cue " + strconv.Itoa(i) },
			run: func(t *testing.T, dir, path string, o mkv.Options) int {
				out := filepath.Join(dir, "out.srt")
				if err := ExtractSubtitle(context.Background(), path, 2, out, o); err != nil {
					t.Fatalf("ExtractSubtitle: %v", err)
				}
				return strings.Count(readFile(t, out), " --> ")
			},
		},
		{
			name:    "ASS",
			codec:   "ass",
			cueText: func(i int) string { return "0,0,Default,,0,0,0,,line " + strconv.Itoa(i) },
			run: func(t *testing.T, dir, path string, o mkv.Options) int {
				out := filepath.Join(dir, "out.ass")
				if err := ExtractASS(context.Background(), path, 2, out, o); err != nil {
					t.Fatalf("ExtractASS: %v", err)
				}
				return strings.Count(readFile(t, out), "Dialogue: ")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := buildHeavyTrackFixture(t, dir, "heavy.mkv", tc.codec, tc.cueText)
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			cfs := &countingOpenFS{}
			if n := tc.run(t, dir, path, mkv.Options{FS: cfs.fs()}); n != 2 {
				t.Fatalf("got %d entries, want 2", n)
			}
			if len(cfs.reads) == 0 {
				t.Fatal("the injected FS was never used")
			}
			// The last handle is the block walk (the first is the metadata pass).
			walk := cfs.reads[len(cfs.reads)-1]
			if limit := st.Size() / 3; walk.n > limit {
				t.Errorf("block walk read %d of %d bytes (%.0f%%) - other tracks' payloads must be skipped, not read",
					walk.n, st.Size(), 100*float64(walk.n)/float64(st.Size()))
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
