package ops

import (
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// The same frames give the same fingerprint whether a track stores them as is
// or with their header stripped, while the stored-bytes digest tells them apart.
func TestFingerprintIgnoresHeaderStripping(t *testing.T) {
	w, h := uint32(320), uint32(240)
	frame := func(i int) []byte { return append([]byte{0, 0, 1, 0x2D, 0x65, byte(i)}, make([]byte, 299)...) }
	var plain, stripped []mkv.Block
	for i := 0; i < 50; i++ {
		plain = append(plain, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: i%25 == 0, Data: frame(i)})
		stripped = append(stripped, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: i%25 == 0, Data: frame(i)[3:]})
	}
	dir := t.TempDir()
	a := buildMKVWithCues(t, dir, "plain.mkv", []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h}}, [][]mkv.Block{plain[:25], plain[25:]}, []mkv.CuePoint{{TimeMs: 0, Track: 1}})
	b := buildMKVWithCues(t, dir, "stripped.mkv", []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h, Compression: mkv.CompressionHeaderStrip, HeaderStripping: []byte{0, 0, 1}}}, [][]mkv.Block{stripped[:25], stripped[25:]}, []mkv.CuePoint{{TimeMs: 0, Track: 1}})
	ctx := context.Background()
	fa, err := Fingerprint(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Fingerprint(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if fa.Presentation != fb.Presentation || fa.Tracks[0].SHA256 != fb.Tracks[0].SHA256 {
		t.Errorf("fingerprints differ on the same content:\n%s\n%s", fa.Presentation, fb.Presentation)
	}
	_, da, _ := digestTracks(ctx, a, nil, nil, false)
	_, db, _ := digestTracks(ctx, b, nil, nil, false)
	if da[0].hash == db[0].hash {
		t.Errorf("the stored-bytes digest must still tell the stripped track apart")
	}
}
