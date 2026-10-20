package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// segmentChildren are elements a sound Segment may end with: none of them is junk to resync past or damage.
var segmentChildren = map[string][]byte{
	"a Void":                        {0xEC, 0x84, 0, 0, 0, 0},
	"an unknown element":            {0x1B, 0xAD, 0xBE, 0xEF, 0x82, 0, 0},
	"an unknown element and a Void": {0x1B, 0xAD, 0xBE, 0xEF, 0x82, 0, 0, 0xEC, 0x82, 0, 0},
	"two unknown elements":          {0x1B, 0xAD, 0xBE, 0xEF, 0x82, 0, 0, 0x1B, 0xAD, 0xBE, 0xEE, 0x81, 0},
	"empty Cues":                    {0x1C, 0x53, 0xBB, 0x6B, 0x80},
	"Cues with an empty CuePoint":   {0x1C, 0x53, 0xBB, 0x6B, 0x82, 0xBB, 0x80},
	"a CuePoint without a position": {0x1C, 0x53, 0xBB, 0x6B, 0x85, 0xBB, 0x83, 0xB3, 0x81, 0x00},
}

// withSegmentEnding appends tail inside the Segment that closes data, sealed or of unknown size.
func withSegmentEnding(t *testing.T, data, tail []byte, unknownSize bool) []byte {
	t.Helper()
	r := bytes.NewReader(data)
	h, _, err := ebml.ReadElementHeader(r)
	if err != nil || h.ID != ebml.IDEBMLHeader {
		t.Fatalf("EBML header: %v", err)
	}
	segAt := int64(len(data)) - int64(r.Len()) + h.Size
	if _, err := r.Seek(segAt, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	id, idLen, err := ebml.ReadElementID(r)
	if err != nil || id != mkv.IDSegment {
		t.Fatalf("Segment at %d: %x %v", segAt, id, err)
	}
	size, width, err := ebml.ReadDataSize(r)
	if err != nil || size < 0 || segAt+int64(idLen+width)+size != int64(len(data)) {
		t.Fatalf("the Segment does not end the file: size %d, %v", size, err)
	}
	var field bytes.Buffer
	if unknownSize {
		field.Write(bytes.Repeat([]byte{0xFF}, width))
		field.Bytes()[0] = byte(0xFF >> (width - 1))
	} else if _, err := ebml.WriteDataSizeWidth(&field, size+int64(len(tail)), width); err != nil {
		t.Fatal(err)
	}
	out := append([]byte{}, data...)
	copy(out[segAt+int64(idLen):], field.Bytes())
	return append(out, tail...)
}

// TestSoundSegmentEndingsShowNoDamage: elements a Segment may legally end with, sealed or of unknown size (a live
// recording), read as no damage by any check.
func TestSoundSegmentEndingsShowNoDamage(t *testing.T) {
	dir := t.TempDir()
	for _, src := range soundFiles(t, dir) {
		if c, err := reader.Open(context.Background(), src); err != nil || c.SegmentEnd != int64(len(readAll(t, src))) {
			continue
		}
		data := readAll(t, src)
		for _, unknown := range []bool{false, true} {
			endings := map[string][]byte{"nothing": nil}
			maps.Copy(endings, segmentChildren)
			for name, tail := range endings {
				if !unknown && tail == nil {
					continue
				}
				path := filepath.Join(dir, fmt.Sprintf("%s.%s.%t.mkv", filepath.Base(src), name, unknown))
				writeAll(t, path, withSegmentEnding(t, data, tail, unknown))
				v := viewDamage(t, path)
				if why := hasDamage(v); len(why) > 0 {
					t.Errorf("%s ending with %s (unknown size %t): %v (validate %v, findings %v)", filepath.Base(src), name, unknown, why, v.validate, v.findings)
				}
			}
		}
	}
}
