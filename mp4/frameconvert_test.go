package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// The FrameConverter seam is exercised with two stubs. An identity converter -
// one that claims the audio track but returns every frame and its codec
// unchanged - must leave the packaged output byte-for-byte what the nil default
// produces, proving the seam itself adds nothing. A codec-swapping stub - one
// that relabels the AAC track as FLAC and marks every frame - must make the
// init segment advertise fLaC/dfLa and carry the marked bytes, proving the seam
// actually reaches both the codec description and the media payload.

// identityConverter claims audio tracks and passes them through untouched.
type identityConverter struct{}

func (identityConverter) NewTrackConverter(t mkv.Track) (TrackConverter, error) {
	if t.Type != mkv.AudioTrack {
		return nil, nil
	}
	return identityTrack{codec: t.Codec, priv: t.CodecPrivate}, nil
}

type identityTrack struct {
	codec string
	priv  []byte
}

func (t identityTrack) Convert(frame []byte) ([]byte, error) { return frame, nil }
func (t identityTrack) OutputCodec() (string, []byte)        { return t.codec, t.priv }

// toFLACConverter relabels an AAC audio track as FLAC and prepends a marker to
// every frame, so the converted bytes are identifiable in a media segment.
type toFLACConverter struct{}

func (toFLACConverter) NewTrackConverter(t mkv.Track) (TrackConverter, error) {
	if t.Type != mkv.AudioTrack {
		return nil, nil
	}
	return &toFLACTrack{}, nil
}

var frameMarker = []byte{0xF1, 0xAC}

type toFLACTrack struct{ n int }

func (t *toFLACTrack) Convert(frame []byte) ([]byte, error) {
	t.n++
	return append(append([]byte{}, frameMarker...), frame...), nil
}

func (t *toFLACTrack) OutputCodec() (string, []byte) {
	// "fLaC" marker + a STREAMINFO metadata block (last-block flag, type 0,
	// length 34, then a 34-byte body). flacEntry strips the marker and wraps
	// the block in a dfLa box; the body's content is not inspected.
	priv := append([]byte("fLaC"), 0x80, 0x00, 0x00, 0x22)
	priv = append(priv, make([]byte, 34)...)
	return "flac", priv
}

func TestFrameConverterIdentityIsByteIdentical(t *testing.T) {
	src := chapterFixture(t, nil)
	ctx := context.Background()

	dirNil := t.TempDir()
	if err := RemuxToHLS(ctx, src, dirNil, Options{SegmentMs: 2000}); err != nil {
		t.Fatalf("nil converter: %v", err)
	}
	dirID := t.TempDir()
	if err := RemuxToHLS(ctx, src, dirID, Options{SegmentMs: 2000, FrameConverter: identityConverter{}}); err != nil {
		t.Fatalf("identity converter: %v", err)
	}
	assertDirsByteIdentical(t, dirNil, dirID)
}

func TestFrameConverterSwapsCodecAndPayload(t *testing.T) {
	src := chapterFixture(t, nil)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000, FrameConverter: toFLACConverter{}}); err != nil {
		t.Fatalf("converting remux: %v", err)
	}

	// The audio init segment must now describe FLAC, not AAC.
	audioInit, err := os.ReadFile(filepath.Join(dir, "init_a1.mp4"))
	if err != nil {
		t.Fatalf("read audio init: %v", err)
	}
	if !bytes.Contains(audioInit, []byte("fLaC")) || !bytes.Contains(audioInit, []byte("dfLa")) {
		t.Errorf("audio init does not advertise FLAC (fLaC/dfLa boxes absent)")
	}
	if bytes.Contains(audioInit, []byte("mp4a")) || bytes.Contains(audioInit, []byte("esds")) {
		t.Errorf("audio init still carries the AAC sample entry (mp4a/esds)")
	}

	// At least one audio media segment must carry the converter's marker, so we
	// know the frame bytes actually went through Convert (not just the header).
	segs, _ := filepath.Glob(filepath.Join(dir, "seg_a1_*.m4s"))
	if len(segs) == 0 { // fall back to any segment naming
		segs, _ = filepath.Glob(filepath.Join(dir, "*a1*.m4s"))
	}
	if len(segs) == 0 {
		t.Fatal("no audio media segments produced")
	}
	marked := false
	for _, s := range segs {
		b, err := os.ReadFile(s)
		if err != nil {
			t.Fatalf("read segment %s: %v", s, err)
		}
		if bytes.Contains(b, frameMarker) {
			marked = true
			break
		}
	}
	if !marked {
		t.Errorf("no audio segment carries the converted-frame marker %x", frameMarker)
	}
}

// TestFrameConverterPlanMatchesFullPass: the on-demand plan and the full pass
// must produce the same converted audio - the plan/full-pass byte-identity the
// rest of the packager guarantees has to survive the seam. The audio init and
// the first audio segment are compared between RemuxToHLS output and the plan's
// served resources, with the codec-swapping converter on both.
func TestFrameConverterPlanMatchesFullPass(t *testing.T) {
	src := chapterFixture(t, nil)
	ctx := context.Background()
	opts := Options{SegmentMs: 2000, FrameConverter: toFLACConverter{}}

	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, opts); err != nil {
		t.Fatalf("full pass: %v", err)
	}
	plan, err := PlanHLS(ctx, src, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	for _, name := range []string{"init_a1.mp4", "seg_a1_00001.m4s"} {
		full, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("full-pass %s: %v", name, err)
		}
		served, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatalf("plan %s: %v", name, err)
		}
		if !bytes.Equal(full, served) {
			t.Errorf("%s: plan (%d bytes) differs from full pass (%d bytes)", name, len(served), len(full))
		}
	}

	// And the converter must actually have run on the plan path (marker present,
	// FLAC advertised) - not silently skipped into a passthrough that happens to
	// match a full pass that also skipped.
	init, _, err := plan.Resource(ctx, "init_a1.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(init, []byte("fLaC")) {
		t.Error("plan audio init does not advertise FLAC")
	}
	seg, _, err := plan.Resource(ctx, "seg_a1_00001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(seg, frameMarker) {
		t.Error("plan audio segment does not carry the converted-frame marker")
	}
}

// assertDirsByteIdentical fails unless a and b hold the same set of files with
// identical bytes.
func assertDirsByteIdentical(t *testing.T, a, b string) {
	t.Helper()
	fa := readDirFiles(t, a)
	fb := readDirFiles(t, b)
	if len(fa) != len(fb) {
		t.Fatalf("file counts differ: %d in %s, %d in %s", len(fa), a, len(fb), b)
	}
	for name, ba := range fa {
		bb, ok := fb[name]
		if !ok {
			t.Errorf("%s present in %s, absent in %s", name, a, b)
			continue
		}
		if !bytes.Equal(ba, bb) {
			t.Errorf("%s differs (%d vs %d bytes)", name, len(ba), len(bb))
		}
	}
}

func readDirFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}
