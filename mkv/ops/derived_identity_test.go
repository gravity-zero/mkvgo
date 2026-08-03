package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// An op whose output holds different content than its source - a track removed
// or added, a container converted - writes its own segment identity: copying
// the source's left two different files claiming to be one segment, the same
// defect Split had when every part wore the source's SegmentUID. The identity
// is derived, so running the op twice writes the same bytes, and the hard links
// (PrevUID/NextUID) stay: the timeline did not move, so a link into it is still
// true - which is also what keeps a subtitled part joinable at the precise seam.
func TestDerivedOutputsGetTheirOwnIdentity(t *testing.T) {
	ctx := context.Background()
	uid := bytes.Repeat([]byte{0xA1}, 16)
	prev := bytes.Repeat([]byte{0xB2}, 16)
	next := bytes.Repeat([]byte{0xC3}, 16)

	srtBody := []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n\n")
	assBody := []byte("[Script Info]\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:00.00,0:00:01.00,Default,,0,0,0,,Hello\n")

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dir, src, dst string)
	}{
		{"RemoveTrack", func(t *testing.T, dir, src, dst string) {
			if err := RemoveTrack(ctx, src, dst, []uint64{2}); err != nil {
				t.Fatal(err)
			}
		}},
		{"AddTrack", func(t *testing.T, dir, src, dst string) {
			extra := buildMinimalMKV(t, dir, "extra.mkv", []mkv.Track{audioTrack(1)},
				[]mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("a")}}, 500)
			if err := AddTrack(ctx, src, dst, mkv.TrackInput{SourcePath: extra, TrackID: 1}); err != nil {
				t.Fatal(err)
			}
		}},
		{"MergeSubtitle", func(t *testing.T, dir, src, dst string) {
			srt := filepath.Join(dir, "sub.srt")
			if err := os.WriteFile(srt, srtBody, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := MergeSubtitle(ctx, src, srt, dst, "eng", "Sub"); err != nil {
				t.Fatal(err)
			}
		}},
		{"MergeASS", func(t *testing.T, dir, src, dst string) {
			ass := filepath.Join(dir, "sub.ass")
			if err := os.WriteFile(ass, assBody, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := MergeASS(ctx, src, ass, dst, "eng", "Sub"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := buildLinkedMKV(t, dir, "src.mkv",
				[]mkv.Track{videoTrack(1), audioTrack(2)},
				[]mkv.Block{
					{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
					{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a")},
				}, 500, uid, prev, next)

			dst := filepath.Join(dir, "out.mkv")
			tc.run(t, dir, src, dst)
			assertDerivedIdentity(t, dst, uid, prev, next)
		})
	}
}

// assertDerivedIdentity: the output has its OWN 16-byte SegmentUID, different
// from the source's, and the hard links are untouched.
func assertDerivedIdentity(t *testing.T, path string, srcUID, prev, next []byte) {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Info.SegmentUID) != 16 {
		t.Fatalf("SegmentUID is %d bytes, want 16", len(c.Info.SegmentUID))
	}
	if bytes.Equal(c.Info.SegmentUID, srcUID) {
		t.Error("output still wears the source's SegmentUID")
	}
	if !bytes.Equal(c.Info.PrevUID, prev) || !bytes.Equal(c.Info.NextUID, next) {
		t.Errorf("hard links changed: prev %x next %x", c.Info.PrevUID, c.Info.NextUID)
	}
}

// The WebM remux is the same rule in another container.
func TestRemuxToWebM_GetsItsOwnIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	uid := bytes.Repeat([]byte{0xA1}, 16)

	src := buildLinkedMKV(t, dir, "src.mkv",
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Language: "eng"}},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}},
		500, uid, nil, nil)

	dst := filepath.Join(dir, "out.webm")
	if err := RemuxToWebM(ctx, src, dst); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Info.SegmentUID) != 16 || bytes.Equal(c.Info.SegmentUID, uid) {
		t.Errorf("webm output SegmentUID = %x, want its own 16 bytes", c.Info.SegmentUID)
	}
}

// Determinism, proven rather than assumed: the same op run twice writes the
// same derived identity.
func TestDerivedIdentityIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	uid := bytes.Repeat([]byte{0xA1}, 16)
	src := buildLinkedMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1), audioTrack(2)},
		[]mkv.Block{
			{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte("a")},
		}, 500, uid, nil, nil)

	read := func(path string) []byte {
		c, err := reader.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return c.Info.SegmentUID
	}
	a, b := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv")
	if err := RemoveTrack(ctx, src, a, []uint64{2}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTrack(ctx, src, b, []uint64{2}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(a), read(b)) {
		t.Error("two runs of the same op derived different identities")
	}
	// ... and two DIFFERENT ops on the same source do not collide.
	srt := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nHi\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filepath.Join(dir, "m.mkv")
	if err := MergeSubtitle(ctx, src, srt, m, "eng", "Sub"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(read(a), read(m)) {
		t.Error("remove-track and merge-subtitle derived the same identity")
	}
}

// EditMetadata rewrites the same content: it IS the same segment, and its
// identity - hard links included - survives untouched. A retitled part must
// still join at the precise seam.
func TestEditMetadata_KeepsTheIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	uid := bytes.Repeat([]byte{0xA1}, 16)
	prev := bytes.Repeat([]byte{0xB2}, 16)
	next := bytes.Repeat([]byte{0xC3}, 16)
	src := buildLinkedMKV(t, dir, "src.mkv",
		[]mkv.Track{videoTrack(1)},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte("v")}},
		500, uid, prev, next)

	dst := filepath.Join(dir, "out.mkv")
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) {
		c.Info.Title = "renamed"
	}); err != nil {
		t.Fatal(err)
	}
	c, err := reader.Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Info.SegmentUID, uid) || !bytes.Equal(c.Info.PrevUID, prev) || !bytes.Equal(c.Info.NextUID, next) {
		t.Errorf("identity changed on a metadata-only rewrite: uid %x prev %x next %x",
			c.Info.SegmentUID, c.Info.PrevUID, c.Info.NextUID)
	}
}
