package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDiagnoseSeesADamagedSampleTable: Diagnose read the boxes and the track headers and called healthy a file whose
// sample table points outside the media - a chunk at offset 0, inside the ftyp - which every remux and edit then
// refuses or copies wrong. The sample tables are read and their chunks checked against the mdat boxes.
func TestDiagnoseSeesADamagedSampleTable(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 50; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0, 0, 0, 1, 0x65, byte(i)}})
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	good := filepath.Join(t.TempDir(), "good.mp4")
	if err := RemuxToMP4(context.Background(), src, good); err != nil {
		t.Fatal(err)
	}
	d, err := Diagnose(context.Background(), good)
	if err != nil || !d.Healthy {
		t.Fatalf("the sound file: %v, %+v", err, d)
	}
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte("stco"))
	if i < 0 {
		t.Fatal("no stco in the fixture")
	}
	binary.BigEndian.PutUint32(data[i+12:], 0) // the first chunk now starts at offset 0
	bad := filepath.Join(t.TempDir(), "bad.mp4")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err = Diagnose(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if d.Healthy {
		t.Errorf("a chunk at offset 0 diagnosed healthy: %+v", d.Findings)
	}
}
