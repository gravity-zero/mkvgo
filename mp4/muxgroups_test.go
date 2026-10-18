package mp4

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestMuxRegroupsTracksFromDifferentSources: tracks muxed from files that grouped them differently share their type's group, as to-mp4 writes it.
func TestMuxRegroupsTracksFromDifferentSources(t *testing.T) {
	old := editFixture(t)
	// An MP4 written before the grouping: every tkhd alternate_group 0.
	rewriteTailMoov(t, old, func(p []byte) []byte {
		return mapTraks(t, p, func(_ int, trak []byte) []byte {
			tb, err := iterBoxes(trak)
			if err != nil {
				t.Fatal(err)
			}
			var w bw
			for _, b := range tb {
				if b.typ == "tkhd" {
					b.payload = append([]byte(nil), b.payload...)
					binary.BigEndian.PutUint16(b.payload[34:36], 0)
				}
				w.bytes(box(b.typ, b.payload))
			}
			return w.b
		})
	})
	dst := filepath.Join(t.TempDir(), "mux.mp4")
	err := Mux(context.Background(), mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{
		{SourcePath: old, TrackID: 1, IsDefault: true},
		{SourcePath: old, TrackID: 2, IsDefault: true},
		{SourcePath: realFixture("bframes_aac.mp4"), TrackID: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"vide", "0"}, {"soun", "1"}, {"soun", "1"}}
	if got := trakGroups(t, data); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("groups %v, want %v", got, want)
	}
}
