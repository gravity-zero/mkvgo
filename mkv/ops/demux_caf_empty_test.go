package ops

import (
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An empty ALAC packet is refused: a laced block of 7 bytes holds 256 of them, each one more entry of the packet
// table kept until the end of the stream, 2 KiB of memory for 7 bytes read.
func TestDemuxCAFRefusesAnEmptyPacket(t *testing.T) {
	f := cafALACFormat(mkv.Track{CodecPrivate: make([]byte, 24)})
	if f == nil {
		t.Fatal("no CAF format for a 24-byte cookie")
	}
	err := f.frame(io.Discard, mkv.Block{}, nil)
	var fe *frameError
	if !errors.As(err, &fe) {
		t.Errorf("an empty packet: err = %v, want a frame the file cannot hold", err)
	}
}
