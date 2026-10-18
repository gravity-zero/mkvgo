package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// withTrakBefore returns an MP4 whose last box, its moov, holds trak inserted before its nth trak (1-based); the
// bytes before the moov are kept as they are, so no sample moves.
func withTrakBefore(t *testing.T, data []byte, nth int, trak []byte) []byte {
	t.Helper()
	at := 0
	for at < len(data) {
		size := int(binary.BigEndian.Uint32(data[at:]))
		if size == 1 {
			size = int(binary.BigEndian.Uint64(data[at+8:]))
		}
		if string(data[at+4:at+8]) == "moov" {
			if at+size != len(data) {
				t.Fatal("the fixture's moov is not its last box")
			}
			break
		}
		at += size
	}
	kids, err := iterBoxes(data[at+8:])
	if err != nil {
		t.Fatal(err)
	}
	var moov []byte
	k := 0
	for _, c := range kids {
		if c.typ == "trak" {
			if k++; k == nth {
				moov = append(moov, trak...)
			}
		}
		moov = append(moov, box(c.typ, c.payload)...)
	}
	return append(append([]byte(nil), data[:at]...), box("moov", moov)...)
}

// TestMP4DiagnoseNamesTracksAsRetimeDoes pins that the audio-delay remedy numbers tracks as retime reads them: a
// timecode track before the audio is no track of the probe's, so the remedy shifts the track retime moved.
func TestMP4DiagnoseNamesTracksAsRetimeDoes(t *testing.T) {
	ctx := context.Background()
	path := editShiftFixture(t, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmcd := bytes.Replace(soundTrak(9, 600, 0, audioSampleEntry("tmcd", &stereo48k, nil), emptyTables()...), []byte("soun"), []byte("tmcd"), 1)
	if err := os.WriteFile(path, withTrakBefore(t, data, 2, tmcd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: 300_000_000}); err != nil {
		t.Fatal(err)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Findings) != 1 || d.Findings[0].Track != 2 || !strings.Contains(d.Findings[0].Remedy, "retime --shift 2=-300") || d.AudioDelaysNs[2] != 300_000_000 {
		t.Fatalf("findings %+v, delays %v; want track 2 delayed 300 ms with the retime that cancels it", d.Findings, d.AudioDelaysNs)
	}
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: -300_000_000}); err != nil {
		t.Fatal(err)
	}
	if kinds := diagKinds(t, path); len(kinds) != 0 {
		t.Errorf("after the remedy the file must be healthy again, got %v", kinds)
	}
}
