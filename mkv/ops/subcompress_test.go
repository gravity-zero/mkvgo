package ops

import (
	"bytes"
	"compress/zlib"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// bzip2Hello is "hello bzip2\n" compressed by Python's bz2 module, so the
// test exercises a stream mkvgo did not produce (compress/bzip2 is decode-only).
var bzip2Hello = []byte{0x42, 0x5a, 0x68, 0x39, 0x31, 0x41, 0x59, 0x26, 0x53, 0x59, 0xab, 0x6b, 0xa1, 0xf1, 0x00, 0x00, 0x02, 0xd9, 0x80, 0x00, 0x10, 0x40, 0x00, 0x10, 0x00, 0x12, 0x64, 0xc0, 0x10, 0x20, 0x00, 0x31, 0x00, 0xd3, 0x4d, 0x04, 0x00, 0x1e, 0xa3, 0xef, 0x4e, 0x51, 0xa2, 0x07, 0x8b, 0xb9, 0x22, 0x9c, 0x28, 0x48, 0x55, 0xb5, 0xd0, 0xf8, 0x80}

func zlibBytes(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecompressSubtitleBlock(t *testing.T) {
	plain := []byte("Pas de raison que cette trahison")

	t.Run("no compression passes through untouched", func(t *testing.T) {
		got, err := decompressSubtitleBlock(&mkv.Track{ID: 1}, plain)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("got %q, want the payload unchanged", got)
		}
	})

	t.Run("zlib inflates", func(t *testing.T) {
		tr := &mkv.Track{ID: 1, Compression: mkv.CompressionZlib}
		got, err := decompressSubtitleBlock(tr, zlibBytes(t, plain))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("got %q, want %q", got, plain)
		}
	})

	t.Run("header stripping is left alone", func(t *testing.T) {
		tr := &mkv.Track{ID: 1, Compression: mkv.CompressionHeaderStrip, HeaderStripping: []byte{1, 2}}
		got, err := decompressSubtitleBlock(tr, plain)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("got %q, want the payload unchanged", got)
		}
	})

	t.Run("bzlib inflates", func(t *testing.T) {
		// compress/bzip2 is decompress-only, so the fixture is a stream captured
		// from a known-good compressor rather than one built here.
		tr := &mkv.Track{ID: 1, Compression: mkv.CompressionBzlib}
		got, err := decompressSubtitleBlock(tr, bzip2Hello)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "hello bzip2\n" {
			t.Errorf("got %q, want %q", got, "hello bzip2\n")
		}
	})

	t.Run("a track that lies about bzlib is refused", func(t *testing.T) {
		tr := &mkv.Track{ID: 1, Compression: mkv.CompressionBzlib}
		if _, err := decompressSubtitleBlock(tr, plain); err == nil {
			t.Error("a non-bzip2 payload was accepted")
		}
	})

	t.Run("a scheme with no decoder says which", func(t *testing.T) {
		for _, c := range []mkv.Compression{mkv.CompressionLZO1X} {
			tr := &mkv.Track{ID: 4, Compression: c}
			_, err := decompressSubtitleBlock(tr, plain)
			if err == nil || !strings.Contains(err.Error(), c.String()) {
				t.Errorf("%v: got %v, want an error naming the scheme", c, err)
			}
		}
	})

	t.Run("a track that lies about zlib is refused", func(t *testing.T) {
		tr := &mkv.Track{ID: 1, Compression: mkv.CompressionZlib}
		_, err := decompressSubtitleBlock(tr, plain) // not a zlib stream
		if err == nil || !strings.Contains(err.Error(), "does not start a zlib stream") {
			t.Errorf("got %v, want a refusal naming the problem", err)
		}
	})

	// A compressed stream can claim any expansion ratio; mkvgo has a memory
	// budget, so the inflated size is capped rather than trusted.
	t.Run("a decompression bomb is bounded", func(t *testing.T) {
		tr := &mkv.Track{ID: 1, Compression: mkv.CompressionZlib}
		bomb := zlibBytes(t, make([]byte, maxDecompressedBlock+1))
		if len(bomb) > 1<<16 {
			t.Fatalf("the bomb fixture is %d bytes, it should compress far smaller", len(bomb))
		}
		_, err := decompressSubtitleBlock(tr, bomb)
		if err == nil || !strings.Contains(err.Error(), "inflates past") {
			t.Errorf("got %v, want the size ceiling to refuse it", err)
		}
	})
}

// buildCompressedSubMKV writes a source whose subtitle track declares zlib and
// whose block payloads really are deflated - the shape mkvmerge produces, and
// the shape that made a whole PGS track come back as raw zlib.
func buildCompressedSubMKV(t *testing.T, dir, name string, tr mkv.Track, payloads [][]byte) string {
	t.Helper()
	blocks := make([]mkv.Block, len(payloads))
	for i, p := range payloads {
		blocks[i] = mkv.Block{TrackNumber: tr.ID, Timecode: int64(i * 1000), Duration: 900, Data: zlibBytes(t, p)}
	}
	return buildMinimalMKV(t, dir, name, []mkv.Track{tr}, blocks, int64(len(payloads)*1000))
}

// The declaration must survive the write/read round trip, or the next reader of
// the file has no way to know the blocks are compressed.
func TestCompressedTrack_DeclarationRoundTrips(t *testing.T) {
	dir := t.TempDir()
	tr := mkv.Track{ID: 1, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Compression: mkv.CompressionZlib}
	path := buildCompressedSubMKV(t, dir, "zlib.mkv", tr, [][]byte{[]byte("bonjour"), []byte("au revoir")})

	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tracks[0].Compression; got != mkv.CompressionZlib {
		t.Fatalf("Compression = %v, want zlib - the declaration was lost in the round trip", got)
	}
}

// The text extractors used to emit the compressed bytes AS TEXT: no error, just
// zlib in the output file. That silent failure is what this guards.
func TestExtractSubtitle_ZlibCompressedTextTrack(t *testing.T) {
	dir := t.TempDir()
	tr := mkv.Track{ID: 1, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Compression: mkv.CompressionZlib}
	path := buildCompressedSubMKV(t, dir, "zlib.mkv", tr, [][]byte{[]byte("bonjour"), []byte("au revoir")})

	out := dir + "/out.srt"
	if err := ExtractSubtitle(context.Background(), path, 1, out); err != nil {
		t.Fatalf("ExtractSubtitle: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bonjour", "au revoir"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("SRT output does not contain %q:\n%s", want, got)
		}
	}
	if bytes.Contains(got, []byte{0x78}) && !bytes.Contains(got, []byte("bonjour")) {
		t.Error("the output still looks like raw zlib")
	}

	var vtt bytes.Buffer
	if err := ExtractSubtitleWebVTT(context.Background(), path, 1, &vtt); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	if !strings.Contains(vtt.String(), "bonjour") {
		t.Errorf("WebVTT output is not the decompressed text:\n%s", vtt.String())
	}

	// And the same through the index-served path, which reads blocks by seeking.
	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var served bytes.Buffer
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 1, ix, &served); err != nil {
		t.Fatalf("ExtractSubtitleWebVTTFrom: %v", err)
	}
	if served.String() != vtt.String() {
		t.Errorf("the index-served output differs from the walk:\n--- walk ---\n%s\n--- served ---\n%s", vtt.String(), served.String())
	}
}

// The end of the real-file story: a PGS track whose blocks are zlib-compressed,
// which is how the format is actually shipped on disc rips.
func TestExtractSubtitlePGS_ZlibCompressedTrack(t *testing.T) {
	dir := t.TempDir()
	tr := mkv.Track{ID: 1, Type: mkv.SubtitleTrack, Codec: "pgs", Language: "fre", Compression: mkv.CompressionZlib}
	path := buildCompressedSubMKV(t, dir, "pgszlib.mkv", tr,
		[][]byte{pgsShow(10, 100, 900, true), pgsClear()})

	cues, err := ExtractSubtitlePGS(context.Background(), path, 1)
	if err != nil {
		t.Fatalf("ExtractSubtitlePGS on a compressed track: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if cues[0].X != 100 || cues[0].Y != 900 || cues[0].ScreenW != 1920 {
		t.Errorf("cue = (%d,%d) on %dx%d, want (100,900) on 1920x1080",
			cues[0].X, cues[0].Y, cues[0].ScreenW, cues[0].ScreenH)
	}
	off := cues[0].Image.PixOffset(0, 0)
	if got := [4]uint8(cues[0].Image.Pix[off : off+4]); got != [4]uint8{255, 255, 255, 255} {
		t.Errorf("top-left pixel = %v, want opaque white", got)
	}
}
