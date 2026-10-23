package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// lacedStripMKV writes 6 s of AVCC H.264 (3 NAL units a frame, keyframe each
// second; with strip, every frame's first two bytes header-stripped) and AAC
// as 8-frame laced SimpleBlocks: lacing 1 Xiph, 3 EBML, 2 fixed (flags bits).
func lacedStripMKV(t *testing.T, lacing byte, strip bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "laced.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sr, ch := 48000.0, uint8(2)
	w, h := uint32(320), uint32(240)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}
	if strip {
		video.Compression, video.HeaderStripping = mkv.CompressionHeaderStrip, []byte{0, 0}
	}
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, DefaultDurationNs: lacedFixtureDurNs}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteMetadata(c, []mkv.Track{video, audio}, 6000); err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for ts := int64(0); ts < 6000; ts += 1000 {
		var cl bytes.Buffer
		cl.Write(rawUintElem(mkv.IDTimestamp, uint64(ts), 2))
		for i := 0; i < 25; i++ {
			fr := cryptVecFrame(int(ts/40)+i, i == 0)
			if strip {
				fr = fr[2:]
			}
			writeRawBlock(&cl, 1, int16(int64(i)*40), i == 0, fr)
		}
		for {
			pts := lacedAudioBlockPts(blocks)
			if pts >= ts+1000 || pts >= 6000 {
				break
			}
			frames := make([][]byte, 8)
			for i := range frames {
				n := 20 + 37*i
				if lacing == 2 {
					n = 64
				}
				frames[i] = bytes.Repeat([]byte{byte(blocks), byte(i)}, n/2)
			}
			payload := []byte{0x82, byte(uint16(pts-ts) >> 8), byte(uint16(pts - ts)), 0x80 | lacing<<1, byte(len(frames) - 1)}
			switch lacing {
			case 1: // Xiph
				for _, fr := range frames[:7] {
					n := len(fr)
					for ; n >= 255; n -= 255 {
						payload = append(payload, 0xFF)
					}
					payload = append(payload, byte(n))
				}
			case 3: // EBML: the first size, then signed 2-byte differences
				payload = binary.BigEndian.AppendUint16(payload, 0x4000|uint16(len(frames[0])))
				for i := 1; i < 7; i++ {
					payload = binary.BigEndian.AppendUint16(payload, 0x4000|uint16(len(frames[i])-len(frames[i-1])+8191))
				}
			}
			for _, fr := range frames {
				payload = append(payload, fr...)
			}
			ebml.WriteElementHeader(&cl, mkv.IDSimpleBlock, int64(len(payload)))
			cl.Write(payload)
			blocks++
		}
		m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: ts, Track: 1, ClusterPos: m.RelPos()})
		if _, err := ebml.WriteElementHeader(m.W, mkv.IDCluster, int64(cl.Len())); err != nil {
			t.Fatal(err)
		}
		if _, err := m.W.Write(cl.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A Matroska source's segments stream from each sample's place in the file -
// laced audio (Xiph, EBML, fixed) and header-stripped video included - byte
// for byte like Resource, in the clear and encrypted (AES-128, cenc, cbcs).
func TestOpenStreamsLacedAndStrippedMKV(t *testing.T) {
	copyFromSource(t)
	modes := encryptionModes()
	modes["clear"] = Options{}
	for _, lacing := range []byte{1, 3, 2} {
		for _, strip := range []bool{false, true} {
			src := lacedStripMKV(t, lacing, strip)
			for name, o := range modes {
				t.Run(name, func(t *testing.T) {
					o.SegmentMs = 2000
					plan := mustPlan(t, src, o)
					assertOpenMatchesResource(t, plan)
					segs := int64(0)
					for _, r := range plan.Resources() {
						if _, _, ok := plan.segmentName(r); ok {
							segs++
						}
					}
					if st := plan.Stats(); st.StreamedSegments != segs || st.StreamFallbacks != 0 {
						t.Errorf("lacing %d strip %v: %d of %d segments streamed (audio included), stats %+v", lacing, strip, st.StreamedSegments, segs, st)
					}
				})
			}
		}
	}
}
