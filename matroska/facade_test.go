package matroska

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// webmContainer builds an in-memory WebM-compatible container (AV1 + Opus, with
// the init data WebM requires).
func webmContainer() *Container {
	opusHead := append([]byte("OpusHead"), 1, 2, 0x38, 0x01, 0x80, 0xBB, 0x00, 0x00, 0x00, 0x00, 0x00)
	return &Container{
		Info: SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: []byte{0x81, 0x04, 0x0C, 0x00}},
			{ID: 2, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead},
		},
	}
}

func TestFacadeWebMHelpers(t *testing.T) {
	c := webmContainer()

	if err := ValidateWebM(c); err != nil {
		t.Fatalf("ValidateWebM(av1+opus): %v", err)
	}
	if !IsWebMCodec("av1") || !IsWebMCodec("V_VP9") || IsWebMCodec("h264") {
		t.Error("IsWebMCodec: wrong classification")
	}
	if v := WebMDocTypeVersion(c); v != 4 {
		t.Errorf("WebMDocTypeVersion = %d, want 4 (AV1 present)", v)
	}
	if n := WebMNonSubsetElements(c); len(n) != 0 {
		t.Errorf("WebMNonSubsetElements = %v, want empty", n)
	}

	var buf bytes.Buffer
	if err := WriteWebM(&buf, c); err != nil {
		t.Fatalf("WriteWebM: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("WriteWebM wrote nothing")
	}

	// A container with chapters reports them as a non-subset element.
	c.Chapters = []Chapter{{StartMs: 0, Title: "x"}}
	if n := WebMNonSubsetElements(c); len(n) == 0 {
		t.Error("WebMNonSubsetElements should list Chapters")
	}
}

func TestFacadeMetaAndReindex(t *testing.T) {
	ctx := context.Background()

	if _, err := OpenMeta(ctx, fixturePath); err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	if _, err := OpenMetaWithFS(ctx, fixturePath, nil); err != nil {
		t.Fatalf("OpenMetaWithFS: %v", err)
	}

	f, err := os.Open(fixturePath)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	defer f.Close()
	if _, err := ReadMeta(ctx, f, fixturePath); err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "reindexed.mkv")
	if err := Reindex(ctx, fixturePath, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
}

func TestFacadeRemuxToWebMRejectsNonWebM(t *testing.T) {
	// The fixture is H.264/AAC - outside the WebM subset - so the remux must
	// reject it (this also exercises the facade wrapper).
	dst := filepath.Join(t.TempDir(), "out.webm")
	if err := RemuxToWebM(context.Background(), fixturePath, dst); err == nil {
		t.Error("expected RemuxToWebM to reject non-WebM codecs")
	}
}
