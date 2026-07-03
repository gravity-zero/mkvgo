package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// A laced block stores ONE timecode for its N frames; the reader must deliver
// frame i at blockTS + round(i×DefaultDuration). These tests pin the fix for
// the collapsed-timestamp bug: every frame of a lace used to keep the block
// timecode, so any consumer deriving durations from timestamp deltas (fMP4
// muxing, rewrites) produced runs of zero-duration samples - broken audio and
// non-monotonic DTS downstream.

// aacDurNs is an AAC-LC frame at 48 kHz: 1024 samples = 21.333... ms.
const aacDurNs = 21_333_333

// lacedTimingCluster builds a Cluster{Timestamp, SimpleBlock} whose block
// carries 8 fixed-laced frames of 2 bytes each - data[0] = frame index.
func lacedTimingCluster(ts int64) []byte {
	payload := []byte{0x81, 0x00, 0x00, 0x84, 0x07} // track 1, relTC 0, keyframe|fixed-lacing, 8 frames
	for i := 0; i < 8; i++ {
		payload = append(payload, byte(i), 0xAB)
	}
	var cl bytes.Buffer
	cl.Write(uintElem(mkv.IDTimestamp, uint64(ts), 2))
	ebml.WriteElementHeader(&cl, mkv.IDSimpleBlock, int64(len(payload)))
	cl.Write(payload)
	return masterElem(mkv.IDCluster, cl.Bytes())
}

// lacedTimingFixture builds a complete MKV - Info, Tracks (one audio track,
// optionally declaring DefaultDuration), two laced clusters at 0 ms and
// 171 ms - and returns the bytes plus the first cluster's absolute offset.
func lacedTimingFixture(withDur bool) (data []byte, clusterOff int64) {
	entry := []([]byte){
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
	}
	if withDur {
		entry = append(entry, uintElem(mkv.IDDefaultDuration, aacDurNs, 4))
	}
	tracks := masterElem(mkv.IDTracks, trackEntry(entry...))

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(tracks)
	clusterRel := int64(seg.Len())
	seg.Write(lacedTimingCluster(0))
	seg.Write(lacedTimingCluster(171))

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	segStart := int64(buf.Len())
	buf.Write(seg.Bytes())
	return buf.Bytes(), segStart + clusterRel
}

// wantLacedTimecodes is the hand-computed grid: blockTS + round(i×21.333 ms).
var wantLacedTimecodes = []int64{
	0, 21, 43, 64, 85, 107, 128, 149, // cluster at 0
	171, 192, 214, 235, 256, 278, 299, 320, // cluster at 171
}

func collectBlocks(t *testing.T, br *BlockReader, n int) []mkv.Block {
	t.Helper()
	out := make([]mkv.Block, 0, n)
	for i := 0; i < n; i++ {
		b, err := br.Next()
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		out = append(out, b)
	}
	if _, err := br.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after %d blocks: err = %v, want EOF", n, err)
	}
	return out
}

// The sequential reader walks over the Tracks element anyway: it must pick the
// DefaultDurations up on its own and time every laced frame.
func TestLacedFramesTimedByWalkedTracksElement(t *testing.T) {
	data, _ := lacedTimingFixture(true)
	br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	blocks := collectBlocks(t, br, len(wantLacedTimecodes))
	for i, b := range blocks {
		if b.Timecode != wantLacedTimecodes[i] {
			t.Errorf("frame %d: timecode %d, want %d", i, b.Timecode, wantLacedTimecodes[i])
		}
		if !b.Keyframe {
			t.Errorf("frame %d: not a keyframe - the lace's flag covers every frame", i)
		}
		if len(b.Data) != 2 || b.Data[0] != byte(i%8) {
			t.Errorf("frame %d: data %v, want [%d 171]", i, b.Data, i%8)
		}
	}
}

// Without a DefaultDuration the stride is unknowable: the frames keep the
// block timecode (the documented fallback), rather than guessing.
func TestLacedFramesWithoutDefaultDurationKeepBlockTimecode(t *testing.T) {
	data, _ := lacedTimingFixture(false)
	br, err := NewBlockReader(bytes.NewReader(data), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	blocks := collectBlocks(t, br, 16)
	for i, b := range blocks {
		want := int64(0)
		if i >= 8 {
			want = 171
		}
		if b.Timecode != want {
			t.Errorf("frame %d: timecode %d, want %d (collapsed fallback)", i, b.Timecode, want)
		}
	}
}

// A mid-file reader never sees the Tracks element: without the explicit
// durations it falls back to the block timecode, with them it distributes -
// exactly what the on-demand HLS plan wires.
func TestBlockReaderAtLacedNeedsExplicitDurations(t *testing.T) {
	data, off := lacedTimingFixture(true)

	br, err := NewBlockReaderAt(bytes.NewReader(data), 1_000_000, off)
	if err != nil {
		t.Fatal(err)
	}
	b0, _ := br.Next()
	b1, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b0.Timecode != 0 || b1.Timecode != 0 {
		t.Fatalf("without durations: timecodes %d,%d, want 0,0 (fallback)", b0.Timecode, b1.Timecode)
	}

	br, err = NewBlockReaderAt(bytes.NewReader(data), 1_000_000, off)
	if err != nil {
		t.Fatal(err)
	}
	br.SetTrackDefaultDurations(map[uint64]int64{1: aacDurNs})
	blocks := collectBlocks(t, br, len(wantLacedTimecodes))
	for i, b := range blocks {
		if b.Timecode != wantLacedTimecodes[i] {
			t.Errorf("frame %d: timecode %d, want %d", i, b.Timecode, wantLacedTimecodes[i])
		}
	}
}

// Xiph lacing (variable frame sizes) rides the same distribution: the stride
// logic is lacing-scheme agnostic.
func TestLacedFramesXiphDistributed(t *testing.T) {
	// track 1, relTC 0, keyframe|xiph, 3 frames of 2, 3 and 4 bytes.
	payload := []byte{0x81, 0x00, 0x00, 0x82, 0x02, 0x02, 0x03,
		0xA0, 0xA1, 0xB0, 0xB1, 0xB2, 0xC0, 0xC1, 0xC2, 0xC3}
	var cl bytes.Buffer
	cl.Write(uintElem(mkv.IDTimestamp, 0, 1))
	ebml.WriteElementHeader(&cl, mkv.IDSimpleBlock, int64(len(payload)))
	cl.Write(payload)

	entry := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
		uintElem(mkv.IDDefaultDuration, aacDurNs, 4),
	)
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(masterElem(mkv.IDTracks, entry))
	seg.Write(masterElem(mkv.IDCluster, cl.Bytes()))
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	br, err := NewBlockReader(bytes.NewReader(buf.Bytes()), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	blocks := collectBlocks(t, br, 3)
	wantTC := []int64{0, 21, 43}
	wantLen := []int{2, 3, 4}
	for i, b := range blocks {
		if b.Timecode != wantTC[i] || len(b.Data) != wantLen[i] {
			t.Errorf("frame %d: tc %d len %d, want tc %d len %d", i, b.Timecode, len(b.Data), wantTC[i], wantLen[i])
		}
	}
}

// TrackDefaultDurations keeps only usable strides: declared, positive, and
// plausible (a bogus multi-second per-frame duration must not shift frames).
func TestTrackDefaultDurationsHelper(t *testing.T) {
	m := TrackDefaultDurations([]mkv.Track{
		{ID: 1, DefaultDurationNs: aacDurNs},
		{ID: 2}, // none declared
		{ID: 3, DefaultDurationNs: 60_000_000_000}, // implausible: filtered
	})
	if len(m) != 1 || m[1] != aacDurNs {
		t.Errorf("map = %v, want {1: %d}", m, aacDurNs)
	}
	if m := TrackDefaultDurations(nil); m != nil {
		t.Errorf("no tracks: map = %v, want nil", m)
	}
}
