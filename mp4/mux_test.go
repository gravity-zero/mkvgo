package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// --- synthetic input fixtures -------------------------------------------------

// minimal, content-irrelevant codec private blobs (mkvgo only copies them).
var (
	fakeAVCC = []byte{0x01, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}
	fakeASC  = []byte{0x12, 0x10}             // AAC-LC, 44100, stereo
	fakeAV1C = []byte{0x81, 0x04, 0x0C, 0x00} // AV1CodecConfigurationRecord stub (copied verbatim)
)

func u32p(v uint32) *uint32   { return &v }
func u8p(v uint8) *uint8      { return &v }
func f64p(v float64) *float64 { return &v }

type genBlock struct {
	track uint64
	pts   int64
	key   bool
	data  []byte
}

// buildMKV writes a seekable .mkv file (known-size clusters, SeekHead + Cues,
// like a real muxer) from the given tracks and blocks. Blocks must be supplied
// in non-decreasing timecode order; they are grouped into ~1s clusters.
func buildMKV(t testing.TB, tracks []mkv.Track, blocks []genBlock) string {
	return buildMKVWithChapters(t, tracks, blocks, nil)
}

// writeTestClusters groups blocks into ~1s clusters so block offsets stay
// within a SimpleBlock's int16 range regardless of the total duration.
func writeTestClusters(m *writer.MKVWriter, scale int64, blks []mkv.Block) error {
	start := 0
	for i := 1; i <= len(blks); i++ {
		if i == len(blks) || blks[i].Timecode-blks[start].Timecode >= 1000 {
			if err := m.WriteClusterWithCues(blks[start].Timecode, scale, blks[start:i]); err != nil {
				return err
			}
			start = i
		}
	}
	return nil
}

// buildMKVTitled is buildMKV with a container title (Info.Title), for exercising the
// title metadata mapping.
func buildMKVTitled(t testing.TB, title string, tracks []mkv.Track, blocks []genBlock) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	var blks []mkv.Block
	var durationMs int64
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
		if gb.pts > durationMs {
			durationMs = gb.pts
		}
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, Title: title, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, durationMs); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if len(blks) > 0 {
		if err := writeTestClusters(m, scale, blks); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildMKVWithChapters(t testing.TB, tracks []mkv.Track, blocks []genBlock, chapters []mkv.Chapter) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000

	var blks []mkv.Block
	var durationMs int64
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
		if gb.pts > durationMs {
			durationMs = gb.pts
		}
	}

	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}, Chapters: chapters}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, durationMs); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if len(blks) > 0 {
		if err := writeTestClusters(m, scale, blks); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// remux runs RemuxToMP4 and returns the output file bytes plus its top-level boxes.
func remux(t *testing.T, srcPath string) ([]byte, []tbox) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcPath, dst); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return data, walkBoxes(t, data, 0)
}

func moovTraks(t *testing.T, boxes []tbox) []parsedTrack {
	t.Helper()
	moov := mustBox(t, boxes, "moov")
	moovBoxes := walkBoxes(t, moov.payload, moov.dataOff)
	var out []parsedTrack
	for _, b := range moovBoxes {
		if b.typ == "trak" {
			out = append(out, walkTrak(t, b))
		}
	}
	return out
}

// --- tests --------------------------------------------------------------------

func TestRemuxToMP4VideoAudio(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(1920), Height: u32p(1080), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}

	var blocks []genBlock
	wantVideo := [][]byte{}
	wantAudio := [][]byte{}
	// 4 video frames at 40ms (only first is a keyframe), 8 audio frames at 20ms.
	vIdx, aIdx := 0, 0
	for ts := int64(0); ts < 160; ts += 20 {
		if ts%40 == 0 {
			d := bytes.Repeat([]byte{byte('V'), byte(vIdx)}, 6+vIdx)
			blocks = append(blocks, genBlock{track: 1, pts: ts, key: vIdx == 0, data: d})
			wantVideo = append(wantVideo, d)
			vIdx++
		}
		d := bytes.Repeat([]byte{byte('A'), byte(aIdx)}, 4+aIdx)
		blocks = append(blocks, genBlock{track: 2, pts: ts, key: true, data: d})
		wantAudio = append(wantAudio, d)
		aIdx++
	}

	src := buildMKV(t, tracks, blocks)
	data, boxes := remux(t, src)

	if _, ok := findBox(boxes, "ftyp"); !ok {
		t.Error("no ftyp box")
	}
	if _, ok := findBox(boxes, "mdat"); !ok {
		t.Error("no mdat box")
	}

	traks := moovTraks(t, boxes)
	if len(traks) != 2 {
		t.Fatalf("got %d traks, want 2", len(traks))
	}

	var video, audio parsedTrack
	for _, pt := range traks {
		switch pt.handler {
		case "vide":
			video = pt
		case "soun":
			audio = pt
		}
	}
	if video.sampleEntry != "avc1" {
		t.Errorf("video entry = %q, want avc1", video.sampleEntry)
	}
	if audio.sampleEntry != "mp4a" {
		t.Errorf("audio entry = %q, want mp4a", audio.sampleEntry)
	}

	// No B-frames → no ctts on either track.
	if video.cttsVersion != -1 {
		t.Errorf("unexpected ctts on video (no reordering)")
	}
	// Video: only first sample is sync.
	if len(video.syncSamples) != 1 || video.syncSamples[0] != 1 {
		t.Errorf("video sync samples = %v, want [1]", video.syncSamples)
	}
	// Audio: all sync → stss omitted (nil).
	if audio.syncSamples != nil {
		t.Errorf("audio stss should be omitted, got %v", audio.syncSamples)
	}

	// Sample bytes must round-trip verbatim, in order.
	gotVideo := extractSamples(t, data, video)
	assertSamplesEqual(t, "video", gotVideo, wantVideo)
	gotAudio := extractSamples(t, data, audio)
	assertSamplesEqual(t, "audio", gotAudio, wantAudio)

	// Video durations: 40ms each (frame rate 25 → last sample 40 too).
	for i, d := range video.durations {
		if d != 40 {
			t.Errorf("video duration[%d] = %d, want 40", i, d)
		}
	}
}

func TestRemuxToMP4PreservesBFrameOrder(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
	}
	// Decode order with reordered presentation timestamps (B-frames).
	ptsOrder := []int64{0, 120, 40, 80}
	var blocks []genBlock
	var want [][]byte
	for i, pts := range ptsOrder {
		d := bytes.Repeat([]byte{0xB, byte(i)}, 8)
		blocks = append(blocks, genBlock{track: 1, pts: pts, key: i == 0, data: d})
		want = append(want, d)
	}

	src := buildMKV(t, tracks, blocks)
	data, boxes := remux(t, src)
	traks := moovTraks(t, boxes)
	if len(traks) != 1 {
		t.Fatalf("got %d traks, want 1", len(traks))
	}
	v := traks[0]

	if v.cttsVersion != 1 {
		t.Fatalf("ctts version = %d, want 1 (signed)", v.cttsVersion)
	}
	wantCTTS := []int32{0, 80, -40, -40}
	if len(v.cttsOffsets) != len(wantCTTS) {
		t.Fatalf("ctts len = %d, want %d", len(v.cttsOffsets), len(wantCTTS))
	}
	for i := range wantCTTS {
		if v.cttsOffsets[i] != wantCTTS[i] {
			t.Errorf("ctts[%d] = %d, want %d", i, v.cttsOffsets[i], wantCTTS[i])
		}
	}
	// DTS deltas are uniform 40ms.
	for i, d := range v.durations {
		if d != 40 {
			t.Errorf("duration[%d] = %d, want 40", i, d)
		}
	}
	// Samples remain in decode (file) order.
	got := extractSamples(t, data, v)
	assertSamplesEqual(t, "video", got, want)
}

func TestRemuxToMP4Opus(t *testing.T) {
	head := makeOpusHead(2, 312, 48000, 0, 0, nil)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: head,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	for ts := int64(0); ts < 100; ts += 20 {
		blocks = append(blocks, genBlock{track: 1, pts: ts, key: true, data: []byte{0x01, 0x02, byte(ts)}})
	}
	src := buildMKV(t, tracks, blocks)
	_, boxes := remux(t, src)
	traks := moovTraks(t, boxes)
	if len(traks) != 1 || traks[0].sampleEntry != "Opus" {
		t.Fatalf("expected single Opus track, got %v", traks)
	}
}

// TestRemuxWebMAV1OpusToMP4 covers the WebM → MP4 case: AV1 video + Opus audio
// is the codec subset WebM and MP4 share. RemuxToMP4 reads any EBML container
// (MKV or WebM) through the same path, so such a source remuxes to MP4 with
// av01 + Opus sample entries and verbatim samples.
func TestRemuxWebMAV1OpusToMP4(t *testing.T) {
	head := makeOpusHead(2, 312, 48000, 0, 0, nil)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: fakeAV1C,
			Width: u32p(640), Height: u32p(360), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: head,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	var wantVideo, wantAudio [][]byte
	vIdx, aIdx := 0, 0
	for ts := int64(0); ts < 160; ts += 20 {
		if ts%40 == 0 {
			d := bytes.Repeat([]byte{0xA1, byte(vIdx)}, 5+vIdx)
			blocks = append(blocks, genBlock{track: 1, pts: ts, key: vIdx == 0, data: d})
			wantVideo = append(wantVideo, d)
			vIdx++
		}
		d := bytes.Repeat([]byte{0x0F, byte(aIdx)}, 3+aIdx)
		blocks = append(blocks, genBlock{track: 2, pts: ts, key: true, data: d})
		wantAudio = append(wantAudio, d)
		aIdx++
	}

	src := buildMKV(t, tracks, blocks)
	data, boxes := remux(t, src)

	traks := moovTraks(t, boxes)
	if len(traks) != 2 {
		t.Fatalf("got %d traks, want 2", len(traks))
	}
	var video, audio parsedTrack
	for _, pt := range traks {
		switch pt.handler {
		case "vide":
			video = pt
		case "soun":
			audio = pt
		}
	}
	if video.sampleEntry != "av01" {
		t.Errorf("video entry = %q, want av01", video.sampleEntry)
	}
	if audio.sampleEntry != "Opus" {
		t.Errorf("audio entry = %q, want Opus", audio.sampleEntry)
	}
	assertSamplesEqual(t, "video", extractSamples(t, data, video), wantVideo)
	assertSamplesEqual(t, "audio", extractSamples(t, data, audio), wantAudio)
}

func TestRemuxToMP4RejectsUnsupportedCodec(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "ac3", Channels: u8p(6), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{0x0B, 0x77}}}
	src := buildMKV(t, tracks, blocks)
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err == nil {
		t.Fatal("expected error for unsupported codec ac3")
	}
}

func TestRemuxToMP4DropsBitmapSubtitles(t *testing.T) {
	// PGS (bitmap) subtitles cannot become MP4 timed text → dropped.
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "pgs"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1, 2, 3}},
		{track: 2, pts: 0, key: true, data: []byte{0x14, 0x00}},
		{track: 1, pts: 40, key: false, data: []byte{4, 5, 6}},
	}
	src := buildMKV(t, tracks, blocks)
	_, boxes := remux(t, src)
	traks := moovTraks(t, boxes)
	if len(traks) != 1 || traks[0].handler != "vide" {
		t.Fatalf("expected 1 video trak (bitmap subtitle dropped), got %d", len(traks))
	}
}

func TestRemuxToMP4NoCompatibleTracks(t *testing.T) {
	tracks := []mkv.Track{{ID: 1, Type: mkv.SubtitleTrack, Codec: "pgs"}}
	blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte("x")}}
	src := buildMKV(t, tracks, blocks)
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err == nil {
		t.Fatal("expected error when no MP4-compatible tracks exist")
	}
}

func TestRemuxToMP4SkipsEmptyTrack(t *testing.T) {
	// A declared video track with no blocks must be dropped, not crash.
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{
		{track: 2, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 20, key: true, data: []byte{2}},
	}
	src := buildMKV(t, tracks, blocks)
	_, boxes := remux(t, src)
	traks := moovTraks(t, boxes)
	if len(traks) != 1 || traks[0].handler != "soun" {
		t.Fatalf("expected only the audio trak, got %d traks", len(traks))
	}
}

func TestRemuxToMP4MdatSizeConsistent(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	var blocks []genBlock
	for i := 0; i < 10; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i * 40), key: i%5 == 0, data: bytes.Repeat([]byte{byte(i)}, 100)})
	}
	src := buildMKV(t, tracks, blocks)
	data, boxes := remux(t, src)

	// walkBoxes already asserts every box size fits the file exactly; additionally
	// verify the three top-level boxes tile the whole file with no gap/overlap.
	var covered int64
	for _, b := range boxes {
		// header length: distinguish 32 vs 64-bit by re-reading.
		hdr := int64(8)
		if b.typ == "mdat" {
			hdr = 16 // we always write mdat with the 64-bit largesize form
		}
		covered += hdr + int64(len(b.payload))
	}
	if covered != int64(len(data)) {
		t.Errorf("boxes cover %d bytes, file is %d", covered, len(data))
	}
}

func assertSamplesEqual(t *testing.T, label string, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d samples, want %d", label, len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s sample %d mismatch:\n got % x\nwant % x", label, i, got[i], want[i])
		}
	}
}
