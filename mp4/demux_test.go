package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// readMKV reads a Matroska file's tracks and all its blocks (in file order).
func readMKV(t *testing.T, path string) (*mkv.Container, []mkv.Block) {
	t.Helper()
	c, err := reader.OpenWithFS(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("read mkv: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []mkv.Block
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read block: %v", err)
		}
		blocks = append(blocks, b)
	}
	return c, blocks
}

func blocksByTrack(blocks []mkv.Block) map[uint64][]mkv.Block {
	m := map[uint64][]mkv.Block{}
	for _, b := range blocks {
		m[b.TrackNumber] = append(m[b.TrackNumber], b)
	}
	return m
}

func TestRoundTripMKVMP4MKV(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(1280), Height: u32p(720), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC,
			Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	vIdx, aIdx := 0, 0
	for ts := int64(0); ts < 200; ts += 20 {
		if ts%40 == 0 {
			blocks = append(blocks, genBlock{track: 1, pts: ts, key: vIdx == 0, data: bytes.Repeat([]byte{byte('V'), byte(vIdx)}, 5+vIdx)})
			vIdx++
		}
		blocks = append(blocks, genBlock{track: 2, pts: ts, key: true, data: bytes.Repeat([]byte{byte('A'), byte(aIdx)}, 3+aIdx)})
		aIdx++
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

	// Tracks: codecs and codec private round-trip verbatim.
	if len(c.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(c.Tracks))
	}
	byType := map[mkv.TrackType]mkv.Track{}
	for _, tr := range c.Tracks {
		byType[tr.Type] = tr
	}
	if v := byType[mkv.VideoTrack]; v.Codec != "h264" || !bytes.Equal(v.CodecPrivate, fakeAVCC) {
		t.Errorf("video track wrong: codec=%q private=% x", v.Codec, v.CodecPrivate)
	}
	if a := byType[mkv.AudioTrack]; a.Codec != "aac" || !bytes.Equal(a.CodecPrivate, fakeASC) {
		t.Errorf("audio track wrong: codec=%q private=% x", a.Codec, a.CodecPrivate)
	}

	// Build expected per-track blocks from the input.
	wantByTrack := map[uint64][]genBlock{}
	for _, gb := range blocks {
		wantByTrack[gb.track] = append(wantByTrack[gb.track], gb)
	}

	gotByTrack := blocksByTrack(gotBlocks)
	for track, want := range wantByTrack {
		got := gotByTrack[track]
		if len(got) != len(want) {
			t.Fatalf("track %d: got %d blocks, want %d", track, len(got), len(want))
		}
		// Compare ordered by timecode (decode order is preserved, but sort to be
		// robust to interleaving differences across tracks).
		sort.SliceStable(got, func(i, j int) bool { return got[i].Timecode < got[j].Timecode })
		sort.SliceStable(want, func(i, j int) bool { return want[i].pts < want[j].pts })
		for i := range want {
			if got[i].Timecode != want[i].pts {
				t.Errorf("track %d block %d: timecode %d, want %d", track, i, got[i].Timecode, want[i].pts)
			}
			if got[i].Keyframe != want[i].key {
				t.Errorf("track %d block %d: keyframe %v, want %v", track, i, got[i].Keyframe, want[i].key)
			}
			if !bytes.Equal(got[i].Data, want[i].data) {
				t.Errorf("track %d block %d: data mismatch", track, i)
			}
		}
	}
}

func TestRoundTripPreservesBFramePTS(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
	}
	ptsOrder := []int64{0, 120, 40, 80, 200, 320, 240, 280}
	var blocks []genBlock
	for i, pts := range ptsOrder {
		blocks = append(blocks, genBlock{track: 1, pts: pts, key: i == 0 || i == 4, data: bytes.Repeat([]byte{byte(i)}, 7)})
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatal(err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatal(err)
	}

	_, gotBlocks := readMKV(t, outMKV)
	if len(gotBlocks) != len(ptsOrder) {
		t.Fatalf("got %d blocks, want %d", len(gotBlocks), len(ptsOrder))
	}
	// Every original PTS must survive the round trip (as block timecodes).
	gotPTS := map[int64]bool{}
	for _, b := range gotBlocks {
		gotPTS[b.Timecode] = true
	}
	for _, pts := range ptsOrder {
		if !gotPTS[pts] {
			t.Errorf("PTS %d lost in round trip", pts)
		}
	}
}

func TestRoundTripOpus(t *testing.T) {
	head := makeOpusHead(2, 312, 48000, 0, 0, nil)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: head, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	for ts := int64(0); ts < 100; ts += 20 {
		blocks = append(blocks, genBlock{track: 1, pts: ts, key: true, data: []byte{0xAA, byte(ts)}})
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatal(err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatal(err)
	}
	c, blks := readMKV(t, outMKV)
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "opus" {
		t.Fatalf("expected single opus track, got %v", c.Tracks)
	}
	// OpusHead must round-trip exactly (LE↔BE conversion through dOps).
	if !bytes.Equal(c.Tracks[0].CodecPrivate, head) {
		t.Errorf("OpusHead mismatch:\n got % x\nwant % x", c.Tracks[0].CodecPrivate, head)
	}
	if len(blks) != 5 {
		t.Errorf("got %d opus blocks, want 5", len(blks))
	}
}

func TestParseESDSRoundTrip(t *testing.T) {
	for _, asc := range [][]byte{{0x12, 0x10}, {0x11, 0x90, 0x56, 0xE5, 0x00}} {
		// esdsBox returns a full box; the parser receives the payload (as childConfig
		// would hand it), so strip the 8-byte box header.
		objType, got, err := parseESDS(esdsBox(0x40, asc)[8:])
		if err != nil {
			t.Fatalf("parseESDS: %v", err)
		}
		if objType != 0x40 {
			t.Errorf("objType = %#x, want 0x40 (AAC)", objType)
		}
		if !bytes.Equal(got, asc) {
			t.Errorf("asc round trip: got % x, want % x", got, asc)
		}
	}
}

func TestOpusHeadFromDOpsRoundTrip(t *testing.T) {
	heads := [][]byte{
		makeOpusHead(2, 312, 48000, 0, 0, nil),
		makeOpusHead(6, 120, 48000, 0, 1, []byte{0x04, 0x02, 0, 1, 2, 3, 4, 5}),
	}
	for _, head := range heads {
		dops, err := dOpsBox(head)
		if err != nil {
			t.Fatal(err)
		}
		// dOpsBox returns a full box; strip the 8-byte header for the parser.
		got, err := opusHeadFromDOps(dops[8:])
		if err != nil {
			t.Fatalf("opusHeadFromDOps: %v", err)
		}
		if !bytes.Equal(got, head) {
			t.Errorf("OpusHead round trip:\n got % x\nwant % x", got, head)
		}
	}
}

func TestRemuxFromMP4NoMoov(t *testing.T) {
	// A file with a valid ftyp but no moov must error, not panic.
	data := buildFtyp([]string{"avc1"})
	src := filepath.Join(t.TempDir(), "bad.mp4")
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), src, dst); err == nil {
		t.Fatal("expected error for MP4 without moov")
	}
}

func TestParseMP4RejectsTruncatedBox(t *testing.T) {
	// Box claims a size larger than the data.
	bad := []byte{0x00, 0x00, 0x10, 0x00, 'm', 'o', 'o', 'v'}
	if _, err := parseMP4(bytes.NewReader(bad), int64(len(bad)), true); err == nil {
		t.Error("expected error for oversized box")
	}
}
