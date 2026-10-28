package mp4

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

// TestCo64OffsetNearTheTopIsRefused: a co64 chunk at 2^63-6 holding a 10-byte sample ends past 2^63, wrapped negative,
// which passed the "past the file" check; the sample table took it.
func TestCo64OffsetNearTheTopIsRefused(t *testing.T) {
	a := audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC))
	stsd := fullBox("stsd", 0, 0, func(w *bw) { w.u32(1); w.bytes(a) })
	stts := fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(1024) })
	stsc := fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(1); w.u32(1) })
	stsz := fullBox("stsz", 0, 0, func(w *bw) { w.u32(10); w.u32(1) })
	co64 := fullBox("co64", 0, 0, func(w *bw) { w.u32(1); w.u32(math.MaxInt32); w.u32(math.MaxUint32 - 5) })
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(48000)
		w.u32(1024)
		w.u16(0x55C4)
		w.u16(0)
	})
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) { w.u32(0); w.fourcc("soun"); w.zeros(12); w.u8(0) })
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) { w.u32(0); w.u32(0); w.u32(1); w.u32(0); w.u32(0) })
	trak := container("trak", tkhd, container("mdia", mdhd, hdlr, container("minf", container("stbl", stsd, stts, stsc, stsz, co64))))
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0), trak)...)
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
	if err == nil && mv.tracks[0].unsupported == "" {
		t.Errorf("a sample at 2^63-6 was taken: %+v", mv.tracks[0].samples)
	}
}

// TestTrackWindowsSampleNearTheTop: a sample whose end wraps past 2^63 made read allocate a negative length.
func TestTrackWindowsSampleNearTheTop(t *testing.T) {
	sr := newTrackWindows(bytes.NewReader(make([]byte, 100)), 100, 1)
	samples := []inSample{{offset: math.MaxInt64 - 5, size: 10}}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("read panicked: %v", r)
			}
		}()
		if _, err := sr.read(0, samples, 0); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("read = %v; want io.ErrUnexpectedEOF", err)
		}
	}()
}
