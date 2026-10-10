package ops

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// diagnose reports an audio track whose laced frames carry no DefaultDuration,
// no frame size in the stream and no trusted frame count; a frame count lifts
// the finding.
func TestDiagnoseFindsUnsizedLacedAudio(t *testing.T) {
	build := func(withStats bool) string {
		path := filepath.Join(t.TempDir(), "opus.mkv")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		w, h := uint32(320), uint32(240)
		sr := 48000.0
		ch := uint8(2)
		opusHead := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0, 0x80, 0xBB, 0, 0, 0, 0, 0}
		tracks := []mkv.Track{
			{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
			{ID: 2, UID: 22, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead, SampleRate: &sr, Channels: &ch},
		}
		var tags []mkv.Tag
		if withStats {
			tags = []mkv.Tag{{TargetID: 22, SimpleTags: []mkv.SimpleTag{{Name: "_STATISTICS_WRITING_APP", Value: "test"}, {Name: "DURATION", Value: "00:00:03.000000000"}, {Name: "NUMBER_OF_FRAMES", Value: strconv.Itoa(24 * 8)}}}}
		}
		mw := writer.NewMKVWriter(f)
		if err := mw.WriteStart(); err != nil {
			t.Fatal(err)
		}
		c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}, Tags: tags}
		if err := mw.WriteMetadata(c, tracks, 3000); err != nil {
			t.Fatal(err)
		}
		for cl := 0; cl < 3; cl++ {
			var blocks []mkv.Block
			for k := 0; k < 8; k++ {
				tc := int64(cl*1000 + k*171)
				blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(cl*1000 + k*125), Keyframe: k == 0, Data: []byte{0, 0, 1, 0x65, byte(k)}})
				for fr := 0; fr < 8; fr++ { // eight frames on one timecode: written back as one lace
					blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte{0xFC, byte(k), byte(fr)}})
				}
			}
			if err := mw.WriteClusterWithCues(int64(cl*1000), 1000000, blocks); err != nil {
				t.Fatal(err)
			}
		}
		if err := mw.Finalize(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		stats bool
		want  bool
	}{{false, true}, {true, false}} {
		d, err := Diagnose(context.Background(), build(tc.stats))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range d.Findings {
			if f.Kind == "unsized-laced-audio" {
				found = true
			}
		}
		if found != tc.want {
			t.Errorf("stats=%v: unsized-laced-audio reported=%v, want %v (%+v)", tc.stats, found, tc.want, d.Findings)
		}
	}
}
