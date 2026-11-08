package ops

import (
	"bytes"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// The streams a demux writes, each in the file its codec's tools read (checked against a decoder on real files):
// AV1 as a low-overhead OBU stream with a temporal delimiter a temporal unit, VP8/VP9 in IVF, MPEG-4 Part 2 behind
// its VOS/VOL headers, ALAC in CAF with its magic cookie, PCM in WAV. Written bare, none of them decoded.

// TestDemuxAV1WritesTemporalDelimiters: each block becomes a temporal unit opened by a TD; a keyframe without its
// sequence header takes the av1C's configOBUs after the TD; a block that already opens on a TD keeps it alone.
func TestDemuxAV1WritesTemporalDelimiters(t *testing.T) {
	sh := []byte{0x0A, 0x02, 0xAA, 0xBB}    // OBU_SEQUENCE_HEADER, 2 bytes
	frame := []byte{0x32, 0x02, 0xCC, 0xDD} // OBU_FRAME, 2 bytes
	td := []byte{0x12, 0x00}
	av1C := append([]byte{0x81, 0x00, 0x0C, 0x00}, sh...)
	w, h := uint32(64), uint32(64)
	av1 := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: av1C, Width: &w, Height: &h}
	blocks := [][]byte{append(append([]byte{}, sh...), frame...), frame, frame, append(append([]byte{}, td...), frame...)}
	keys := []bool{true, false, true, false}
	dir := t.TempDir()
	src := demuxFixture(t, dir, []mkv.Track{av1}, 4, func(_ uint64, i int) ([]byte, bool) { return blocks[i], keys[i] })
	got := readAll(t, filepath.Join(demuxTo(t, src, false), "1.obu"))
	var want []byte
	want = append(want, td...)
	want = append(want, blocks[0]...) // its own sequence header
	want = append(want, td...)
	want = append(want, frame...)
	want = append(want, td...)
	want = append(want, sh...) // a keyframe without one: the av1C's
	want = append(want, frame...)
	want = append(want, td...) // kept, not doubled
	want = append(want, frame...)
	if !bytes.Equal(got, want) {
		t.Errorf("obu stream:\n got % x\nwant % x", got, want)
	}
}

// TestDemuxVP9WritesIVF: VP8 and VP9 go out in IVF, the frame count patched into the header once the stream ends,
// each frame behind its size and its timestamp in milliseconds (timebase 1/1000).
func TestDemuxVP9WritesIVF(t *testing.T) {
	w, h := uint32(800), uint32(640)
	for _, c := range []struct{ codec, fourcc string }{{"vp9", "VP90"}, {"vp8", "VP80"}} {
		dir := t.TempDir()
		tr := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: c.codec, Width: &w, Height: &h}
		src := demuxFixture(t, dir, []mkv.Track{tr}, 3, func(_ uint64, i int) ([]byte, bool) { return bytes.Repeat([]byte{byte(i + 1)}, i+2), i == 0 })
		got := readAll(t, filepath.Join(demuxTo(t, src, false), "1.ivf"))
		if len(got) < 32 || string(got[:4]) != "DKIF" || binary.LittleEndian.Uint16(got[4:6]) != 0 || binary.LittleEndian.Uint16(got[6:8]) != 32 || string(got[8:12]) != c.fourcc {
			t.Fatalf("%s: IVF header % x", c.codec, got[:min(32, len(got))])
		}
		if binary.LittleEndian.Uint16(got[12:14]) != 800 || binary.LittleEndian.Uint16(got[14:16]) != 640 || binary.LittleEndian.Uint32(got[16:20]) != 1000 || binary.LittleEndian.Uint32(got[20:24]) != 1 || binary.LittleEndian.Uint32(got[24:28]) != 3 {
			t.Errorf("%s: size %dx%d timebase %d/%d frames %d", c.codec, binary.LittleEndian.Uint16(got[12:14]), binary.LittleEndian.Uint16(got[14:16]), binary.LittleEndian.Uint32(got[16:20]), binary.LittleEndian.Uint32(got[20:24]), binary.LittleEndian.Uint32(got[24:28]))
		}
		pos := 32
		for i := 0; i < 3; i++ {
			n := int(binary.LittleEndian.Uint32(got[pos:]))
			pts := binary.LittleEndian.Uint64(got[pos+4:])
			if n != i+2 || pts != uint64(i*100) || !bytes.Equal(got[pos+12:pos+12+n], bytes.Repeat([]byte{byte(i + 1)}, i+2)) {
				t.Errorf("%s: frame %d: size %d pts %d", c.codec, i, n, pts)
			}
			pos += 12 + n
		}
		if pos != len(got) {
			t.Errorf("%s: %d bytes past the last frame", c.codec, len(got)-pos)
		}
	}
}

// TestDemuxMPEG4VisualLeadsWithItsHeaders: an MPEG-4 Part 2 stream opens on the CodecPrivate's VOS/VOL headers,
// the frames following as stored, in a .m4v.
func TestDemuxMPEG4VisualLeadsWithItsHeaders(t *testing.T) {
	dsi := []byte{0, 0, 1, 0xB0, 0xF5, 0, 0, 1, 0xB5, 0x09}
	w, h := uint32(176), uint32(144)
	tr := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "V_MPEG4/ISO/ASP", CodecPrivate: dsi, Width: &w, Height: &h}
	dir := t.TempDir()
	src := demuxFixture(t, dir, []mkv.Track{tr}, 2, func(_ uint64, i int) ([]byte, bool) { return []byte{0, 0, 1, 0xB6, byte(i)}, i == 0 })
	got := readAll(t, filepath.Join(demuxTo(t, src, false), "1.m4v"))
	want := append(append(append([]byte{}, dsi...), 0, 0, 1, 0xB6, 0), 0, 0, 1, 0xB6, 1)
	if !bytes.Equal(got, want) {
		t.Errorf("m4v:\n got % x\nwant % x", got, want)
	}
}

// TestDemuxALACWritesCAF: an ALAC track goes out as a CAF: desc from the cookie, the cookie as kuki, the frames in
// the data chunk (its size patched at the end), then a pakt chunk with every packet's size.
func TestDemuxALACWritesCAF(t *testing.T) {
	cookie := make([]byte, 24)
	binary.BigEndian.PutUint32(cookie[0:], 4096) // frameLength
	cookie[5], cookie[6], cookie[7], cookie[8], cookie[9] = 16, 40, 10, 14, 2
	binary.BigEndian.PutUint16(cookie[10:], 255)
	binary.BigEndian.PutUint32(cookie[20:], 44100)
	rate, ch := 44100.0, uint8(2)
	tr := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "A_ALAC", CodecPrivate: cookie, SampleRate: &rate, Channels: &ch}
	sizes := []int{5, 7, 9}
	dir := t.TempDir()
	src := demuxFixture(t, dir, []mkv.Track{tr}, 3, func(_ uint64, i int) ([]byte, bool) { return bytes.Repeat([]byte{byte(0xA0 + i)}, sizes[i]), true })
	got := readAll(t, filepath.Join(demuxTo(t, src, false), "1.caf"))
	if len(got) < 8 || string(got[:4]) != "caff" || binary.BigEndian.Uint16(got[4:6]) != 1 {
		t.Fatalf("caf header % x", got[:min(8, len(got))])
	}
	chunks := map[string][]byte{}
	for pos := 8; pos+12 <= len(got); {
		typ, size := string(got[pos:pos+4]), int(binary.BigEndian.Uint64(got[pos+4:]))
		if size < 0 || pos+12+size > len(got) {
			t.Fatalf("chunk %s of %d bytes at %d runs past the file (%d)", typ, size, pos, len(got))
		}
		chunks[typ] = got[pos+12 : pos+12+size]
		pos += 12 + size
	}
	desc := chunks["desc"]
	if len(desc) != 32 || math.Float64frombits(binary.BigEndian.Uint64(desc)) != 44100 || string(desc[8:12]) != "alac" || binary.BigEndian.Uint32(desc[16:]) != 0 || binary.BigEndian.Uint32(desc[20:]) != 4096 || binary.BigEndian.Uint32(desc[24:]) != 2 || binary.BigEndian.Uint32(desc[28:]) != 16 {
		t.Errorf("desc % x", desc)
	}
	if !bytes.Equal(chunks["kuki"], cookie) {
		t.Errorf("kuki % x", chunks["kuki"])
	}
	var payload []byte
	for i, n := range sizes {
		payload = append(payload, bytes.Repeat([]byte{byte(0xA0 + i)}, n)...)
	}
	if data := chunks["data"]; len(data) != 4+len(payload) || !bytes.Equal(data[4:], payload) {
		t.Errorf("data chunk %d bytes, want the edit count and %d bytes of frames", len(data), len(payload))
	}
	pakt := chunks["pakt"]
	if len(pakt) != 24+3 || binary.BigEndian.Uint64(pakt) != 3 || binary.BigEndian.Uint64(pakt[8:]) != 3*4096 || !bytes.Equal(pakt[24:], []byte{5, 7, 9}) {
		t.Errorf("pakt % x", pakt)
	}
}

// TestDemuxPCMWritesWAV: PCM goes out as WAV with its sizes patched at the end; big-endian samples are turned
// little-endian (WAV has no other byte order), 16-bit integers as plain PCM, floats as IEEE float.
func TestDemuxPCMWritesWAV(t *testing.T) {
	rate, ch, bits := 48000.0, uint8(2), uint8(16)
	for _, c := range []struct {
		codec string
		tag   uint16
		in    []byte
		out   []byte
	}{
		{"A_PCM/INT/LIT", 1, []byte{1, 2, 3, 4, 5, 6, 7, 8}, []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{"A_PCM/INT/BIG", 1, []byte{1, 2, 3, 4, 5, 6, 7, 8}, []byte{2, 1, 4, 3, 6, 5, 8, 7}},
	} {
		dir := t.TempDir()
		tr := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: c.codec, SampleRate: &rate, Channels: &ch, BitDepth: &bits}
		src := demuxFixture(t, dir, []mkv.Track{tr}, 1, func(_ uint64, i int) ([]byte, bool) { return c.in, true })
		got := readAll(t, filepath.Join(demuxTo(t, src, false), "1.wav"))
		if len(got) != 44+len(c.out) || string(got[:4]) != "RIFF" || binary.LittleEndian.Uint32(got[4:]) != uint32(36+len(c.out)) || string(got[8:16]) != "WAVEfmt " {
			t.Fatalf("%s: wav header % x (%d bytes)", c.codec, got[:min(16, len(got))], len(got))
		}
		if binary.LittleEndian.Uint32(got[16:]) != 16 || binary.LittleEndian.Uint16(got[20:]) != c.tag || binary.LittleEndian.Uint16(got[22:]) != 2 || binary.LittleEndian.Uint32(got[24:]) != 48000 || binary.LittleEndian.Uint32(got[28:]) != 192000 || binary.LittleEndian.Uint16(got[32:]) != 4 || binary.LittleEndian.Uint16(got[34:]) != 16 {
			t.Errorf("%s: fmt % x", c.codec, got[20:36])
		}
		if string(got[36:40]) != "data" || binary.LittleEndian.Uint32(got[40:]) != uint32(len(c.out)) || !bytes.Equal(got[44:], c.out) {
			t.Errorf("%s: data % x", c.codec, got[36:])
		}
	}
	fbits := uint8(32)
	tr := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "A_PCM/FLOAT/IEEE", SampleRate: &rate, Channels: &ch, BitDepth: &fbits}
	src := demuxFixture(t, t.TempDir(), []mkv.Track{tr}, 1, func(_ uint64, i int) ([]byte, bool) { return make([]byte, 8), true })
	got := readAll(t, filepath.Join(demuxTo(t, src, false), "1.wav"))
	if len(got) < 36 || binary.LittleEndian.Uint16(got[20:]) != 3 || binary.LittleEndian.Uint16(got[34:]) != 32 {
		t.Errorf("float: fmt tag %d bits %d", binary.LittleEndian.Uint16(got[20:]), binary.LittleEndian.Uint16(got[34:]))
	}
}

// TestAnnexBSkipsEmptyNALUnits: a zero-length NAL unit (padding some muxers leave) is skipped, as the mainstream
// bitstream filter skips it, instead of refusing the frame; a length running past the frame still does.
func TestAnnexBSkipsEmptyNALUnits(t *testing.T) {
	got, err := appendAnnexB(nil, []byte{0, 0, 0, 0, 0, 0, 0, 2, 0x65, 0xAA, 0, 0, 0, 0}, 4)
	if err != nil || !bytes.Equal(got, []byte{0, 0, 0, 1, 0x65, 0xAA}) {
		t.Errorf("got % x, %v", got, err)
	}
	if _, err := appendAnnexB(nil, []byte{0, 0, 0, 9, 0x65}, 4); err == nil {
		t.Error("a NAL length past the frame was accepted")
	}
	if got, err := annexBAppend(nil, []byte{0, 0, 0, 0, 0, 0, 0, 1, 0x65}, 4); err != nil || !bytes.Equal(got, []byte{0, 0, 0, 1, 0x65}) { // the demux's own entry
		t.Errorf("annexBAppend: % x, %v", got, err)
	}
}
