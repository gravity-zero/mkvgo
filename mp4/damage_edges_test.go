package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// fragSampleView is one sample of a served media segment: its trun entry and its bytes.
type fragSampleView struct {
	dur  uint32
	sync bool
	data []byte
}

// readFragment decodes a served media segment's tfdt and samples (one traf, trun with duration, size and flags).
func readFragment(t *testing.T, seg []byte) (tfdt uint64, samples []fragSampleView) {
	t.Helper()
	var moof, mdat []byte
	for off := 0; off+8 <= len(seg); {
		size := int(binary.BigEndian.Uint32(seg[off:]))
		typ, hl := string(seg[off+4:off+8]), 8
		if size == 1 {
			size, hl = int(binary.BigEndian.Uint64(seg[off+8:])), 16
		}
		switch typ {
		case "moof":
			moof = seg[off+8 : off+size]
		case "mdat":
			mdat = seg[off+hl : off+size]
		}
		off += size
	}
	var trun []byte
	var walk func(b []byte)
	walk = func(b []byte) {
		for off := 0; off+8 <= len(b); {
			size := int(binary.BigEndian.Uint32(b[off:]))
			switch string(b[off+4 : off+8]) {
			case "traf":
				walk(b[off+8 : off+size])
			case "tfdt":
				tfdt = binary.BigEndian.Uint64(b[off+12:])
			case "trun":
				trun = b[off+8 : off+size]
			}
			off += size
		}
	}
	walk(moof)
	flags := binary.BigEndian.Uint32(trun) & 0xFFFFFF
	count := int(binary.BigEndian.Uint32(trun[4:]))
	p, at := 8, 0
	if flags&0x1 != 0 {
		p += 4
	}
	for i := 0; i < count; i++ {
		var s fragSampleView
		size := 0
		if flags&0x100 != 0 {
			s.dur = binary.BigEndian.Uint32(trun[p:])
			p += 4
		}
		if flags&0x200 != 0 {
			size = int(binary.BigEndian.Uint32(trun[p:]))
			p += 4
		}
		if flags&0x400 != 0 {
			s.sync = binary.BigEndian.Uint32(trun[p:])&0x10000 == 0
			p += 4
		}
		if flags&0x800 != 0 {
			p += 4
		}
		s.data = mdat[at : at+size]
		at += size
		samples = append(samples, s)
	}
	return tfdt, samples
}

// TestOnDemandDamageEdges: a zeroed stretch that starts inside a block and ends
// inside a GOP (clusters of about a second, a keyframe every 4 s), the shape of
// the region a real download left. A window serves no block the zeros cut, no
// frame stretched over the region, no frame after it before the next keyframe,
// and a window left empty is timed where it stands, never at the track's end.
func TestOnDemandDamageEdges(t *testing.T) {
	sr, ch := 48000.0, uint8(2)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
		Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}}
	frame := func(i int) []byte {
		b := bytes.Repeat([]byte{0x55}, 3000)
		copy(b, []byte{0xAB, 0xCD, byte(i >> 8), byte(i)})
		return b
	}
	var blocks []genBlock
	for i := 0; i < 25*24; i++ { // 24 s at 25 fps, a keyframe every 4 s
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%100 == 0, data: frame(i)})
		blocks = append(blocks, genBlock{track: 2, pts: int64(i)*40 + 20, key: true, data: bytes.Repeat([]byte{0x21}, 200)})
	}
	clean := buildMKV(t, tracks, blocks)
	data, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	at := func(i int) int { return bytes.Index(data, []byte{0xAB, 0xCD, byte(i >> 8), byte(i)}) }
	for _, shape := range []struct {
		name     string
		from, to int
		lostTo   int // the first frame past the region a player may decode: the next keyframe
	}{
		{"inside a window", at(237) + 1500, at(266) + 10, 300},     // 9.48 s, cut mid-block, to 10.64 s: resumes mid-GOP
		{"over a whole window", at(237) + 1500, at(426) + 10, 500}, // to 17.04 s: the 12-16 s window's cue lies in the zeros
	} {
		t.Run(shape.name, func(t *testing.T) { checkDamageEdges(t, clean, shape.from, shape.to, shape.lostTo) })
	}
}

// checkDamageEdges serves every window of clean zeroed over [from, to) and checks what a damaged region may leave in them.
func checkDamageEdges(t *testing.T, clean string, from, to, lostTo int) {
	t.Helper()
	bad := zeroedSource(t, clean, from, to)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, bad, Options{SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	var prev, prevEnd uint64
	for n := 0; n < plan.NumSegments(); n++ {
		for _, buffered := range []bool{false, true} {
			var seg []byte
			if buffered {
				seg, _, err = plan.Resource(ctx, plan.SegmentName(n))
				if err != nil {
					t.Fatal(err)
				}
			} else {
				seg, _ = serveWindow(t, plan, n)
			}
			tfdt, samples := readFragment(t, seg)
			for k, s := range samples {
				if z := len(s.data) - len(bytes.TrimRight(s.data, "\x00")); z >= 64 {
					t.Errorf("window %d sample %d: ends with %d zero bytes, a block the damage cut", n, k, z)
				}
				if s.dur > 1000 {
					t.Errorf("window %d sample %d: %d ms, a frame stretched over the damage", n, k, s.dur)
				}
				if s.sync && (len(s.data) < 4 || s.data[0] != 0xAB || s.data[1] != 0xCD) {
					t.Errorf("window %d sample %d: flagged a keyframe but holds no frame of the source", n, k)
				}
				if len(s.data) >= 4 && s.data[0] == 0xAB && s.data[1] == 0xCD {
					if i := int(s.data[2])<<8 | int(s.data[3]); i >= 237 && i < lostTo {
						t.Errorf("window %d sample %d: frame %d served, cut by the damage or referencing frames it lost", n, k, i)
					}
					if i := int(s.data[2])<<8 | int(s.data[3]); s.sync && i%100 != 0 {
						t.Errorf("window %d sample %d: flagged a keyframe but holds frame %d", n, k, i)
					}
				}
			}
			if len(samples) > 0 && !samples[0].sync {
				t.Errorf("window %d opens on a frame that is no keyframe (references lost to the damage)", n)
			}
			if tfdt < prev {
				t.Errorf("window %d: tfdt %d ms behind the previous window's %d", n, tfdt, prev)
			}
			if len(samples) == 0 {
				t.Errorf("window %d serves no video: a player loops on an empty fragment when seeking back over the region", n)
			}
			if n > 0 && tfdt != prevEnd {
				t.Errorf("window %d opens at %d ms, the previous one ends at %d: a hole in the video timeline", n, tfdt, prevEnd)
			}
			if buffered {
				prev, prevEnd = tfdt, tfdt
				for _, s := range samples {
					prevEnd += uint64(s.dur)
				}
			}
		}
	}
	if plan.Stats().DamageSkips == 0 {
		t.Fatal("no damaged region was passed over: the fixture does not cut the file")
	}
}

// TestOnDemandDamageFillKeepsTheKeyframeBytes: on a real x264 + AAC file (a cluster and a GOP every 2 s, the
// audio before the video in each cluster) zeroed over two clusters, the window the region emptied serves copies of
// the keyframe the walk met past it. The reader hands that keyframe out in its in-place arena, where the next video
// block lands when the keyframe is not kept: copied unkept, the window served a P frame flagged a keyframe.
func TestOnDemandDamageFillKeepsTheKeyframeBytes(t *testing.T) {
	bad := zeroedSource(t, "../internal/testdata/gop2s_aac.mkv", 80000, 150000) // 4.8 s to 9 s: resumes on the 10 s cluster
	ctx := context.Background()
	plan, err := PlanHLS(ctx, bad, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < plan.NumSegments(); n++ {
		seg, _, err := plan.Resource(ctx, plan.SegmentName(n))
		if err != nil {
			t.Fatal(err)
		}
		_, samples := readFragment(t, seg)
		for k, s := range samples {
			if s.sync && !holdsIDRSlice(s.data) {
				t.Errorf("window %d sample %d: flagged a keyframe, but holds no IDR slice", n, k)
			}
		}
	}
}

// holdsIDRSlice reports whether an AVCC sample (4-byte lengths) holds an IDR slice NAL unit.
func holdsIDRSlice(b []byte) bool {
	for len(b) >= 5 {
		n := int(binary.BigEndian.Uint32(b))
		if n == 0 || 4+n > len(b) {
			return false
		}
		if b[4]&0x1F == 5 {
			return true
		}
		b = b[4+n:]
	}
	return false
}
