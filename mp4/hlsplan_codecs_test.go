package mp4

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
	"github.com/gravity-zero/mkvgo/mkv"
)

// dtsCoreFrame is a DTS core frame header stating 16 blocks of 32 samples.
var dtsCoreFrame = []byte{0x7F, 0xFE, 0x80, 0x01, 0x00, 0x3C, 0x00, 0x00, 0, 0, 0, 0}

// codecsSource writes an HEVC track, its hvcC stating the high tier when high is set, beside one audio track.
func codecsSource(t *testing.T, high bool, audio mkv.Track, frame []byte, frameMs int64) string {
	t.Helper()
	fx := dovitest.Build(t, t.TempDir(), dovitest.Spec{})
	video := fx.Track
	video.DolbyVision = nil
	video.CodecPrivate = append([]byte(nil), video.CodecPrivate...)
	video.CodecPrivate[1] &^= 0x20
	if high {
		video.CodecPrivate[1] |= 0x20 // general_tier_flag
	}
	audio.ID, audio.Type = 2, mkv.AudioTrack
	var blocks []genBlock
	ai := int64(0)
	for i, base := range fx.Base {
		pts := int64(i) * 40
		for ; ai*frameMs < pts; ai++ {
			blocks = append(blocks, genBlock{track: 2, pts: ai * frameMs, key: true, data: frame})
		}
		blocks = append(blocks, genBlock{track: 1, pts: pts, key: i == 0, data: base})
	}
	return buildKeyframeClusteredMKV(t, []mkv.Track{video, audio}, blocks)
}

var codecsAttr = regexp.MustCompile(`[,:]CODECS="([^"]*)"`)

// masterCodecs returns the CODECS of the master's variant, ok false when it has none.
func masterCodecs(t *testing.T, master []byte) (string, bool) {
	t.Helper()
	for _, line := range strings.Split(string(master), "\n") {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		m := codecsAttr.FindStringSubmatch(line)
		if m == nil {
			return "", false
		}
		return m[1], true
	}
	t.Fatalf("no variant in the master:\n%s", master)
	return "", false
}

// TestPlanCodecs: a caller that writes its own master had to rebuild each track's codec string, and guessed the HEVC
// tier. The plan hands out the strings it writes, track by track, so the video's stays known when an audio track
// with no string makes the master drop CODECS.
func TestPlanCodecs(t *testing.T) {
	sr, ch := 48000.0, uint8(2)
	for _, tc := range []struct {
		name      string
		high      bool
		audio     mkv.Track
		frame     []byte
		frameMs   int64
		tier      string
		audioWant PlanCodec
	}{
		{"main tier, AAC", false, mkv.Track{Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}, []byte{0xAA, 0x01}, 20, ".L", PlanCodec{TrackID: 2, Type: mkv.AudioTrack, Codec: "aac", RFC6381: "mp4a.40.2", Known: true}},
		{"high tier, AAC", true, mkv.Track{Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}, []byte{0xAA, 0x01}, 20, ".H", PlanCodec{TrackID: 2, Type: mkv.AudioTrack, Codec: "aac", RFC6381: "mp4a.40.2", Known: true}},
		{"high tier, DTS-HD", true, mkv.Track{Codec: "dts", Profile: "DTS-HD MA", SampleRate: &sr, Channels: &ch}, dtsCoreFrame, 10, ".H", PlanCodec{TrackID: 2, Type: mkv.AudioTrack, Codec: "dts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := codecsSource(t, tc.high, tc.audio, tc.frame, tc.frameMs)
			plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
			if err != nil {
				t.Fatal(err)
			}
			got := plan.Codecs()
			if len(got) != 2 {
				t.Fatalf("%d tracks listed, want the video and the audio: %+v", len(got), got)
			}
			v := got[0]
			if v.TrackID != 1 || v.Type != mkv.VideoTrack || v.Codec != "hevc" || !v.Known || !strings.HasPrefix(v.RFC6381, "hvc1.") {
				t.Errorf("video listed as %+v, want track 1, hevc, a known hvc1 string", v)
			}
			if parts := strings.Split(v.RFC6381, "."); len(parts) < 4 || !strings.HasPrefix("."+parts[3], tc.tier) {
				t.Errorf("video string %q, want tier %q ahead of the level", v.RFC6381, tc.tier)
			}
			if got[1] != tc.audioWant {
				t.Errorf("audio listed as %+v, want %+v", got[1], tc.audioWant)
			}
			written, ok := masterCodecs(t, plan.MasterPlaylist())
			t.Logf("listed %q and %q (known %v), master CODECS %q (present %v)", v.RFC6381, got[1].RFC6381, got[1].Known, written, ok)
			if !tc.audioWant.Known {
				if ok {
					t.Errorf("master names CODECS=%q with an audio track that has no string", written)
				}
				return
			}
			if want := v.RFC6381 + "," + got[1].RFC6381; !ok || written != want {
				t.Errorf("master CODECS %q (present %v), the listed strings give %q", written, ok, want)
			}
		})
	}
}

// TestPlanCodecsOfAConvertedTrack: the string listed for a profile 7 track is the one the master writes in each mode.
func TestPlanCodecsOfAConvertedTrack(t *testing.T) {
	fx := dovitest.Build(t, t.TempDir(), dovitest.Spec{Full: true, HDR: true})
	for _, mode := range []mkv.DolbyVisionConversion{mkv.DolbyVisionKeep, mkv.DolbyVisionToProfile81, mkv.DolbyVisionToBaseLayer} {
		plan, err := PlanHLS(context.Background(), fx.Path, Options{SegmentMs: 2000, DolbyVision: mode})
		if err != nil {
			t.Fatal(err)
		}
		got := plan.Codecs()
		written, ok := masterCodecs(t, plan.MasterPlaylist())
		if len(got) != 1 || !got[0].Known || !ok || written != got[0].RFC6381 || !strings.HasPrefix(written, "hvc1.") {
			t.Errorf("mode %d: listed %+v, master CODECS %q (present %v)", mode, got, written, ok)
		}
	}
}
