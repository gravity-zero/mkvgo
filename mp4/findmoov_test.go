package mp4

import (
	"bytes"
	"testing"
)

// TestFindMoovBackwardFallback covers a file whose linear box walk desyncs - an
// mdat that declares a size shorter than its content sends the walk into the
// media data - yet the moov sits at the end. The forward walk fails; findMoov
// must recover it with the backward scan, as external probers do.
func TestFindMoovBackwardFallback(t *testing.T) {
	ftyp := box4("ftyp", make([]byte, 8))
	// Declares size 16 but carries 100 bytes of 0xFF: the walk lands in the
	// content and reads a bogus, oversized box.
	badMdat := append(append(u32be(16), []byte("mdat")...), bytes.Repeat([]byte{0xFF}, 100)...)
	moov := box4("moov", box4("mvhd", make([]byte, 100)))
	file := bytes.Join([][]byte{ftyp, badMdat, moov}, nil)
	size := int64(len(file))

	if _, _, err := findMoovForward(bytes.NewReader(file), size); err == nil {
		t.Fatal("the forward walk should desync on the bad mdat")
	}

	off, plen, err := findMoov(bytes.NewReader(file), size)
	if err != nil {
		t.Fatalf("findMoov should recover via the backward scan: %v", err)
	}
	wantOff := int64(len(ftyp)+len(badMdat)) + 8
	if off != wantOff || plen != int64(len(moov))-8 {
		t.Errorf("moov payload = (off %d, len %d), want (%d, %d)", off, plen, wantOff, len(moov)-8)
	}

	// A 'moov' byte sequence inside the media (not a real box) must not be
	// mistaken for the moov: with no valid moov, findMoov reports an error.
	noMoov := bytes.Join([][]byte{ftyp, append(append(u32be(16), []byte("mdat")...), bytes.Repeat([]byte("moov"), 25)...)}, nil)
	if _, _, err := findMoov(bytes.NewReader(noMoov), int64(len(noMoov))); err == nil {
		t.Error("a 'moov' string in the media must not be accepted as the moov box")
	}
}
