package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// demuxFixture is a Matroska of the given tracks, each fed n frames from frame(track, i), one cluster a second.
func demuxFixture(t *testing.T, dir string, tracks []mkv.Track, n int, frame func(id uint64, i int) ([]byte, bool)) string {
	t.Helper()
	var sets [][]mkv.Block
	for c := range (n + 9) / 10 {
		var set []mkv.Block
		for i := c * 10; i < min(n, c*10+10); i++ {
			for _, tr := range tracks {
				data, key := frame(tr.ID, i)
				set = append(set, mkv.Block{TrackNumber: tr.ID, Timecode: int64(i) * 100, Keyframe: key, Data: data})
			}
		}
		sets = append(sets, set)
	}
	return buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, int64(n)*100)
}

func demuxTo(t *testing.T, src string, raw bool) string {
	t.Helper()
	out := filepath.Join(filepath.Dir(src), "out")
	if raw {
		out += "-raw"
	}
	if err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: out, Raw: raw}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDemuxADTS: an AAC track is written as ADTS - each frame behind a 7-byte header carrying the AudioSpecificConfig's
// profile, rate and channels and the frame's length - and Raw keeps the bare frames.
func TestDemuxADTS(t *testing.T) {
	dir := t.TempDir()
	aac := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: f64(48000), Channels: u8(2)} // LC, 48 kHz, 2 ch
	src := demuxFixture(t, dir, []mkv.Track{aac}, 20, func(_ uint64, i int) ([]byte, bool) {
		return bytes.Repeat([]byte{byte(i)}, 100+i), true
	})
	out := readAll(t, filepath.Join(demuxTo(t, src, false), "1.aac"))
	for i := 0; len(out) > 0; i++ {
		if len(out) < 7 || out[0] != 0xFF || out[1] != 0xF1 {
			t.Fatalf("frame %d: no ADTS syncword: % x", i, out[:min(8, len(out))])
		}
		profile, sf, cc := out[2]>>6, out[2]>>2&0x0F, (out[2]&1)<<2|out[3]>>6
		n := int(out[3]&3)<<11 | int(out[4])<<3 | int(out[5]>>5)
		if profile != 1 || sf != 3 || cc != 2 || n != 7+100+i {
			t.Fatalf("frame %d: profile %d, rate index %d, channels %d, length %d; want 1, 3, 2, %d", i, profile, sf, cc, n, 7+100+i)
		}
		out = out[n:]
	}
	if raw := readAll(t, filepath.Join(demuxTo(t, src, true), "1.aac")); raw[0] == 0xFF {
		t.Error("Raw wrote ADTS")
	}
}

// avcRecord is an avcC with one SPS and one PPS, NAL lengths on 4 bytes.
func avcRecord(sps, pps []byte) []byte {
	b := []byte{1, 0x64, 0, 0x1F, 0xFF, 0xE1, 0, byte(len(sps))}
	b = append(b, sps...)
	b = append(b, 1, 0, byte(len(pps)))
	return append(b, pps...)
}

// TestDemuxAnnexB: an H.264 track is written as Annex-B, its SPS and PPS ahead of every keyframe, each NAL behind a
// start code.
func TestDemuxAnnexB(t *testing.T) {
	dir := t.TempDir()
	sps, pps := []byte{0x67, 0x64, 0x00, 0x1F}, []byte{0x68, 0xEE}
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcRecord(sps, pps), Width: u32(64), Height: u32(64)}
	src := demuxFixture(t, dir, []mkv.Track{video}, 20, func(_ uint64, i int) ([]byte, bool) {
		nal := []byte{0x41, byte(i)}
		if i%10 == 0 {
			nal[0] = 0x65
		}
		return append([]byte{0, 0, 0, byte(len(nal))}, nal...), i%10 == 0
	})
	out := readAll(t, filepath.Join(demuxTo(t, src, false), "1.h264"))
	params := append(append([]byte{0, 0, 0, 1}, sps...), append([]byte{0, 0, 0, 1}, pps...)...)
	var want []byte
	for i := range 20 {
		if i%10 == 0 {
			want = append(append(want, params...), 0, 0, 0, 1, 0x65, byte(i))
		} else {
			want = append(want, 0, 0, 0, 1, 0x41, byte(i))
		}
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("Annex-B output\n% x\nwant\n% x", out, want)
	}
}

// TestDemuxFLAC: a FLAC track is a FLAC file: the stream marker and the STREAMINFO ahead of the frames.
func TestDemuxFLAC(t *testing.T) {
	dir := t.TempDir()
	streaminfo := append([]byte{0x80, 0, 0, 34}, make([]byte, 34)...) // last block, STREAMINFO, 34 bytes
	flac := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "flac", CodecPrivate: append([]byte("fLaC"), streaminfo...), SampleRate: f64(48000), Channels: u8(2)}
	src := demuxFixture(t, dir, []mkv.Track{flac}, 5, func(_ uint64, i int) ([]byte, bool) { return []byte{0xFF, 0xF8, byte(i)}, true })
	out := readAll(t, filepath.Join(demuxTo(t, src, false), "1.flac"))
	want := append(append([]byte("fLaC"), streaminfo...), 0xFF, 0xF8, 0, 0xFF, 0xF8, 1, 0xFF, 0xF8, 2, 0xFF, 0xF8, 3, 0xFF, 0xF8, 4)
	if !bytes.Equal(out, want) {
		t.Fatalf("FLAC output % x, want % x", out, want)
	}
}

// TestDemuxSup: a PGS track is a .sup: each segment of each block behind "PG", its presentation time at 90 kHz and a
// zero decoding time.
func TestDemuxSup(t *testing.T) {
	dir := t.TempDir()
	pgs := mkv.Track{ID: 1, Type: mkv.SubtitleTrack, Codec: "pgs"}
	src := demuxFixture(t, dir, []mkv.Track{pgs}, 3, func(_ uint64, i int) ([]byte, bool) {
		return []byte{0x16, 0, 2, 0xAA, byte(i), 0x80, 0, 0}, true // a PCS of 2 bytes, then an END
	})
	out := readAll(t, filepath.Join(demuxTo(t, src, false), "1.sup"))
	for i := range 3 {
		for _, seg := range [][]byte{{0x16, 0, 2, 0xAA, byte(i)}, {0x80, 0, 0}} {
			if len(out) < 10+len(seg) || string(out[:2]) != "PG" || binary.BigEndian.Uint32(out[2:]) != uint32(i*100*90) ||
				binary.BigEndian.Uint32(out[6:]) != 0 || !bytes.Equal(out[10:10+len(seg)], seg) {
				t.Fatalf("block %d: segment % x, want PG, PTS %d, % x", i, out[:min(len(out), 10+len(seg))], i*100*90, seg)
			}
			out = out[10+len(seg):]
		}
	}
	if len(out) != 0 {
		t.Errorf("%d bytes past the segments", len(out))
	}
}

// oggPages splits an Ogg stream into its pages, checking each one's checksum.
func oggPages(t *testing.T, b []byte) (pages []struct {
	flags   byte
	granule int64
	body    []byte
}) {
	t.Helper()
	for len(b) > 0 {
		if len(b) < 27 || string(b[:4]) != "OggS" {
			t.Fatalf("no page at % x", b[:min(len(b), 8)])
		}
		n := int(b[26])
		size := 27 + n
		body := 0
		for _, l := range b[27 : 27+n] {
			body += int(l)
		}
		page := append([]byte{}, b[:size+body]...)
		crc := binary.LittleEndian.Uint32(page[22:])
		binary.LittleEndian.PutUint32(page[22:], 0)
		if oggCRC(page) != crc {
			t.Fatalf("page %d: checksum %08x, computed %08x", len(pages), crc, oggCRC(page))
		}
		pages = append(pages, struct {
			flags   byte
			granule int64
			body    []byte
		}{b[5], int64(binary.LittleEndian.Uint64(b[6:])), b[size : size+body]})
		b = b[size+body:]
	}
	return pages
}

// TestDemuxOggOpus: an Opus track is an Ogg Opus file: OpusHead on the first page (beginning of stream), OpusTags,
// then a packet a page whose granule counts the 48 kHz samples decoded so far, the last page ending the stream.
func TestDemuxOggOpus(t *testing.T) {
	dir := t.TempDir()
	head := []byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00") // 2 ch, pre-skip 312, 48 kHz
	opus := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: head, SampleRate: f64(48000), Channels: u8(2)}
	src := demuxFixture(t, dir, []mkv.Track{opus}, 12, func(_ uint64, i int) ([]byte, bool) {
		return []byte{0xFC, byte(i), 0, 0}, true // CELT 20 ms, one frame: 960 samples
	})
	pages := oggPages(t, readAll(t, filepath.Join(demuxTo(t, src, false), "1.opus")))
	if len(pages) != 2+12 || !bytes.Equal(pages[0].body, head) || pages[0].flags != 2 || !bytes.HasPrefix(pages[1].body, []byte("OpusTags")) {
		t.Fatalf("%d pages, first %q flags %d, second %q", len(pages), pages[0].body, pages[0].flags, pages[1].body)
	}
	for i, p := range pages[2:] {
		if p.granule != int64(960*(i+1)) {
			t.Errorf("packet %d: granule %d, want %d", i, p.granule, 960*(i+1))
		}
	}
	if last := pages[len(pages)-1]; last.flags != 4 {
		t.Errorf("last page flags %d, want end of stream", last.flags)
	}
}

// TestDemuxOggVorbis: a Vorbis track is an Ogg Vorbis file: the three headers its Xiph-laced CodecPrivate holds,
// the identification one first, then a packet a page.
func TestDemuxOggVorbis(t *testing.T) {
	dir := t.TempDir()
	ident := append([]byte("\x01vorbis\x00\x00\x00\x00\x02\x44\xac\x00\x00"), make([]byte, 14)...) // 2 ch, 44.1 kHz
	comment, setup := []byte("\x03vorbis-comment"), []byte("\x05vorbis-setup-header")
	cp := append([]byte{2, byte(len(ident)), byte(len(comment))}, ident...)
	cp = append(append(cp, comment...), setup...)
	vorbis := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "vorbis", CodecPrivate: cp, SampleRate: f64(44100), Channels: u8(2)}
	src := demuxFixture(t, dir, []mkv.Track{vorbis}, 5, func(_ uint64, i int) ([]byte, bool) { return []byte{0, byte(i)}, true })
	pages := oggPages(t, readAll(t, filepath.Join(demuxTo(t, src, false), "1.ogg")))
	if len(pages) != 3+5 || !bytes.Equal(pages[0].body, ident) || pages[0].flags != 2 ||
		!bytes.Equal(pages[1].body, comment) || !bytes.Equal(pages[2].body, setup) {
		t.Fatalf("headers: %d pages, %q / %q / %q", len(pages), pages[0].body, pages[1].body, pages[2].body)
	}
	for i, p := range pages[3:] {
		if !bytes.Equal(p.body, []byte{0, byte(i)}) {
			t.Errorf("packet %d: % x", i, p.body)
		}
	}
	if pages[len(pages)-1].flags != 4 {
		t.Error("the last page does not end the stream")
	}
}

// TestDemuxRawWithoutConfig: a track lacking what its format needs - an AAC without an AudioSpecificConfig - keeps
// its payloads, named after its codec as before.
func TestDemuxRawWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	src := demuxFixture(t, dir, []mkv.Track{audioTrack(1)}, 3, func(_ uint64, i int) ([]byte, bool) { return []byte{byte(i), 1}, true })
	out, err := os.ReadFile(filepath.Join(demuxTo(t, src, false), "1.aac"))
	if err != nil || !bytes.Equal(out, []byte{0, 1, 1, 1, 2, 1}) {
		t.Fatalf("AAC without a config: % x, %v; want the bare frames", out, err)
	}
}
