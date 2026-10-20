package mp4

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A sealed Segment followed by junk serves what the Segment alone serves: the junk lies past the Segment's end, where
// a window's walk stops, so it is no damage to skip or to refuse a window on, whatever DamageBytes allows.
func TestHLSPlanIgnoresJunkPastTheSegment(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 48000.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 250; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0, 0, 0, 1, 0x65, byte(i)}})
	}
	for i := 0; i < 500; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(blocks)
	clean := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	data, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, 8192)
	rand.New(rand.NewSource(20261020)).Read(junk)
	junked := filepath.Join(t.TempDir(), "junk.mkv")
	if err := os.WriteFile(junked, append(data, junk...), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, damage := range []int64{0, 1024} {
		want, err := PlanHLS(ctx, clean, Options{SegmentMs: 2000, DamageBytes: damage})
		if err != nil {
			t.Fatal(err)
		}
		got, err := PlanHLS(ctx, junked, Options{SegmentMs: 2000, DamageBytes: damage})
		if err != nil {
			t.Fatalf("DamageBytes %d: %v", damage, err)
		}
		for _, name := range want.Resources() {
			a, _, aerr := want.Resource(ctx, name)
			b, _, berr := got.Resource(ctx, name)
			if aerr != nil || berr != nil || !bytes.Equal(a, b) {
				t.Errorf("DamageBytes %d, %s: clean %d bytes (%v), with junk past the Segment %d bytes (%v)", damage, name, len(a), aerr, len(b), berr)
			}
		}
		if st := got.Stats(); st.DamageSkips != 0 || st.DamageRefusals != 0 {
			t.Errorf("DamageBytes %d: the junk past the Segment counted as damage: %d skipped, %d refused", damage, st.DamageSkips, st.DamageRefusals)
		}
	}
}
