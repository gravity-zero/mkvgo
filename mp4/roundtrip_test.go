package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// audioEditList walks an MP4's boxes and returns the first audio (soun) track's
// mdhd timescale, edit-list media_time and empty-edit offset (all 0 if no edit
// list). It is how the tests assert the priming layout (media_time) and the A/V
// presentation offset (the empty edit).
func audioEditList(t *testing.T, data []byte) (timescale uint32, mediaTime, offset int64) {
	t.Helper()
	top, err := iterBoxes(data)
	if err != nil {
		t.Fatalf("iterBoxes: %v", err)
	}
	moov, ok := findMemBox(top, "moov")
	if !ok {
		t.Fatal("no moov")
	}
	moovBoxes, _ := iterBoxes(moov.payload)
	for _, b := range moovBoxes {
		if b.typ != "trak" {
			continue
		}
		trakBoxes, _ := iterBoxes(b.payload)
		mdia, ok := findMemBox(trakBoxes, "mdia")
		if !ok {
			continue
		}
		mdiaBoxes, _ := iterBoxes(mdia.payload)
		if hdlr, ok := findMemBox(mdiaBoxes, "hdlr"); !ok || len(hdlr.payload) < 12 || string(hdlr.payload[8:12]) != "soun" {
			continue
		}
		mdhd, ok := findMemBox(mdiaBoxes, "mdhd")
		if !ok || len(mdhd.payload) < 16 {
			t.Fatal("audio track without a readable mdhd")
		}
		timescale = binary.BigEndian.Uint32(mdhd.payload[12:16]) // v0: after version/flags + 2 times
		if edts, ok := findMemBox(trakBoxes, "edts"); ok {
			eb, _ := iterBoxes(edts.payload)
			if elst, ok := findMemBox(eb, "elst"); ok {
				mediaTime, offset, _ = parseElst(elst.payload)
			}
		}
		return timescale, mediaTime, offset
	}
	t.Fatal("no audio track")
	return 0, 0, 0
}

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

// TestRoundTripTitleAndTrackName checks that the container title and a per-track
// name reach the MP4 (title -> moov/udta/meta/ilst/©nam, name -> trak/udta/name, the
// way mainstream muxers write them) and survive the full MKV -> MP4 -> MKV round trip.
func TestRoundTripTitleAndTrackName(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4},
			Width: u32p(64), Height: u32p(64), FrameRate: f64p(25), Name: "Piste Vidéo VF"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 1, pts: 40, key: false, data: []byte{2}},
	}
	srcMKV := buildMKVTitled(t, "Mon Titre", tracks, blocks)

	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	data, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("\xa9nam")) || !bytes.Contains(data, []byte("Mon Titre")) {
		t.Error("MP4 is missing the movie title (moov/udta/meta/ilst/©nam)")
	}
	if !bytes.Contains(data, []byte("namePiste Vidéo VF")) {
		t.Error("MP4 is missing the track name box (trak/udta/name)")
	}

	outMKV := filepath.Join(t.TempDir(), "back.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	c, _ := readMKV(t, outMKV)
	if c.Info.Title != "Mon Titre" {
		t.Errorf("Info.Title = %q, want %q", c.Info.Title, "Mon Titre")
	}
	if len(c.Tracks) == 0 || c.Tracks[0].Name != "Piste Vidéo VF" {
		t.Errorf("track name lost on round trip: %q", trackName(c))
	}
}

// TestBuildMovieMetaTags checks the movie metadata box carries the title and the
// mapped global tags (ARTIST -> ©ART, ALBUM -> ©alb, ...) and skips unmapped ones.
func TestBuildMovieMetaTags(t *testing.T) {
	meta := buildMovieMeta("Mon Titre", []mkv.SimpleTag{
		{Name: "ARTIST", Value: "Studio"},
		{Name: "ALBUM", Value: "Coll"},
		{Name: "WEIRD_TAG", Value: "ZZUNMAPPEDZZ"},
		{Name: "ARTIST", Value: "Dup"}, // duplicate name → first wins
	}, nil, nil)
	if meta == nil {
		t.Fatal("expected a meta box")
	}
	for _, want := range []string{"\xa9nam", "Mon Titre", "\xa9ART", "Studio", "\xa9alb", "Coll"} {
		if !bytes.Contains(meta, []byte(want)) {
			t.Errorf("meta box missing %q", want)
		}
	}
	if bytes.Contains(meta, []byte("ZZUNMAPPEDZZ")) {
		t.Error("unmapped tag value should not be written")
	}
	if bytes.Contains(meta, []byte("Dup")) {
		t.Error("duplicate atom should keep the first value")
	}
	if buildMovieMeta("", nil, nil, nil) != nil {
		t.Error("no title, no tags and no cover should yield no meta box")
	}
}

func trackName(c *mkv.Container) string {
	if len(c.Tracks) == 0 {
		return ""
	}
	return c.Tracks[0].Name
}

// TestEditListSampleExact locks in the layout that makes the priming round trip
// sample-exact for every audio codec: audio tracks are written on a sample-rate
// media timescale, and the CodecDelay becomes an edit list whose media_time is the
// exact priming in samples. Mainstream demuxers only trim a codec's delay (notably AC-3) from
// such a sample-exact edit list - a millisecond-quantised one is ignored/padded.
func TestEditListSampleExact(t *testing.T) {
	const rate = 48000
	const primingSamples = 1024
	codecDelayNs := int64(primingSamples) * 1_000_000_000 / rate
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(rate), CodecDelay: codecDelayNs},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1, 2}},
		{track: 1, pts: 21, key: true, data: []byte{3, 4}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	data, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	ts, mediaTime, _ := audioEditList(t, data)
	if ts != rate {
		t.Errorf("audio mdhd timescale = %d, want %d (sample rate)", ts, rate)
	}
	if mediaTime != primingSamples {
		t.Errorf("edit-list media_time = %d, want %d samples (sample-exact)", mediaTime, primingSamples)
	}
}

// TestRoundTripAVOffset checks that a per-track presentation offset (the A/V sync
// gap mainstream muxers write as an empty edit) survives MKV -> MP4: the audio track starts
// 476 ms after the video, and that gap must be re-emitted as an empty edit, not
// rebased to 0 (which silently desyncs the audio).
func TestRoundTripAVOffset(t *testing.T) {
	const offsetMs = 476
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4},
			Width: u32p(64), Height: u32p(64), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 1, pts: 40, key: false, data: []byte{2}},
		{track: 2, pts: offsetMs, key: true, data: []byte{3, 4}},
		{track: 2, pts: offsetMs + 21, key: true, data: []byte{5, 6}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	data, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, offset := audioEditList(t, data); offset != offsetMs {
		t.Errorf("audio empty-edit offset = %d ms, want %d (A/V sync lost)", offset, offsetMs)
	}
}

// TestMP3NoDerivedEditList guards the native-MKV MP3 head-desync fix: MP3's encoder
// delay lives in its in-band Xing/LAME header, which decoders apply anyway,
// so a derived MP4 edit list over-trims and desyncs the head (a native MKV MP3 lost
// ~20 ms of real audio). No edit list must be written for MP3.
func TestMP3NoDerivedEditList(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "A_MPEG/L3",
			Channels: u8p(2), SampleRate: f64p(44100), CodecDelay: 24_000_000},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0xFF, 0xFB, 0x90, 0x00}},
		{track: 1, pts: 26, key: true, data: []byte{0xFF, 0xFB, 0x90, 0x00}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	if b, _ := os.ReadFile(mp4Path); bytes.Contains(b, []byte("elst")) {
		t.Error("MP4 must not emit an edit list for MP3 (delay is in the in-band LAME header)")
	}
}

// TestMP3ContainerDelayOptIn checks the opt-in flag carries an MP3 delay as an edit
// list (for recovering a round trip of an MP3 that originated in an MP4).
func TestMP3ContainerDelayOptIn(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "A_MPEG/L3",
			Channels: u8p(2), SampleRate: f64p(44100), CodecDelay: 24_000_000},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0xFF, 0xFB, 0x90, 0x00}},
		{track: 1, pts: 26, key: true, data: []byte{0xFF, 0xFB, 0x90, 0x00}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path, Options{MP3ContainerDelay: true}); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	if b, _ := os.ReadFile(mp4Path); !bytes.Contains(b, []byte("elst")) {
		t.Error("with MP3ContainerDelay the MP4 should carry an MP3 edit list")
	}
}

// TestRoundTripOpusPrimingEditList: Opus priming (Matroska CodecDelay = pre-skip)
// becomes an edit list whose media_time is the pre-skip, as Opus-in-ISOBMFF 4.4
// requires (dOps PreSkip is informative only), on the progressive MP4 and the HLS
// init alike, and comes back as CodecDelay. Without it the audio played 6.5 ms late.
func TestRoundTripOpusPrimingEditList(t *testing.T) {
	const preSkip = 312
	opusHead := makeOpusHead(2, preSkip, 48000, 0, 0, nil)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4},
			Width: u32p(64), Height: u32p(64), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead,
			Channels: u8p(2), SampleRate: f64p(48000), CodecDelay: 6_500_000},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{1, 2}},
		{track: 2, pts: 20, key: true, data: []byte{3, 4}},
		{track: 1, pts: 40, key: false, data: []byte{2}},
	}
	srcMKV := buildMKV(t, tracks, blocks)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	data, _ := os.ReadFile(mp4Path)
	if _, mt, _ := audioEditList(t, data); mt != preSkip {
		t.Errorf("RemuxToMP4 Opus edit-list media_time = %d, want the pre-skip %d", mt, preSkip)
	}

	hlsDir := t.TempDir()
	if err := RemuxToHLS(context.Background(), srcMKV, hlsDir, Options{SegmentMs: 2000}); err != nil {
		t.Fatalf("RemuxToHLS: %v", err)
	}
	init, _ := os.ReadFile(filepath.Join(hlsDir, "init_a1.mp4"))
	if _, mt, _ := audioEditList(t, init); mt != preSkip {
		t.Errorf("HLS Opus init edit-list media_time = %d, want the pre-skip %d", mt, preSkip)
	}

	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	c, _ := readMKV(t, outMKV)
	for i := range c.Tracks {
		if tr := c.Tracks[i]; tr.Type == mkv.AudioTrack && tr.CodecDelay != 6_500_000 {
			t.Errorf("Opus CodecDelay after the round trip = %d ns, want 6500000", tr.CodecDelay)
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

// TestRoundTripRotation checks that a clockwise Track.Rotation, mirrored or not,
// becomes the tkhd display matrix ffmpeg writes for it and survives MKV -> MP4 -> MKV.
func TestRoundTripRotation(t *testing.T) {
	// tkhd a,b,c,d (16.16), as ffmpeg writes them for -display_rotation -rot and -display_hflip.
	cases := []struct {
		rot   int
		hflip bool
		m     [4]uint32
	}{
		{90, false, [4]uint32{0, 0x00010000, 0xffff0000, 0}},
		{180, false, [4]uint32{0xffff0000, 0, 0, 0xffff0000}},
		{270, false, [4]uint32{0, 0xffff0000, 0x00010000, 0}},
		{0, true, [4]uint32{0xffff0000, 0, 0, 0x00010000}},
		{90, true, [4]uint32{0, 0x00010000, 0x00010000, 0}},
		{180, true, [4]uint32{0x00010000, 0, 0, 0xffff0000}},
		{270, true, [4]uint32{0, 0xffff0000, 0xffff0000, 0}},
	}
	for _, tc := range cases {
		rot, m := tc.rot, tc.m
		tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: []byte{0x01, 0x64, 0x00, 0x1f},
			Width: u32p(1280), Height: u32p(720), FrameRate: f64p(25), Rotation: rot, HFlip: tc.hflip}}
		blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{1}}, {track: 1, pts: 40, data: []byte{2}}}
		mp4Path := filepath.Join(t.TempDir(), "rot.mp4")
		if err := RemuxToMP4(context.Background(), buildMKV(t, tracks, blocks), mp4Path); err != nil {
			t.Fatalf("RemuxToMP4: %v", err)
		}
		data, err := os.ReadFile(mp4Path)
		if err != nil {
			t.Fatal(err)
		}
		i := bytes.Index(data, []byte("tkhd"))
		if i < 0 {
			t.Fatal("no tkhd")
		}
		mat := data[i+4+40:]
		got := [4]uint32{binary.BigEndian.Uint32(mat), binary.BigEndian.Uint32(mat[4:]), binary.BigEndian.Uint32(mat[12:]), binary.BigEndian.Uint32(mat[16:])}
		if got != m {
			t.Errorf("rotation %d hflip %v: tkhd matrix a,b,c,d = %08x, want %08x", rot, tc.hflip, got, m)
		}
		c, _, err := OpenMeta(context.Background(), mp4Path)
		if err != nil {
			t.Fatal(err)
		}
		if tr := c.Tracks[0]; tr.Rotation != rot || tr.HFlip != tc.hflip {
			t.Errorf("rotation %d hflip %v: MP4 reads back %d %v", rot, tc.hflip, tr.Rotation, tr.HFlip)
		}
		outMKV := filepath.Join(t.TempDir(), "out.mkv")
		if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
			t.Fatalf("RemuxFromMP4: %v", err)
		}
		if back, _ := readMKV(t, outMKV); back.Tracks[0].Rotation != rot || back.Tracks[0].HFlip != tc.hflip {
			t.Errorf("rotation %d hflip %v: MKV after the round trip reads %d %v", rot, tc.hflip, back.Tracks[0].Rotation, back.Tracks[0].HFlip)
		}
	}
}

// TestEditListBoundsDuration: an edit list that ends the presentation before
// the media (an audio track cut at 6 s over 10 s of samples) sets the track's
// and the file's duration, in the probe and in the Segment from-mp4 writes.
func TestEditListBoundsDuration(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4}, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000), CodecDelay: 21_000_000},
	}
	var blocks []genBlock
	for ms := int64(0); ms < 5000; ms += 40 {
		blocks = append(blocks, genBlock{track: 1, pts: ms, key: ms == 0, data: []byte{1}})
	}
	for ms := int64(0); ms < 10000; ms += 21 {
		blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: []byte{2}})
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].pts < blocks[j].pts })
	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), buildMKV(t, tracks, blocks), mp4Path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	patched := false
	for i := bytes.Index(b, []byte("elst")); i >= 0; i = nextIndex(b, []byte("elst"), i) {
		if b[i+4] == 0 && binary.BigEndian.Uint32(b[i+12:]) > 9000 { // v0: the audio edit, about 10 s
			binary.BigEndian.PutUint32(b[i+12:], 6000)
			patched = true
		}
	}
	if !patched {
		t.Fatal("no audio edit list to cut")
	}
	if err := os.WriteFile(mp4Path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs != 6000 || c.Tracks[1].DurationMs != 6000 {
		t.Errorf("probe: file %d ms, audio %d ms; want 6000 and 6000", c.DurationMs, c.Tracks[1].DurationMs)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatal(err)
	}
	if m, _ := readMKV(t, outMKV); m.DurationMs != 6000 {
		t.Errorf("from-mp4 Segment duration %d ms, want 6000", m.DurationMs)
	}
}

func nextIndex(b, sep []byte, after int) int {
	if j := bytes.Index(b[after+1:], sep); j >= 0 {
		return after + 1 + j
	}
	return -1
}

// TestEditListBeyondMedia: edits that present more than the media holds (a
// muxer that wrote the same full-length edit twice) end the track with its
// media, the empty edits aside.
func TestEditListBeyondMedia(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4}, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000), CodecDelay: 21_000_000},
	}
	var blocks []genBlock
	for ms := int64(0); ms < 10000; ms += 40 {
		blocks = append(blocks, genBlock{track: 1, pts: ms, key: ms == 0, data: []byte{1}})
		blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: []byte{2}})
	}
	mp4Path := filepath.Join(t.TempDir(), "twice.mp4")
	if err := RemuxToMP4(context.Background(), buildMKV(t, tracks, blocks), mp4Path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	patched := false
	for i := bytes.Index(b, []byte("elst")); i >= 0; i = nextIndex(b, []byte("elst"), i) {
		if b[i+4] == 0 && binary.BigEndian.Uint32(b[i+12:]) > 9000 { // v0: the audio edit, about 10 s
			binary.BigEndian.PutUint32(b[i+12:], 2*binary.BigEndian.Uint32(b[i+12:]))
			patched = true
		}
	}
	if !patched {
		t.Fatal("no audio edit list to stretch")
	}
	if err := os.WriteFile(mp4Path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationMs > 10100 || c.Tracks[1].DurationMs > 10100 {
		t.Errorf("probe: file %d ms, audio %d ms; want about 10000, the media's", c.DurationMs, c.Tracks[1].DurationMs)
	}
}

// TestEditListEmptyEditOutsideTkhd: an empty edit delays the track even when
// the tkhd duration leaves it out (as a mainstream muxer writes it).
func TestEditListEmptyEditOutsideTkhd(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4}, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	for ms := int64(0); ms < 10000; ms += 40 {
		blocks = append(blocks, genBlock{track: 1, pts: ms, key: ms == 0, data: []byte{1}})
		if ms+500 < 10500 {
			blocks = append(blocks, genBlock{track: 2, pts: ms + 500, key: true, data: []byte{2}})
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].pts < blocks[j].pts })
	mp4Path := filepath.Join(t.TempDir(), "late.mp4")
	if err := RemuxToMP4(context.Background(), buildMKV(t, tracks, blocks), mp4Path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	empty := int64(-1)
	for i := bytes.Index(b, []byte("elst")); i >= 0; i = nextIndex(b, []byte("elst"), i) {
		if b[i+4] == 0 && binary.BigEndian.Uint32(b[i+8:]) == 2 && int32(binary.BigEndian.Uint32(b[i+16:])) == -1 {
			empty = int64(binary.BigEndian.Uint32(b[i+12:]))
		}
	}
	if empty <= 0 {
		t.Fatal("the late track has no empty edit: the case this test guards (an empty edit outside the tkhd) is no longer built")
	}
	audioTkhd := -1
	for i := bytes.Index(b, []byte("tkhd")); i >= 0; i = nextIndex(b, []byte("tkhd"), i) {
		audioTkhd = i // the audio trak comes second
	}
	if b[audioTkhd+4] != 0 {
		t.Fatal("want a v0 tkhd")
	}
	d := binary.BigEndian.Uint32(b[audioTkhd+4+20:])
	binary.BigEndian.PutUint32(b[audioTkhd+4+20:], d-uint32(empty))
	if err := os.WriteFile(mp4Path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := OpenMeta(context.Background(), mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tracks[1].DurationMs; got < 10400 {
		t.Errorf("audio %d ms; want about 10500, the 500 ms empty edit and the media", got)
	}
}
