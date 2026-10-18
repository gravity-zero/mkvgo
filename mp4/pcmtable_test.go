package mp4

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// longPCMMovie is a moov-only MOV of one sowt stereo 48 kHz track: seconds one-second chunks of one-frame samples.
func longPCMMovie(seconds int) []byte {
	const frames, frameBytes = 48000, 4
	stts := fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(uint32(seconds * frames)); w.u32(1) })
	stsc := fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(frames); w.u32(1) })
	stsz := fullBox("stsz", 0, 0, func(w *bw) { w.u32(frameBytes); w.u32(uint32(seconds * frames)) })
	stco := fullBox("co64", 0, 0, func(w *bw) {
		w.u32(uint32(seconds))
		for i := 0; i < seconds; i++ {
			w.u32(0)
			w.u32(uint32(4096 + i*frames*frameBytes))
		}
	})
	moov := container("moov", mvhdBox(uint32(seconds*1000)),
		soundTrak(1, 48000, uint32(seconds*frames), audioSampleEntry("sowt", &stereo48k, nil), stts, stsc, stsz, stco))
	return append(box("ftyp", []byte("qt  \x00\x00\x02\x00qt  ")), moov...)
}

// TestLongPCMTrackIsCarried pins that a PCM track's budget is its packets, not its frames: 6 min of 48 kHz audio
// declares 17.28M one-frame samples, past the movie's 16M, and is still carried, in blocks of 1024 frames.
func TestLongPCMTrackIsCarried(t *testing.T) {
	const seconds = 360
	mv, err := parseMP4(bytes.NewReader(longPCMMovie(seconds)), 1<<40, sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	tr := mv.tracks[0]
	if tr.unsupported != "" {
		t.Fatalf("a 6-minute PCM track was refused: %s", tr.unsupported)
	}
	var bytesTotal int64
	for _, s := range tr.samples {
		bytesTotal += int64(s.size)
	}
	if want := seconds * (48000/1024 + 1); len(tr.samples) != want || bytesTotal != seconds*48000*4 {
		t.Errorf("%d blocks of %d bytes in all; want %d blocks of %d", len(tr.samples), bytesTotal, want, seconds*48000*4)
	}
}

// TestPCMBlocksMatchTheExpandedTable pins that the PCM blocks built from the chunks are the ones the one-frame
// table coalesces into, offsets, sizes and times alike.
func TestPCMBlocksMatchTheExpandedTable(t *testing.T) {
	for _, chunks := range [][]int{{3000}, {1024, 1, 2047, 5000}, {10, 20, 30}} {
		data, _ := pcmMP4(t, chunks)
		mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
		if err != nil {
			t.Fatal(err)
		}
		got := mv.tracks[0]
		moov := mustTrakStbl(t, data)
		want := inTrack{timescale: got.timescale, editShiftMs: got.editShiftMs, pcmFrame: got.pcmFrame, trackType: mkv.AudioTrack}
		if err := buildSampleTable(&want, moov, int64(len(data))); err != nil {
			t.Fatal(err)
		}
		coalescePCM(&want)
		if len(got.samples) != len(want.samples) {
			t.Fatalf("%v: %d blocks, want %d", chunks, len(got.samples), len(want.samples))
		}
		for i := range want.samples {
			if got.samples[i] != want.samples[i] {
				t.Fatalf("%v: block %d = %+v, want %+v", chunks, i, got.samples[i], want.samples[i])
			}
		}
	}
}

// mustTrakStbl returns the children of the first trak's stbl.
func mustTrakStbl(t *testing.T, data []byte) []memBox {
	t.Helper()
	boxes, err := iterBoxes(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"moov", "trak", "mdia", "minf", "stbl"} {
		var next []memBox
		for _, b := range boxes {
			if b.typ == path {
				if next, err = iterBoxes(b.payload); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		boxes = next
	}
	return boxes
}

// TestPCMKeyframesModeKeepsTheSampleBudget pins that the keyframes walk takes a PCM track's end from its stts runs,
// the same end the sample-by-sample walk finds, so no PCM table is expanded whatever its packets are charged.
func TestPCMKeyframesModeKeepsTheSampleBudget(t *testing.T) {
	const perTrak = 1 << 22 // five of them pass 1<<24
	table := []([]byte){
		fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(perTrak); w.u32(1) }),
		fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(perTrak); w.u32(1) }),
		fullBox("stsz", 0, 0, func(w *bw) { w.u32(4); w.u32(perTrak) }),
		fullBox("stco", 0, 0, func(w *bw) { w.u32(1); w.u32(0) }),
	}
	traks := [][]byte{mvhdBox(0)}
	for id := uint32(1); id <= 5; id++ {
		traks = append(traks, soundTrak(id, 48000, perTrak, audioSampleEntry("sowt", &stereo48k, nil), table...))
	}
	data := append(box("ftyp", []byte("qt  \x00\x00\x02\x00qt  ")), container("moov", traks...)...)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	mv, err := parseMP4(bytes.NewReader(data), 1<<40, sampleKeyframes)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
		t.Errorf("the keyframes walk allocated %d bytes for five PCM tables: it expanded them", got)
	}
	for _, tr := range mv.tracks {
		if tr.unsupported != "" || tr.sampleEndMs != (perTrak-1)*1000/48000 {
			t.Errorf("track %d: end %d ms, refused %q; want %d ms, carried", tr.trackID, tr.sampleEndMs, tr.unsupported, (perTrak-1)*1000/48000)
		}
	}
	_, endMs, err := buildKeyframeTimes(mustTrakStbl(t, data), 48000, 0, false, 1<<40)
	if err != nil || endMs != mv.tracks[0].sampleEndMs {
		t.Errorf("the sample-by-sample end is %d ms (%v), the runs' %d", endMs, err, mv.tracks[0].sampleEndMs)
	}
}
