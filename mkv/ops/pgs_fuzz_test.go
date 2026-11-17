package ops

import (
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// FuzzPGSAssembler: two PGS blocks of any bytes decode and flatten to cues or an error, never a panic, and every cue
// ends after it starts.
func FuzzPGSAssembler(f *testing.F) {
	f.Add(pgsShow(1, 100, 900, true), pgsClear(), int64(0))
	f.Add(pgsShow(1, 0, 0, true), pgsShow(2, 1916, 1078, false), int64(40))
	f.Fuzz(func(t *testing.T, first, second []byte, durMs int64) {
		costguard.Fuzz(t, len(first)+len(second), func() {
			var cues []PGSCue
			a, err := newBitmapAssembler(func(c PGSCue) error { cues = append(cues, c); return nil }, &mkv.Track{Codec: "pgs"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if a.block(mkv.Block{Timecode: 1000, Duration: durMs % 5000, Data: first}) != nil || a.block(mkv.Block{Timecode: 2000, Data: second}) != nil {
				return
			}
			for _, c := range cues {
				if c.EndMs <= c.StartMs {
					t.Fatalf("cue %d..%d ms ends before it starts", c.StartMs, c.EndMs)
				}
			}
		})
	})
}

// FuzzBitmapAssembler: two VobSub or DVB blocks of any bytes, with a region lost between them or not, decode and
// flatten to cues or an error, never a panic, and every cue ends after it starts.
func FuzzBitmapAssembler(f *testing.F) {
	f.Add(false, []byte{0x00, 0x08, 0x00, 0x04, 0x00, 0x00, 0x00, 0x04}, []byte{0x00, 0x0a}, int64(0), false)
	f.Add(true, []byte{0x0f, 0x10, 0x00, 0x01, 0x00, 0x02, 0x0f, 0x20, 0x0f, 0x80, 0x00, 0x01, 0x00, 0x00}, []byte{0x0f, 0x80, 0x00, 0x01, 0x00, 0x00}, int64(40), true)
	f.Fuzz(func(t *testing.T, dvb bool, first, second []byte, durMs int64, lost bool) {
		costguard.Fuzz(t, len(first)+len(second), func() {
			track := &mkv.Track{Codec: "vobsub", CodecPrivate: []byte("size: 720x576\npalette: 000000, ff0000, 00ff00, 0000ff, ffffff, 101010, 202020, 303030, 404040, 505050, 606060, 707070, 808080, 909090, a0a0a0, b0b0b0\n")}
			if dvb {
				track = &mkv.Track{Codec: "dvbsub"}
			}
			var cues []BitmapCue
			a, err := newBitmapAssembler(func(c BitmapCue) error { cues = append(cues, c); return nil }, track, nil)
			if err != nil {
				t.Fatal(err)
			}
			if a.block(mkv.Block{Timecode: 1000, Duration: durMs % 5000, Data: first}) != nil {
				return
			}
			if lost && a.lost(1500) != nil {
				return
			}
			if a.block(mkv.Block{Timecode: 2000, Data: second}) != nil || a.finish() != nil {
				return
			}
			for _, c := range cues {
				if c.EndMs <= c.StartMs {
					t.Fatalf("cue %d..%d ms ends before it starts", c.StartMs, c.EndMs)
				}
			}
		})
	})
}
