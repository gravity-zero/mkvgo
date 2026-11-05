package mp4

import (
	"encoding/binary"
	"testing"
)

// TestEntryJOCReadsOnlyAnEC3Entry: entryJOC looked for "dec3" anywhere in any sample entry - an AAC esds copies the
// track's CodecPrivate verbatim - and bounded the box it found by i-4+size, which wraps on 32-bit: a CodecPrivate of
// 7F FF FF FF "dec3" crashed every manifest there, and elsewhere a crafted one announced Atmos on a non-E-AC-3 track.
func TestEntryJOCReadsOnlyAnEC3Entry(t *testing.T) {
	box := func(typ string, body []byte) []byte {
		b := make([]byte, 8, 8+len(body))
		binary.BigEndian.PutUint32(b, uint32(8+len(body)))
		copy(b[4:], typ)
		return append(b, body...)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	wrap := box("mp4a", append(make([]byte, 28), 0x7F, 0xFF, 0xFF, 0xFF, 'd', 'e', 'c', '3', 0, 0, 0, 0, 0, 0, 0, 0))
	if got := entryJOC(wrap); got != 0 {
		t.Errorf("a dec3 of 2 GiB declared inside an mp4a entry: JOC %d", got)
	}
	dec3 := box("dec3", []byte{0, 0, 0, 0, 0, 1, 16}) // one substream, flag_ec3_extension_type_a, complexity 16
	if got := entryJOC(box("mp4a", append(make([]byte, 28), dec3...))); got != 0 {
		t.Errorf("an mp4a entry holding a dec3 announces JOC %d", got)
	}
	if got := entryJOC(box("ec-3", append(make([]byte, 28), dec3...))); got != 16 {
		t.Errorf("an ec-3 entry's dec3: JOC %d, want 16", got)
	}
}
