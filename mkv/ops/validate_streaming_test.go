package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// The streaming-readiness checks catch the real bug classes this project has
// hit: cues keyed on an audio block, cue times matching no keyframe, subtitle
// blocks without durations, video without DefaultDuration.
func TestValidateStreamingReadiness(t *testing.T) {
	w, h := uint32(320), uint32(240)
	build := func(tamper func(mw *writer.MKVWriter), blocks []mkv.Block, tracks []mkv.Track) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "f.mkv")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		mw := writer.NewMKVWriter(f)
		c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "x", WritingApp: "x"}}
		if err := mw.WriteStart(); err != nil {
			t.Fatal(err)
		}
		if err := mw.WriteMetadata(c, tracks, 2000); err != nil {
			t.Fatal(err)
		}
		if err := mw.WriteClusterWithCues(0, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
		if tamper != nil {
			tamper(mw)
		}
		if err := mw.Finalize(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		return path
	}
	fr := 25.0
	vids := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: []byte{1, 2, 3, 4, 5}, Width: &w, Height: &h, FrameRate: &fr, Language: "und"},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x12, 0x10}, SampleRate: f64(44100), Language: "und"},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "und"},
	}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{1}},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte{2}},
		{TrackNumber: 3, Timecode: 100, Keyframe: true, Data: []byte("cue sans duree")},
	}

	find := func(path, needle string) bool {
		t.Helper()
		issues, err := Validate(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		for _, is := range issues {
			if strings.Contains(is.Message, needle) {
				return true
			}
		}
		return false
	}

	// Mis-keyed cue (the audio-cue bug class) → error.
	badCue := build(func(mw *writer.MKVWriter) {
		mw.Cues = []mkv.CuePoint{{TimeMs: 0, Track: 2, ClusterPos: mw.Cues[0].ClusterPos}}
	}, blocks, vids)
	if !find(badCue, "non-video track") {
		t.Error("audio-keyed cue not reported")
	}

	// Stale cue time → warning.
	stale := build(func(mw *writer.MKVWriter) {
		mw.Cues = []mkv.CuePoint{{TimeMs: 16, Track: 1, ClusterPos: mw.Cues[0].ClusterPos}}
	}, blocks, vids)
	if !find(stale, "match no video keyframe") {
		t.Error("stale cue time not reported")
	}

	// Subtitle blocks without BlockDuration → warning.
	if !find(badCue, "no BlockDuration") {
		t.Error("missing subtitle durations not reported")
	}

	// A clean write reports none of these.
	good := build(nil, blocks, vids)
	for _, needle := range []string{"non-video track", "match no video keyframe"} {
		if find(good, needle) {
			t.Errorf("clean file wrongly reports %q", needle)
		}
	}

	// No Cues at all → readiness warning.
	noCues := build(func(mw *writer.MKVWriter) { mw.Cues = nil }, blocks, vids)
	if !find(noCues, "no Cues index") {
		t.Error("missing Cues not reported")
	}

	// Severity of the mis-keyed cue depends on whether a video cue survives it:
	// with none, the index cannot seek video (error); alongside one, the audio
	// cue is inert - the keyframe index drops it - so it is bloat (warning), the
	// shape a muxer that cues every track produces on a perfectly seekable file.
	severity := func(path, code string) (mkv.Severity, bool) {
		t.Helper()
		issues, err := Validate(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		for _, is := range issues {
			if is.Code == code {
				return is.Severity, true
			}
		}
		return "", false
	}
	if sev, ok := severity(badCue, "cues-non-video"); !ok || sev != mkv.SeverityError {
		t.Errorf("audio-only index: severity = %q (found=%v), want error", sev, ok)
	}

	everyTrackCued := build(func(mw *writer.MKVWriter) {
		pos := mw.Cues[0].ClusterPos
		mw.Cues = []mkv.CuePoint{{TimeMs: 0, Track: 1, ClusterPos: pos}, {TimeMs: 0, Track: 2, ClusterPos: pos}}
	}, blocks, vids)
	sev, ok := severity(everyTrackCued, "cues-non-video")
	if !ok || sev != mkv.SeverityWarning {
		t.Errorf("every-track-cued index: severity = %q (found=%v), want warning - the audio cue is inert, the video cue seeks", sev, ok)
	}
}
