package mp4

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// quicktimeFixture is a real muxer-written QuickTime file (brand "qt  ") with
// the classic non-faststart layout: ftyp + wide + mdat FIRST, moov at the END
// of the file, and an mp4a SoundDescription VERSION 1 whose esds is wrapped in
// a 'wave' extension - the layout every raw iPhone/camera .mov uses.
const quicktimeFixture = "../internal/testdata/quicktime.mov"

func TestOpenMetaQuickTimeMov(t *testing.T) {
	c, dropped, err := OpenMeta(context.Background(), quicktimeFixture)
	if err != nil {
		t.Fatalf("OpenMeta on a non-faststart QuickTime file: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %+v, want none", dropped)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (video+audio)", len(c.Tracks))
	}
	v, a := c.Tracks[0], c.Tracks[1]
	if v.Codec != "h264" || v.Width == nil || *v.Width != 160 || v.Height == nil || *v.Height != 120 {
		t.Errorf("video = %s %vx%v, want h264 160x120", v.Codec, v.Width, v.Height)
	}
	if a.Codec != "aac" {
		t.Errorf("audio codec = %q, want aac (esds is wrapped in a wave atom)", a.Codec)
	}
	if a.SampleRate == nil || *a.SampleRate != 44100 {
		t.Errorf("audio sample rate = %v, want 44100", a.SampleRate)
	}
	if a.Channels == nil || *a.Channels != 1 {
		t.Errorf("audio channels = %v, want 1", a.Channels)
	}
	if c.DurationMs < 300 || c.DurationMs > 600 {
		t.Errorf("DurationMs = %d, want ~400", c.DurationMs)
	}
}

func TestRemuxFromQuickTimeMov(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), quicktimeFixture, out); err != nil {
		t.Fatalf("RemuxFromMP4 on a QuickTime file: %v", err)
	}
	c, err := reader.Open(context.Background(), out)
	if err != nil {
		t.Fatalf("read back remuxed MKV: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("remuxed tracks = %d, want 2", len(c.Tracks))
	}
	if c.Tracks[0].Codec != "h264" || c.Tracks[1].Codec != "aac" {
		t.Errorf("remuxed codecs = %s/%s, want h264/aac", c.Tracks[0].Codec, c.Tracks[1].Codec)
	}
	if len(c.Cues) == 0 {
		t.Errorf("remuxed MKV has no Cues")
	}
}

func TestAudioExtOffset(t *testing.T) {
	entry := make([]byte, 64)
	// version field at payload[8:10]
	for _, tc := range []struct {
		version uint16
		want    int
	}{{0, 28}, {1, 44}, {2, 64}, {3, 28}} {
		binary.BigEndian.PutUint16(entry[8:10], tc.version)
		if got := audioExtOffset(entry); got != tc.want {
			t.Errorf("audioExtOffset(version=%d) = %d, want %d", tc.version, got, tc.want)
		}
	}
	if got := audioExtOffset(nil); got != 28 {
		t.Errorf("audioExtOffset(nil) = %d, want 28", got)
	}
}

// A QuickTime SoundDescription v2 carries its sample rate as a float64 and the
// channel count as a 32-bit int; the v0 fields hold placeholder constants.
func TestParseAudioFieldsV2(t *testing.T) {
	payload := make([]byte, 64)
	binary.BigEndian.PutUint16(payload[8:10], 2)                        // version
	binary.BigEndian.PutUint16(payload[16:18], 3)                       // always3 (v0 channel slot)
	binary.BigEndian.PutUint64(payload[32:40], math.Float64bits(96000)) // sample rate
	binary.BigEndian.PutUint32(payload[40:44], 6)                       // channels
	var tr inTrack
	parseAudioFields(&tr, payload)
	if tr.sampleRate != 96000 {
		t.Errorf("v2 sampleRate = %v, want 96000", tr.sampleRate)
	}
	if tr.channels != 6 {
		t.Errorf("v2 channels = %d, want 6 (not the v0 placeholder 3)", tr.channels)
	}
}
