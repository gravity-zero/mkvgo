package ops

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// writeDeclared writes a seekable two-track source (video codec as given plus
// an Opus audio track) under the given DocType: "webm" at docTypeVersion, or
// "matroska" for anything else. Five 1 s clusters, each cued.
func writeDeclared(t *testing.T, path, docType string, docTypeVersion uint64, videoCodec string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if docType == "webm" {
		err = mw.WriteStartWebM(docTypeVersion)
	} else {
		err = mw.WriteStart()
	}
	if err != nil {
		t.Fatal(err)
	}
	w, h := uint32(160), uint32(120)
	rate := 48000.0
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: videoCodec, Width: &w, Height: &h, Language: "und"},
		{ID: 2, Type: mkv.AudioTrack, Codec: "opus", SampleRate: &rate, Language: "und", CodecPrivate: []byte("OpusHead\x01\x02")},
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}
	if err := mw.WriteMetadata(c, tracks, 5000); err != nil {
		t.Fatal(err)
	}
	for ts := int64(0); ts < 5000; ts += 1000 {
		blocks := []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{1, 2, 3}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{4}},
		}
		if err := mw.WriteClusterWithCues(ts, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
}

func declaredOf(t *testing.T, path string) (string, uint64) {
	t.Helper()
	c, err := reader.OpenMeta(context.Background(), path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c.DocType, c.DocTypeVersion
}

func wantDeclared(t *testing.T, path, docType string, version uint64) {
	t.Helper()
	got, gotV := declaredOf(t, path)
	if got != docType || gotV != version {
		t.Errorf("%s declares %q/%d, want %q/%d", filepath.Base(path), got, gotV, docType, version)
	}
}

// TestRewritesKeepWebMDocType: every full rewrite of a WebM-declared source
// keeps the declaration, unless the operation adds a track outside the WebM
// profile - then the result is plain Matroska.
func TestRewritesKeepWebMDocType(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	webm := filepath.Join(dir, "src.webm")
	writeDeclared(t, webm, "webm", 2, "vp9")
	mkvSrc := filepath.Join(dir, "src.mkv")
	writeDeclared(t, mkvSrc, "matroska", 0, "h264")
	webmAV1 := filepath.Join(dir, "av1.webm")
	writeDeclared(t, webmAV1, "webm", 2, "av1")
	srt := filepath.Join(dir, "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ass := filepath.Join(dir, "s.ass")
	if err := os.WriteFile(ass, []byte("[Script Info]\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:00.00,0:00:01.00,Default,,0,0,0,,Hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := func(name string) string { return filepath.Join(dir, name) }

	t.Run("edit-metadata", func(t *testing.T) {
		if err := EditMetadata(ctx, webm, out("edit.webm"), func(c *mkv.Container) { c.Info.Title = "x" }); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("edit.webm"), "webm", 2)
	})
	t.Run("edit-metadata matroska stays matroska", func(t *testing.T) {
		if err := EditMetadata(ctx, mkvSrc, out("edit.mkv"), func(c *mkv.Container) { c.Info.Title = "x" }); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("edit.mkv"), "matroska", 4)
	})
	t.Run("remove-track", func(t *testing.T) {
		if err := RemoveTrack(ctx, webm, out("rm.webm"), []uint64{2}); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("rm.webm"), "webm", 2)
	})
	t.Run("add-track from webm keeps webm", func(t *testing.T) {
		if err := AddTrack(ctx, webm, out("add.webm"), mkv.TrackInput{SourcePath: webmAV1, TrackID: 2}); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("add.webm"), "webm", 2)
	})
	t.Run("add-track outside the profile turns matroska", func(t *testing.T) {
		if err := AddTrack(ctx, webm, out("add.mkv"), mkv.TrackInput{SourcePath: mkvSrc, TrackID: 1}); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("add.mkv"), "matroska", 4)
	})
	t.Run("add-track av1 raises the version", func(t *testing.T) {
		if err := AddTrack(ctx, webm, out("addav1.webm"), mkv.TrackInput{SourcePath: webmAV1, TrackID: 1}); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("addav1.webm"), "webm", 4)
	})
	t.Run("merge srt turns matroska", func(t *testing.T) {
		if err := MergeSubtitle(ctx, webm, srt, out("srt.mkv"), "eng", "Sub"); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("srt.mkv"), "matroska", 4)
	})
	t.Run("merge ass turns matroska", func(t *testing.T) {
		if err := MergeASS(ctx, webm, ass, out("ass.mkv"), "eng", "Sub"); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("ass.mkv"), "matroska", 4)
	})
	t.Run("split", func(t *testing.T) {
		parts, err := Split(ctx, mkv.SplitOptions{SourcePath: webm, OutputDir: out("parts"), EveryMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) < 2 {
			t.Fatalf("parts = %v, want at least 2", parts)
		}
		for _, p := range parts {
			wantDeclared(t, p, "webm", 2)
		}
	})
	t.Run("join webm+webm keeps webm", func(t *testing.T) {
		if err := Join(ctx, []string{webm, webm}, out("join.webm")); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("join.webm"), "webm", 2)
	})
	t.Run("join follows the first source's version", func(t *testing.T) {
		v4 := out("v4.webm")
		writeDeclared(t, v4, "webm", 4, "vp9")
		if err := Join(ctx, []string{webm, v4}, out("joinv4.webm")); err != nil {
			t.Fatal(err)
		}
		// The first source's declaration leads: version 2 (vp9 needs no more).
		wantDeclared(t, out("joinv4.webm"), "webm", 2)
	})
	t.Run("join across declarations turns matroska", func(t *testing.T) {
		vp9mkv := out("vp9.mkv")
		writeDeclared(t, vp9mkv, "matroska", 0, "vp9")
		if err := Join(ctx, []string{webm, vp9mkv}, out("joinmix.mkv")); err != nil {
			t.Fatal(err)
		}
		wantDeclared(t, out("joinmix.mkv"), "matroska", 4)
	})
}

// TestValidateWebMCodecOffProfile: a file declaring webm around a codec the
// profile excludes gets a warning naming the track; a conformant WebM and a
// Matroska file with the same codec do not.
func TestValidateWebMCodecOffProfile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cases := []struct {
		name, docType, codec string
		want                 bool
	}{
		{"webm with h264", "webm", "h264", true},
		{"webm with vp9", "webm", "vp9", false},
		{"matroska with h264", "matroska", "h264", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".mkv")
			writeDeclared(t, p, tc.docType, 2, tc.codec)
			issues, err := Validate(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			var hit []mkv.Issue
			for _, is := range issues {
				if is.Code == "webm-codec-off-profile" {
					hit = append(hit, is)
				}
			}
			if got := len(hit) > 0; got != tc.want {
				t.Fatalf("webm-codec-off-profile reported=%v, want %v (issues %v)", got, tc.want, issues)
			}
			if tc.want && (hit[0].Track != 1 || hit[0].Severity != mkv.SeverityWarning) {
				t.Errorf("issue = %+v, want track 1 warning", hit[0])
			}
		})
	}
}

// TestDiscardNPastInt32: a declared size past a 32-bit int is consumed to EOF
// and reported as such, never truncated into a wrong count.
func TestDiscardNPastInt32(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader(make([]byte, 100)))
	err := discardN(r, int64(math.MaxInt32)+10)
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	r = bufio.NewReader(bytes.NewReader(make([]byte, 100)))
	if err := discardN(r, 60); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(r); len(rest) != 40 {
		t.Fatalf("left %d bytes, want 40", len(rest))
	}
}
