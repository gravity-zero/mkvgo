package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// audioFixture is a Segment with one audio TrackEntry of the given codec and Audio/ContentEncodings children.
func audioFixture(codec string, audio, extra []byte) []byte {
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, codec),
		audio, extra)
	var seg bytes.Buffer
	seg.Write(infoElem())
	seg.Write(masterElem(mkv.IDTracks, te))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

// readBoth parses the fixture with the seekable and the streaming reader.
func readBoth(t *testing.T, data []byte) [2]mkv.Track {
	t.Helper()
	c, err := Read(context.Background(), bytes.NewReader(data), "a.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cs, _, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	return [2]mkv.Track{c.Tracks[0], cs.Tracks[0]}
}

// TestContentEncryptionNamed: a ContentEncoding of type 1 names the track's
// encryption from ContentEncAlgo and the AES cipher mode, on both readers.
func TestContentEncryptionNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		enc  []byte
		want string
	}{
		{"aes-ctr", masterElem(mkv.IDContentEncoding,
			uintElem(mkv.IDContentEncodingType, 1, 1),
			masterElem(mkv.IDContentEncryption,
				uintElem(mkv.IDContentEncAlgo, 5, 1),
				masterElem(mkv.IDContentEncAESSettings, uintElem(mkv.IDAESSettingsCipherMode, 1, 1)))), "AES-CTR"},
		{"des", masterElem(mkv.IDContentEncoding,
			masterElem(mkv.IDContentEncryption, uintElem(mkv.IDContentEncAlgo, 1, 1))), "DES"},
		{"type only", masterElem(mkv.IDContentEncoding, uintElem(mkv.IDContentEncodingType, 1, 1)), "unspecified"},
		{"compression", masterElem(mkv.IDContentEncoding,
			masterElem(mkv.IDContentCompression, uintElem(mkv.IDContentCompAlgo, 0, 1))), ""},
	} {
		extra := masterElem(mkv.IDContentEncodings, tc.enc)
		for i, tr := range readBoth(t, audioFixture("A_OPUS", nil, extra)) {
			if tr.Encryption != tc.want {
				t.Errorf("%s reader %d: Encryption = %q, want %q", tc.name, i, tr.Encryption, tc.want)
			}
		}
	}
}
