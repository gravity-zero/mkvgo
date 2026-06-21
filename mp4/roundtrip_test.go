package mp4

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func u16p(v uint16) *uint16 { return &v }

// TestRoundTripColourAndSubtitles checks that colour code points and SRT cues
// survive the full MKV → MP4 → MKV round trip (exercising the colr box and the
// tx3g text track in both directions, plus the MKV writer's Colour element and
// BlockGroup/BlockDuration support).
func TestRoundTripColourAndSubtitles(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{0x01, 0x02, 0x03, 0x04},
			Width: u32p(3840), Height: u32p(2160), FrameRate: f64p(25),
			ColorPrimaries: u16p(9), ColorTransfer: u16p(16), ColorSpace: u16p(9), ColorRange: u16p(1)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 1000, key: true, data: []byte("Bonjour")},
		{track: 1, pts: 40, key: false, data: []byte{2}},
		{track: 2, pts: 3000, key: true, data: []byte("<i>Au revoir</i>")},
	}
	srcMKV := buildMKV(t, tracks, blocks)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}

	c, gotBlocks := readMKV(t, outMKV)

	var video, subs *mkv.Track
	for i := range c.Tracks {
		switch c.Tracks[i].Type {
		case mkv.VideoTrack:
			video = &c.Tracks[i]
		case mkv.SubtitleTrack:
			subs = &c.Tracks[i]
		}
	}
	if video == nil || subs == nil {
		t.Fatalf("round trip lost a track: video=%v subs=%v", video != nil, subs != nil)
	}

	// Colour code points must survive.
	if video.ColorPrimaries == nil || *video.ColorPrimaries != 9 ||
		video.ColorTransfer == nil || *video.ColorTransfer != 16 ||
		video.ColorSpace == nil || *video.ColorSpace != 9 {
		t.Errorf("colour not round-tripped: primaries=%v transfer=%v matrix=%v",
			video.ColorPrimaries, video.ColorTransfer, video.ColorSpace)
	}

	// Subtitle cues must survive with their text (markup stripped) and a duration.
	if subs.Codec != "srt" {
		t.Errorf("subtitle codec = %q, want srt", subs.Codec)
	}
	var cues []mkv.Block
	for _, b := range gotBlocks {
		if b.TrackNumber == subs.ID {
			cues = append(cues, b)
		}
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	joined := bytes.Join([][]byte{cues[0].Data, cues[1].Data}, []byte("|"))
	if !bytes.Contains(joined, []byte("Bonjour")) || !bytes.Contains(joined, []byte("Au revoir")) {
		t.Errorf("cue text lost: %q", joined)
	}
	if bytes.Contains(joined, []byte("<i>")) {
		t.Errorf("markup should be stripped: %q", joined)
	}
	for _, cue := range cues {
		if cue.Duration <= 0 {
			t.Errorf("cue %q has no duration", cue.Data)
		}
	}
}
