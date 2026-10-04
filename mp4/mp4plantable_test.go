package mp4

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// mp4PlanTable must hand every window back exactly as the full sample arrays
// described it: the fields the segment builders read (size, sync, decode time,
// duration, composition offset) and each sample's file offset. The fixture
// covers both timing branches (reordered video, grid audio), a duration past
// 32 bits, both offset layouts (contiguous runs, one sample per chunk), a
// window whose sync flags follow no pattern, and an empty window.
func TestMP4PlanTableWindowsMatchSamples(t *testing.T) {
	video := make([]fragSample, 0, 97)
	for i := 0; i < 97; i++ {
		// IBBP-like reorder: decode order differs from presentation order.
		pts := int64(i) * 40
		switch i % 4 {
		case 1:
			pts += 80
		case 2, 3:
			pts -= 40
		}
		video = append(video, fragSample{size: uint32(100 + i*7), ptsMs: pts, blockPtsMs: pts, sync: i%24 == 0})
	}
	fillFragTiming(video, 0, movieTimescale, 0)
	video[40].durTS = 1 << 33 // a gap no 32-bit duration holds
	for i := 41; i < len(video); i++ {
		video[i].dtsTS = video[i-1].dtsTS + video[i-1].durTS
	}

	audio := make([]fragSample, 0, 200)
	for i := 0; i < 200; i++ {
		pts := int64(i) * 1024 * 1000 / 48000
		audio = append(audio, fragSample{size: uint32(300 + i%5), ptsMs: pts, blockPtsMs: pts, sync: true})
	}
	fillFragTiming(audio, 0, 48000, 1024)

	runs := func(samples []fragSample, perRun int) []int64 {
		offs := make([]int64, len(samples))
		off := int64(5 << 32) // past 4 GiB: offsets must stay 64-bit
		for i := range samples {
			if i%perRun == 0 {
				off += 4096 // another track's bytes sit in between
			}
			offs[i] = off
			off += int64(samples[i].size)
		}
		return offs
	}

	cases := []struct {
		name    string
		samples []fragSample
		offs    []int64
		starts  []int32
	}{
		{"video in long runs", video, runs(video, 12), []int32{0, 24, 48, 96}},
		{"video one sample per chunk", video, runs(video, 1), []int32{0, 24, 48, 96}},
		{"video cut off its keyframes", video, runs(video, 12), []int32{0, 10, 60}},
		{"grid audio", audio, runs(audio, 20), []int32{0, 7, 150, 150, 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tab := newMP4PlanTable(tc.samples, tc.offs, tc.starts)
			for k := range tc.starts {
				start, end := int(tc.starts[k]), len(tc.samples)
				if k+1 < len(tc.starts) {
					end = int(tc.starts[k+1])
				}
				got, offs := tab.window(k)
				if len(got) != end-start || len(offs) != end-start {
					t.Fatalf("window %d: %d samples, %d offsets, want %d", k, len(got), len(offs), end-start)
				}
				for i := range got {
					want := tc.samples[start+i]
					want.ptsMs, want.blockPtsMs = 0, 0 // not carried: no window reader uses them
					if got[i] != want {
						t.Errorf("window %d sample %d = %+v, want %+v", k, i, got[i], want)
					}
					if offs[i] != tc.offs[start+i] {
						t.Errorf("window %d sample %d offset = %d, want %d", k, i, offs[i], tc.offs[start+i])
					}
				}
			}
		})
	}
}

// An MP4 plan lives in a server cache for as long as a viewer might come back:
// what it keeps per sample is its weight. Once built it must hold the packed
// table only - not the full sample arrays the construction worked on - and
// still report the audio bandwidth those arrays measured.
func TestPlanHLSFromMP4KeepsPackedTablesOnly(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 1500; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 3000; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	mkvSrc := buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		gblocks)
	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.mp4tabs) != len(plan.tracks) {
		t.Fatalf("%d tables for %d tracks", len(plan.mp4tabs), len(plan.tracks))
	}
	for i, pt := range plan.tracks {
		if pt.ft.samples != nil {
			t.Errorf("track %d: the plan still holds %d full samples", i, len(pt.ft.samples))
		}
		tab := plan.mp4tabs[i]
		n := 0
		for k := 0; k < plan.NumSegments(); k++ {
			samples, _ := tab.window(k)
			n += len(samples)
		}
		if want := map[bool]int{true: 1500, false: 3000}[pt.ft.outTrack.spec.video]; n != want {
			t.Fatalf("track %d: the table holds %d samples, want %d", i, n, want)
		}
		held := cap(tab.blob) + cap(tab.segPos)*4 + cap(tab.segDTS)*8
		if perSample := float64(held) / float64(n); perSample > 2 {
			t.Errorf("track %d: %.2f bytes per sample over %d samples, want at most 2", i, perSample, n)
		}
		if pt.ft.outTrack.spec.video {
			continue
		}
		mpd, _, err := plan.Resource(context.Background(), "manifest.mpd")
		if err != nil {
			t.Fatal(err)
		}
		bw := dashAudioBandwidth(pt.ft)
		if bw <= 0 || !bytes.Contains(mpd, []byte(fmt.Sprintf(`id="a1" bandwidth="%d"`, bw))) {
			t.Errorf("audio bandwidth after the release = %d, not what the manifest declared:\n%s", bw, mpd)
		}
	}
}

// Every column form must give back the values it packed: one shared value,
// the 1-, 2- and 4-bit dictionaries (at their distinct-value limits), and the
// plain list a 17th distinct value - or a dictionary that would not be
// shorter - falls back to.
func TestPlanColumnRoundTrip(t *testing.T) {
	cycle := func(n, distinct int, scale uint64) []uint64 {
		vals := make([]uint64, n)
		for i := range vals {
			vals[i] = uint64(i%distinct)*scale + 1500
		}
		return vals
	}
	cases := []struct {
		name string
		vals []uint64
		mode byte
	}{
		{"one value", cycle(37, 1, 1), colConst},
		{"two values", cycle(37, 2, 48), colDict1},
		{"four values", cycle(37, 4, 48), colDict2},
		{"five values", cycle(37, 5, 48), colDict4},
		{"sixteen values", cycle(101, 16, 1<<40), colDict4},
		{"seventeen values", cycle(101, 17, 1<<40), colList},
		{"too short for a dictionary", []uint64{3, 9}, colList},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := packColumn(nil, len(tc.vals), func(i int) uint64 { return tc.vals[i] })
			if b[0] != tc.mode {
				t.Errorf("packed as form %d, want %d", b[0], tc.mode)
			}
			r := planReader{b: append(b, 0xEE)} // a following field must be left untouched
			got := make([]uint64, len(tc.vals))
			r.column(len(got), func(i int, v uint64) { got[i] = v })
			for i := range got {
				if got[i] != tc.vals[i] {
					t.Fatalf("value %d = %d, want %d", i, got[i], tc.vals[i])
				}
			}
			if len(r.b) != 1 || r.b[0] != 0xEE {
				t.Errorf("the column read left %d bytes, want the 1 that follows it", len(r.b))
			}
		})
	}
}
