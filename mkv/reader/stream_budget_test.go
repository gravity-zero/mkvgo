package reader

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestStreamParserChargesBudget covers the streaming reader's cumulative metadata budget: the decoders it borrows
// from the seekable parser charge it, so a Tracks element past what remains is refused before its bytes are held,
// and what one element spends is gone for the next.
func TestStreamParserChargesBudget(t *testing.T) {
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	ebml.WriteElementHeader(&te, mkv.IDCodecPrivate, 50)
	te.Write(make([]byte, 50))
	tracks := wrapEl(mkv.IDTrackEntry, te.Bytes())

	for _, tc := range []struct {
		budget int64
		fail   bool
	}{{10, true}, {maxMetadataBytes, false}} {
		p := &streamParser{r: bytes.NewReader(tracks), metaBudget: tc.budget}
		err := p.decode(int64(len(tracks)), func(d *parser) error { return d.parseTracks(int64(len(tracks)), &mkv.Container{}) })
		if (err != nil) != tc.fail {
			t.Errorf("budget %d: %v, want failure %v", tc.budget, err, tc.fail)
		}
		if !tc.fail && p.metaBudget > tc.budget-50 {
			t.Errorf("budget %d: %d left after a 50-byte CodecPrivate, want it charged", tc.budget, p.metaBudget)
		}
	}
}
