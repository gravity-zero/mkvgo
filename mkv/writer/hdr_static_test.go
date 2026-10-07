package writer

import (
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestWriteHDRStaticRoundTrip(t *testing.T) {
	md := &mkv.MasteringDisplay{
		RedX: 0.708, RedY: 0.292, GreenX: 0.170, GreenY: 0.797, BlueX: 0.131, BlueY: 0.046,
		WhiteX: 0.3127, WhiteY: 0.3290, LuminanceMax: 1000, LuminanceMin: 0.0001,
	}
	pq := uint16(16)
	write := func(hdr *mkv.HDRStaticMetadata) *mkv.HDRStaticMetadata {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "av1", IsDefault: true, ColorTransfer: &pq, HDR: hdr}},
		})
		if len(got.Tracks) != 1 || got.Tracks[0].ColorTransfer == nil || *got.Tracks[0].ColorTransfer != pq {
			t.Fatalf("colour code points lost: %+v", got.Tracks)
		}
		return got.Tracks[0].HDR
	}

	t.Run("both", func(t *testing.T) {
		got := write(&mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400, MasteringDisplay: md})
		if got == nil || got.MaxCLL != 1000 || got.MaxFALL != 400 {
			t.Fatalf("HDR = %+v, want MaxCLL 1000 / MaxFALL 400", got)
		}
		wantMastering(t, got.MasteringDisplay, md, 1e-9)
	})
	t.Run("mastering display only", func(t *testing.T) {
		got := write(&mkv.HDRStaticMetadata{MasteringDisplay: md})
		if got == nil || got.MaxCLL != 0 || got.MaxFALL != 0 {
			t.Fatalf("HDR = %+v, want no light level", got)
		}
		wantMastering(t, got.MasteringDisplay, md, 1e-9)
	})
	t.Run("light level only", func(t *testing.T) {
		got := write(&mkv.HDRStaticMetadata{MaxCLL: 600})
		if got == nil || got.MaxCLL != 600 || got.MaxFALL != 0 || got.MasteringDisplay != nil {
			t.Fatalf("HDR = %+v, want MaxCLL 600 alone", got)
		}
	})
	t.Run("empty holder writes nothing", func(t *testing.T) {
		if got := write(&mkv.HDRStaticMetadata{MasteringDisplay: &mkv.MasteringDisplay{}}); got != nil {
			t.Fatalf("HDR = %+v, want nil", got)
		}
	})
	t.Run("partial mastering display writes nothing", func(t *testing.T) {
		if got := write(&mkv.HDRStaticMetadata{MasteringDisplay: &mkv.MasteringDisplay{RedX: 0.708, LuminanceMax: 1000}}); got != nil {
			t.Fatalf("HDR = %+v, want nil", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		if got := write(nil); got != nil {
			t.Fatalf("HDR = %+v, want nil", got)
		}
	})
}

func wantMastering(t *testing.T, got, want *mkv.MasteringDisplay, tol float64) {
	t.Helper()
	if got == nil {
		t.Fatal("MasteringDisplay is nil")
	}
	for _, f := range []struct {
		name      string
		got, want float64
	}{
		{"RedX", got.RedX, want.RedX}, {"RedY", got.RedY, want.RedY},
		{"GreenX", got.GreenX, want.GreenX}, {"GreenY", got.GreenY, want.GreenY},
		{"BlueX", got.BlueX, want.BlueX}, {"BlueY", got.BlueY, want.BlueY},
		{"WhiteX", got.WhiteX, want.WhiteX}, {"WhiteY", got.WhiteY, want.WhiteY},
		{"LuminanceMax", got.LuminanceMax, want.LuminanceMax}, {"LuminanceMin", got.LuminanceMin, want.LuminanceMin},
	} {
		if math.Abs(f.got-f.want) > tol {
			t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
		}
	}
}
