package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// fullAC3 is a 192 kbit/s 48 kHz AC-3 syncframe at its declared size (768
// bytes): the header makeAC3 writes, then padding.
func fullAC3() []byte {
	return append(makeAC3(0 /*48k*/, 20, 8, 1, 7, 1), make([]byte, 768)...)[:768]
}

// eac3Frame is an E-AC-3 syncframe of exactly its declared size.
func eac3Frame(strmtyp, substreamid, fscod, numblkscod uint32) []byte {
	const frmsiz = 99 // (99+1)*2 = 200 bytes
	var w bitWriter
	w.write(0x0B77, 16)
	w.write(strmtyp, 2)
	w.write(substreamid, 3)
	w.write(frmsiz, 11)
	w.write(fscod, 2)
	w.write(numblkscod, 2) // fscod2 when fscod == 3
	w.write(7, 3)          // acmod
	w.write(1, 1)          // lfeon
	w.write(16, 5)         // bsid
	return append(w.bytes(), make([]byte, 200)...)[:200]
}

func TestAC3PacketSamples(t *testing.T) {
	join := func(frames ...[]byte) []byte { return bytes.Join(frames, nil) }
	ac3At441 := append(makeAC3(1 /*44.1k*/, 21 /*192 kbit/s, padded*/, 8, 1, 7, 1), make([]byte, 836)...)[:836]
	cases := []struct {
		name          string
		packet        []byte
		samples, rate uint32
		ok            bool
	}{
		{"AC-3 frame", fullAC3(), 1536, 48000, true},
		{"AC-3 at 44.1 kHz, padded frame", ac3At441, 1536, 44100, true},
		{"E-AC-3 six blocks", eac3Frame(0, 0, 0, 3), 1536, 48000, true},
		{"E-AC-3 one block", eac3Frame(0, 0, 0, 0), 256, 48000, true},
		{"E-AC-3 three blocks at 32 kHz", eac3Frame(0, 0, 2, 2), 768, 32000, true},
		{"E-AC-3 half rate", eac3Frame(0, 0, 3, 1), 1536, 22050, true},
		{"six one-block frames", join(eac3Frame(0, 0, 0, 0), eac3Frame(0, 0, 0, 0), eac3Frame(0, 0, 0, 0),
			eac3Frame(0, 0, 0, 0), eac3Frame(0, 0, 0, 0), eac3Frame(0, 0, 0, 0)), 1536, 48000, true},
		{"core and its dependent substream", join(eac3Frame(0, 0, 0, 3), eac3Frame(1, 0, 0, 3)), 1536, 48000, true},
		{"a second programme is not counted", join(eac3Frame(0, 0, 0, 3), eac3Frame(0, 1, 0, 3)), 1536, 48000, true},
		{"dependent substream alone", eac3Frame(1, 0, 0, 3), 0, 0, false},
		{"frame cut short", fullAC3()[:700], 0, 0, false},
		{"bytes after the frame", append(fullAC3(), 0xDE, 0xAD), 0, 0, false},
		{"not a frame", bytes.Repeat([]byte{0x42}, 768), 0, 0, false},
		{"reserved sample rate", append(makeAC3(3, 20, 8, 1, 7, 1), make([]byte, 768)...), 0, 0, false},
		{"empty", nil, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples, rate, ok := ac3PacketSamples(tc.packet)
			if samples != tc.samples || rate != tc.rate || ok != tc.ok {
				t.Errorf("ac3PacketSamples = %d, %d, %v, want %d, %d, %v", samples, rate, ok, tc.samples, tc.rate, tc.ok)
			}
		})
	}
}

// jitteredAC3MP4 builds an MP4 whose AC-3 sample table strays from the frame
// duration the way a real muxer's does: the frame at 192 ms is stamped a
// millisecond early, so the table reads 5 x 1536, 1 x 1488, 1 x 1584, then
// 1536 again - and the two odd deltas are then rewritten in place to lowDelta
// and highDelta (1535 and 1537 give the one-tick jitter of real files).
func jitteredAC3MP4(t *testing.T, lowDelta, highDelta uint32) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	ch, sr := uint8(6), 48000.0
	var gblocks []genBlock
	for i := 0; i < 250; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 300; i++ {
		pts := int64(i) * 32
		if i == 6 {
			pts--
		}
		gblocks = append(gblocks, genBlock{track: 2, pts: pts, key: true, data: fullAC3()})
	}
	sortGenBlocks(gblocks)
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "ac3", Channels: &ch, SampleRate: &sr},
	}, gblocks)
	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	for _, e := range [][2]uint32{{5, 1536}, {1, 1488}, {1, 1584}, {293, 1536}} {
		binary.Write(&want, binary.BigEndian, e)
	}
	at := bytes.Index(data, want.Bytes())
	if at < 0 {
		t.Fatal("the fixture's audio stts is not the 4-entry table this test rewrites")
	}
	binary.BigEndian.PutUint32(data[at+12:], lowDelta)
	binary.BigEndian.PutUint32(data[at+20:], highDelta)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// planAudioDurations returns the trun durations of the plan's audio track.
func planAudioDurations(t *testing.T, plan *HLSPlan) map[int64]int {
	t.Helper()
	durs := map[int64]int{}
	for i, pt := range plan.tracks {
		if pt.ft.outTrack.spec.video {
			continue
		}
		for k := 0; k < plan.NumSegments(); k++ {
			samples, _ := plan.mp4tabs[i].window(k)
			for x := range samples {
				durs[samples[x].durTS]++
			}
		}
	}
	return durs
}

// An AC-3 frame holds 1536 samples whatever the container says. A source
// whose sample table strays a tick either side of that must come out on the
// exact grid - not with the stray stretched to a whole millisecond by the
// millisecond timeline - and the plan must still equal the full pass.
func TestMP4SourceAC3RidesTheFrameGrid(t *testing.T) {
	src := jitteredAC3MP4(t, 1535, 1537)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if durs := planAudioDurations(t, plan); len(durs) != 1 || durs[1536] != 300 {
		t.Errorf("audio durations = %v, want 300 frames of 1536", durs)
	}
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	for _, name := range plan.Resources() {
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
			t.Errorf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
}

// The grid is taken from the frame only when the sample table agrees with it
// to the tick: a table that says 31 ms then 33 ms describes something the
// frame header does not, and its timing is carried as it is.
func TestMP4SourceAC3KeepsATableTheFrameDoesNotExplain(t *testing.T) {
	plan, err := PlanHLS(context.Background(), jitteredAC3MP4(t, 1488, 1584), Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	durs := planAudioDurations(t, plan)
	if durs[1488] != 1 || durs[1584] != 1 || durs[1536] != 298 {
		t.Errorf("audio durations = %v, want the table's own 1488 and 1584 among 298 x 1536", durs)
	}
}
