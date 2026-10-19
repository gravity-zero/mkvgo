package reader

import (
	"bytes"
	"compress/zlib"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestContentEncodingScopeAndUnknownAlgo: a compression scoped to the CodecPrivate (scope 2) inflates the private
// and leaves the blocks plain; a ContentCompAlgo Matroska does not define reads as unknown, never as plain.
func TestContentEncodingScopeAndUnknownAlgo(t *testing.T) {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write([]byte("private header"))
	_ = zw.Close()
	encoding := func(scope, algo uint64) []byte {
		return masterElem(mkv.IDContentEncodings, masterElem(mkv.IDContentEncoding,
			uintElem(mkv.IDContentEncodingScope, scope, 1),
			uintElem(mkv.IDContentEncodingType, 0, 1),
			masterElem(mkv.IDContentCompression, uintElem(mkv.IDContentCompAlgo, algo, 1))))
	}
	track := func(n uint64, private []byte, enc []byte) []byte {
		return masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, n, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeSubtitle, 1),
			strElem(mkv.IDCodecID, "S_TEXT/ASS"),
			bytesElem(mkv.IDCodecPrivate, private),
			enc)
	}
	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks, track(1, z.Bytes(), encoding(2, 0)), track(2, []byte("plain"), encoding(1, 9))))
	c, err := readMKV(wrapSegment(seg.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tracks[0]; string(got.CodecPrivate) != "private header" || got.Compression != mkv.CompressionNone {
		t.Errorf("scope 2: CodecPrivate %q, blocks %v; want the private inflated and the blocks plain", got.CodecPrivate, got.Compression)
	}
	if got := c.Tracks[1].Compression; got != mkv.CompressionUnknown {
		t.Errorf("ContentCompAlgo 9 read as %v, want unknown", got)
	}
	if _, err := c.Tracks[1].DecodePayload([]byte("x")); err == nil {
		t.Error("a block under an unknown algorithm decoded")
	}
}
