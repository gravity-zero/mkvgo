package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func u16p(v uint16) *uint16 { return &v }

// TestRoundTripCodecDelay checks that an audio track's gapless priming survives a
// MKV -> MP4 -> MKV round trip: the MKV CodecDelay is written as an MP4 edit list
// (elst) and read back as CodecDelay. Before this, the priming was dropped on the
// MP4 side and the decoded audio drifted by ~10-15 ms each round trip.
func TestRoundTripCodecDelay(t *testing.T) {
	const delayNs = 21_000_000 // 21 ms (a typical AAC encoder delay)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4},
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000), CodecDelay: delayNs},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{2, 3}},
		{track: 1, pts: 40, key: false, data: []byte{4}},
		{track: 2, pts: 21, key: true, data: []byte{5, 6}},
	}
	srcMKV := buildMKV(t, tracks, blocks)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	if b, _ := os.ReadFile(mp4Path); !bytes.Contains(b, []byte("elst")) {
		t.Error("RemuxToMP4 should emit an elst edit list for the audio CodecDelay")
	}

	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	c, _ := readMKV(t, outMKV)
	var audio *mkv.Track
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.AudioTrack {
			audio = &c.Tracks[i]
		}
	}
	if audio == nil {
		t.Fatal("round trip lost the audio track")
	}
	if d := audio.CodecDelay; d < 20_000_000 || d > 22_000_000 {
		t.Errorf("CodecDelay = %d ns, want ~21 ms preserved across the round trip", d)
	}
}

// TestRoundTripOpusNoDerivedCodecDelay guards the Opus regression: Opus carries
// its pre-skip in the OpusHead (codec-private, copied verbatim), so the pipeline
// must NOT add a second delay via an MP4 edit list / Matroska CodecDelay — doing
// so double-counts the pre-skip and shifts the decoded audio.
func TestRoundTripOpusNoDerivedCodecDelay(t *testing.T) {
	opusHead := makeOpusHead(2, 312, 48000, 0, 0, nil) // pre-skip 312 samples, in the OpusHead
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead,
			Channels: u8p(2), SampleRate: f64p(48000), CodecDelay: 6_500_000}, // even with a CodecDelay set
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1, 2}},
		{track: 1, pts: 20, key: true, data: []byte{3, 4}},
	}
	srcMKV := buildMKV(t, tracks, blocks)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	if b, _ := os.ReadFile(mp4Path); bytes.Contains(b, []byte("elst")) {
		t.Error("RemuxToMP4 must not emit an edit list for Opus (pre-skip already in dOps)")
	}

	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	c, _ := readMKV(t, outMKV)
	for i := range c.Tracks {
		if tr := c.Tracks[i]; tr.Type == mkv.AudioTrack && tr.CodecDelay != 0 {
			t.Errorf("Opus acquired CodecDelay=%d ns on round trip; its pre-skip would be double-counted", tr.CodecDelay)
		}
	}
}

// TestRoundTripColourAndSubtitles checks that colour code points and SRT cues
// survive the full MKV → MP4 → MKV round trip (exercising the colr box and the
// tx3g text track in both directions, plus the MKV writer's Colour element and
// BlockGroup/BlockDuration support).
func TestRoundTripColourAndSubtitles(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{0x01, 0x02, 0x03, 0x04},
			Width: u32p(2880), Height: u32p(2160), DisplayWidth: u32p(3840), DisplayHeight: u32p(2160), FrameRate: f64p(25),
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

	// The anamorphic display ASPECT must survive (via the pasp box). MP4 carries
	// only the ratio, not literal display pixels, so the DAR is the invariant.
	if video.DisplayWidth == nil || video.DisplayAspectRatio() != "16:9" {
		t.Errorf("display aspect not round-tripped: dims=%v/%v DAR=%q",
			video.DisplayWidth, video.DisplayHeight, video.DisplayAspectRatio())
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
