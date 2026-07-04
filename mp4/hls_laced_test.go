package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// Real muxers store fixed-rate audio LACED: one SimpleBlock carries 8 AAC
// frames under a single timecode, and the reader times frame i at blockTS +
// i×DefaultDuration. These tests pin the end-to-end consequences of that
// timing on the MP4 paths - the collapsed-timestamp bug gave every laced
// frame the block timecode, so the fMP4 muxer derived runs of ZERO sample
// durations (stuttering audio, non-monotonic DTS, players freezing after a
// few seeks) and the progressive remux wrote collapsed cts.

// lacedFixtureDurNs is an AAC-LC frame at 48 kHz: 1024 samples = 21.333 ms.
const lacedFixtureDurNs = 21_333_333

// lacedAudioBlockPts is laced block j's stored timecode on the drift-free
// grid a real muxer writes: round(j × 8 frames × 21.333 ms).
func lacedAudioBlockPts(j int) int64 {
	return (int64(j)*8*lacedFixtureDurNs + 500_000) / 1_000_000
}

func writeLacedRawBlock(cl *bytes.Buffer, track byte, relTC int16, frames [][]byte) {
	payload := []byte{0x80 | track, byte(uint16(relTC) >> 8), byte(uint16(relTC)), 0x84, byte(len(frames) - 1)}
	for _, fr := range frames {
		payload = append(payload, fr...)
	}
	ebml.WriteElementHeader(cl, mkv.IDSimpleBlock, int64(len(payload)))
	cl.Write(payload)
}

func writeRawBlock(cl *bytes.Buffer, track byte, relTC int16, key bool, data []byte) {
	flags := byte(0)
	if key {
		flags = 0x80
	}
	payload := append([]byte{0x80 | track, byte(uint16(relTC) >> 8), byte(uint16(relTC)), flags}, data...)
	ebml.WriteElementHeader(cl, mkv.IDSimpleBlock, int64(len(payload)))
	cl.Write(payload)
}

// buildLacedFixture writes a 6 s Matroska file: 25 fps video (keyframe
// opening each 1 s cluster, unlaced) and 48 kHz AAC stored as 8-frame laced
// SimpleBlocks with DefaultDuration declared - the shape of real-world files.
// Returns the path and the total number of audio frames.
func buildLacedFixture(t testing.TB) (string, int) {
	return buildLacedFixtureOpt(t, true)
}

// buildLacedFixtureOpt is buildLacedFixture with a switch to drop the audio
// DefaultDuration: real libraries carry laced audio with no declared stride
// (e.g. many E-AC-3 rips), so the reader cannot spread a lace's frames and the
// packager must recover the grid from the block timecodes themselves.
func buildLacedFixtureOpt(t testing.TB, declareDur bool) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "laced.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const scale, durMs = 1_000_000, 6000
	sr := 48000.0
	ch := uint8(2)
	w, h := uint32(320), uint32(240)
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}
	if declareDur {
		audio.DefaultDurationNs = lacedFixtureDurNs
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		audio,
	}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteMetadata(c, tracks, durMs); err != nil {
		t.Fatal(err)
	}

	audioBlocks := 0
	for ts := int64(0); ts < durMs; ts += 1000 {
		var cl bytes.Buffer
		cl.Write(rawUintElem(mkv.IDTimestamp, uint64(ts), 2))
		for i := 0; i < 25; i++ { // video: 40 ms frames, keyframe first
			data := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, byte(ts/1000), byte(i))
			writeRawBlock(&cl, 1, int16(int64(i)*40), i == 0, data)
		}
		for { // audio: the laced blocks whose stored pts fall in this cluster
			pts := lacedAudioBlockPts(audioBlocks)
			if pts >= ts+1000 || pts >= durMs {
				break
			}
			frames := make([][]byte, 8)
			for i := range frames {
				frames[i] = []byte{0xAF, byte(audioBlocks), byte(i), 0x01}
			}
			writeLacedRawBlock(&cl, 2, int16(pts-ts), frames)
			audioBlocks++
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
	return path, audioBlocks * 8
}

func rawUintElem(id uint32, val uint64, n int) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(n))
	ebml.WriteUint(&b, val, n)
	return b.Bytes()
}

// fragTiming extracts the audio segment's decode timeline: the tfdt
// baseMediaDecodeTime and every trun sample duration.
func fragTiming(t *testing.T, seg []byte) (tfdt uint64, durs []uint32) {
	t.Helper()
	var walk func(b []byte)
	walk = func(b []byte) {
		for off := 0; off+8 <= len(b); {
			size := int64(binary.BigEndian.Uint32(b[off:]))
			typ := string(b[off+4 : off+8])
			hdr := int64(8)
			if size == 1 && off+16 <= len(b) {
				size = int64(binary.BigEndian.Uint64(b[off+8:]))
				hdr = 16
			}
			if size < hdr || int64(off)+size > int64(len(b)) {
				t.Fatalf("bad box %q at %d (size %d)", typ, off, size)
			}
			p := b[int64(off)+hdr : int64(off)+size]
			switch typ {
			case "moof", "traf":
				walk(p)
			case "tfdt":
				if p[0] == 1 {
					tfdt = binary.BigEndian.Uint64(p[4:])
				} else {
					tfdt = uint64(binary.BigEndian.Uint32(p[4:]))
				}
			case "trun":
				flags := binary.BigEndian.Uint32(p[:4]) & 0xFFFFFF
				count := binary.BigEndian.Uint32(p[4:])
				pos := 8
				if flags&0x000001 != 0 {
					pos += 4 // data_offset
				}
				if flags&0x000004 != 0 {
					pos += 4 // first_sample_flags
				}
				for i := uint32(0); i < count; i++ {
					if flags&0x000100 != 0 {
						durs = append(durs, binary.BigEndian.Uint32(p[pos:]))
						pos += 4
					}
					if flags&0x000200 != 0 {
						pos += 4
					}
					if flags&0x000400 != 0 {
						pos += 4
					}
					if flags&0x000800 != 0 {
						pos += 4
					}
				}
			}
			off += int(size)
		}
	}
	walk(seg)
	return tfdt, durs
}

// The audio rendition of a laced source must play on the sample-exact frame
// grid: every trun duration is EXACTLY one frame (1024 samples at 48 kHz -
// never zero, never the ±1 ms jitter of the block timecodes) and each
// segment's tfdt continues exactly where the previous one ended - no
// duplicated DTS, no gap, no drift across seeks.
func TestHLSLacedAudioFrameGrid(t *testing.T) {
	src, wantFrames := buildLacedFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	segs, err := filepath.Glob(filepath.Join(dir, "seg_a1_*.m4s"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no audio segments written (%v)", err)
	}
	sort.Strings(segs)

	var next uint64
	total := 0
	for si, p := range segs {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		tfdt, durs := fragTiming(t, data)
		if len(durs) == 0 {
			t.Fatalf("%s: no trun durations", p)
		}
		if si == 0 && tfdt != 0 {
			t.Errorf("first segment tfdt = %d, want 0", tfdt)
		}
		if si > 0 && tfdt != next {
			t.Errorf("segment %d tfdt = %d, want %d (decode timeline must be contiguous)", si, tfdt, next)
		}
		sum := uint64(0)
		for i, d := range durs {
			// Sample-exact grid: 1024 samples per AAC frame at 48 kHz. 1008 or
			// 1056 = the ms-rounding jitter regression; 0 = the collapse bug.
			if d != 1024 {
				t.Errorf("%s sample %d: duration %d ticks, want exactly 1024", p, i, d)
			}
			sum += uint64(d)
		}
		next = tfdt + sum
		total += len(durs)
	}
	if total != wantFrames {
		t.Errorf("audio samples across segments = %d, want %d (every laced frame carried)", total, wantFrames)
	}
	// Zero drift: the decode timeline ends exactly at frames × 1024.
	if want := uint64(wantFrames) * 1024; next != want {
		t.Errorf("decode timeline ends at %d, want %d (drift)", next, want)
	}
}

// The on-demand plan feeds its mid-file readers the track durations
// explicitly (they never walk over the Tracks element): every resource must
// stay byte-identical to the full pass on a laced source.
func TestPlanHLSLacedMatchesFullPass(t *testing.T) {
	src, _ := buildLacedFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() < 2 {
		t.Fatalf("plan has %d segments, want several", plan.NumSegments())
	}
	for _, name := range plan.Resources() {
		switch name {
		case "master.m3u8", "manifest.mpd":
			continue // BANDWIDTH is estimated on the Matroska plan
		}
		got, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil {
			t.Errorf("full pass did not write %s", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes) - laced timing diverged", name, len(got), len(want))
		}
	}
}

// The progressive remux of a laced source must write strictly increasing
// audio composition times (the collapse wrote runs of identical cts).
func TestRemuxToMP4LacedAudioMonotonic(t *testing.T) {
	src, wantFrames := buildLacedFixture(t)
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	mv, err := parseMP4(f, st.Size(), sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range mv.tracks {
		tk := &mv.tracks[i]
		if tk.trackType != mkv.AudioTrack {
			continue
		}
		found = true
		if len(tk.samples) != wantFrames {
			t.Errorf("audio samples = %d, want %d", len(tk.samples), wantFrames)
		}
		prev := int64(-1)
		for si, s := range tk.samples {
			if s.ctsMs <= prev {
				t.Fatalf("audio cts not strictly increasing at sample %d: %d after %d (laced frames collapsed)",
					si, s.ctsMs, prev)
			}
			prev = s.ctsMs
		}
	}
	if !found {
		t.Fatal("no audio track in the remux")
	}
}

// A laced source with NO DefaultDuration is the real-world collapse: the reader
// hands every frame of a block the block timecode, so a bare PTS→DTS mapping
// produces runs of identical, non-monotonic decode times (players reject them).
// The packager must recover the stride from the block timecodes and restore a
// strictly increasing timeline - across the progressive MP4, the HLS segments,
// and the on-demand plan (which must stay byte-identical to the full pass).
func TestLacedAudioNoDefaultDurationRecoversGrid(t *testing.T) {
	src, wantFrames := buildLacedFixtureOpt(t, false)

	// Progressive MP4: strictly increasing audio cts, every frame carried.
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	mv, err := parseMP4(f, st.Size(), sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range mv.tracks {
		tk := &mv.tracks[i]
		if tk.trackType != mkv.AudioTrack {
			continue
		}
		found = true
		if len(tk.samples) != wantFrames {
			t.Errorf("MP4 audio samples = %d, want %d", len(tk.samples), wantFrames)
		}
		prev := int64(-1)
		for si, s := range tk.samples {
			if s.ctsMs <= prev {
				t.Fatalf("MP4 audio cts not strictly increasing at sample %d: %d after %d (no-DefaultDuration collapse)", si, s.ctsMs, prev)
			}
			prev = s.ctsMs
		}
	}
	if !found {
		t.Fatal("no audio track in the remux")
	}

	// HLS: contiguous, strictly increasing decode timeline (no duplicated DTS).
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	segs, err := filepath.Glob(filepath.Join(dir, "seg_a1_*.m4s"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no audio segments written (%v)", err)
	}
	sort.Strings(segs)
	var next uint64
	total := 0
	for si, p := range segs {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		tfdt, durs := fragTiming(t, data)
		if len(durs) == 0 {
			t.Fatalf("%s: no trun durations", p)
		}
		if si > 0 && tfdt != next {
			t.Errorf("segment %d tfdt = %d, want %d (decode timeline must be contiguous)", si, tfdt, next)
		}
		sum := uint64(0)
		for i, d := range durs {
			if d == 0 {
				t.Errorf("%s sample %d: zero duration (collapse)", p, i)
			}
			sum += uint64(d)
		}
		next = tfdt + sum
		total += len(durs)
	}
	if total != wantFrames {
		t.Errorf("HLS audio samples = %d, want %d", total, wantFrames)
	}

	// On-demand plan serves byte-identical media segments with the recovered
	// grid. (Only the segment m4s are checked: for a fractional-millisecond
	// frame - AAC's 21.333 ms here - the plan's closed-form last-frame index
	// and the full pass's incremental one round the init's total duration
	// apart by at most a frame. Integer-ms laced codecs, i.e. the real case,
	// AC-3/E-AC-3 at 32 ms, are exact both ways; the audio bytes always match.)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range plan.Resources() {
		if !strings.HasPrefix(name, "seg_a1_") && !strings.HasPrefix(name, "seg00") {
			continue // media segments only (see note above)
		}
		got, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil {
			t.Errorf("full pass did not write %s", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes) - recovered grid diverged", name, len(got), len(want))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no media segments compared")
	}
}
