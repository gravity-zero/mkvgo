package mp4

import (
	"bytes"
	"strings"
	"testing"
)

// twoEntryTrak is a sound trak whose stsd holds two sample entries and whose second chunk uses the second.
func twoEntryTrak(first, second []byte) []byte {
	stts := fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(4); w.u32(1024) })
	stsc := fullBox("stsc", 0, 0, func(w *bw) {
		w.u32(2)
		w.u32(1)
		w.u32(2)
		w.u32(1)
		w.u32(2)
		w.u32(2)
		w.u32(2)
	})
	stsz := fullBox("stsz", 0, 0, func(w *bw) { w.u32(10); w.u32(4) })
	stco := fullBox("stco", 0, 0, func(w *bw) { w.u32(2); w.u32(100); w.u32(200) })
	stsd := fullBox("stsd", 0, 0, func(w *bw) { w.u32(2); w.bytes(first); w.bytes(second) })
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(48000)
		w.u32(4096)
		w.u16(0x55C4)
		w.u16(0)
	})
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) { w.u32(0); w.fourcc("soun"); w.zeros(12); w.u8(0) })
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) { w.u32(0); w.u32(0); w.u32(1); w.u32(0); w.u32(0) })
	stbl := container("stbl", stsd, stts, stsc, stsz, stco)
	return container("trak", tkhd, container("mdia", mdhd, hdlr, container("minf", stbl)))
}

// TestTrackSwitchingSampleDescriptionsIsRefused pins that a track whose samples use two different sample
// descriptions is refused by name for a remux, which carries one: decoding all of it with the first gave grey or
// corrupt pictures with no warning. Two identical descriptions, and the metadata probe, are unaffected.
func TestTrackSwitchingSampleDescriptionsIsRefused(t *testing.T) {
	a := audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC))
	b := audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, []byte{0x13, 0x90}))
	for _, c := range []struct {
		name    string
		second  []byte
		refused bool
	}{{"two descriptions", b, true}, {"the same description twice", a, false}} {
		data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0), twoEntryTrak(a, c.second))...)
		mv, err := parseMP4(bytes.NewReader(data), 1<<20, sampleFull)
		if err != nil {
			t.Fatal(err)
		}
		got := mv.tracks[0].unsupported
		if c.refused && !strings.Contains(got, "sample description") {
			t.Errorf("%s: unsupported = %q, want a refusal naming the sample descriptions", c.name, got)
		}
		if !c.refused && got != "" {
			t.Errorf("%s: refused: %q", c.name, got)
		}
		if meta, err := parseMP4(bytes.NewReader(data), 1<<20, sampleNone); err != nil || meta.tracks[0].unsupported != "" {
			t.Errorf("%s: the metadata probe refused it: %v %q", c.name, err, meta.tracks[0].unsupported)
		}
	}
}

// TestLazyMoovKeepsTheFirstChunkRun: the lazy moov read kept 16 bytes of a skipped stsc body, its first run's sample
// description index (bytes 16-20) left zeroed when a 64 KiB read ends right before it; the keyframe probe then read
// the run as naming description 0 and refused a track with two identical descriptions.
func TestLazyMoovKeepsTheFirstChunkRun(t *testing.T) {
	a := audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC))
	stsd := fullBox("stsd", 0, 0, func(w *bw) { w.u32(2); w.bytes(a); w.bytes(a) })
	stts := fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(4); w.u32(1024) })
	// stsz runs past the first 64 KiB read, so the read after it starts where it ends; the free box after it is
	// sized so that read ends 24 bytes into the stsc, on its first run's sample description index.
	stsz := fullBox("stsz", 0, 0, func(w *bw) { w.u32(10); w.u32(4); w.zeros(lazyChunk) })
	free := box("free", make([]byte, lazyChunk-24-8))
	stsc := fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(2); w.u32(2) })
	stco := fullBox("stco", 0, 0, func(w *bw) { w.u32(2); w.u32(100); w.u32(200) })
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(48000)
		w.u32(4096)
		w.u16(0x55C4)
		w.u16(0)
	})
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) { w.u32(0); w.fourcc("soun"); w.zeros(12); w.u8(0) })
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) { w.u32(0); w.u32(0); w.u32(1); w.u32(0); w.u32(0) })
	trak := container("trak", tkhd, container("mdia", mdhd, hdlr, container("minf", container("stbl", stsd, stts, stsz, free, stsc, stco))))
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0), trak)...)
	if meta, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull); err != nil || meta.tracks[0].unsupported != "" {
		t.Fatalf("fixture: the full read refuses it: %v %q", err, meta.tracks[0].unsupported)
	}
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleKeyframes)
	if err != nil {
		t.Fatal(err)
	}
	if got := mv.tracks[0].unsupported; got != "" {
		t.Errorf("the keyframe probe refused a track it reads in full: %q", got)
	}
}
