package mp4

// hlsplan_boundary_timing_test.go - segment timing across reordered (open-GOP) boundaries and at the presentation end.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const openGOPMs = 1200

// openGOPDisplayTimes lists GOP g's display times; when vfr the first GOP holds the deepest reorder (a 300 ms stall) and later ones jitter and drop slots.
func openGOPDisplayTimes(g int, vfr bool) []int64 {
	base := int64(g) * openGOPMs
	if vfr && g == 0 {
		return []int64{0, 100, 200, 500, 600, 700, 800, 900, 1000, 1050, 1100, 1150}
	}
	jitter := [12]int64{0, -10, 0, 10, 0, 20, 0, 0, 30, 0, 10, 0}
	var out []int64
	for i := 0; i < 12; i++ {
		if vfr && g%2 == 1 && i == 7 {
			continue
		}
		t := base + int64(i)*100
		if vfr {
			t += jitter[i]
		}
		out = append(out, t)
	}
	return out
}

// openGOPDecodeOrder stores each group of three slots anchor first, so every keyframe but the first is followed by two leading pictures.
func openGOPDecodeOrder(g int, vfr bool) (pts []int64, key []bool) {
	t := openGOPDisplayTimes(g, vfr)
	start := 0
	if g == 0 {
		pts, key = append(pts, t[0]), append(key, true)
		start = 1
	}
	for i := start; i < len(t); i += 3 {
		grp := t[i:min(i+3, len(t))]
		pts, key = append(pts, grp[len(grp)-1]), append(key, g > 0 && i == 0)
		for _, b := range grp[:len(grp)-1] {
			pts, key = append(pts, b), append(key, false)
		}
	}
	return pts, key
}

// openGOPSource builds the source in decode order, one cluster and cue per GOP, audio interleaved after each GOP's video.
func openGOPSource(t testing.TB, gops int, vfr, audio bool) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	seq := 0
	for g := 0; g < gops; g++ {
		pts, key := openGOPDecodeOrder(g, vfr)
		for i := range pts {
			blocks = append(blocks, genBlock{track: 1, pts: pts[i], key: key[i], data: iframeTestFrame(seq, key[i])})
			seq++
		}
		if audio {
			for ms := int64(g) * openGOPMs; ms < int64(g+1)*openGOPMs; ms += 20 {
				blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: []byte{0xAA, byte(ms / 20)}})
			}
		}
	}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	if audio {
		tracks = append(tracks, mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch})
	}
	return buildMKV(t, tracks, blocks)
}

// openGOPEnd is the presentation end: the last display time plus the gap before it.
func openGOPEnd(gops int, vfr bool) int64 {
	t := openGOPDisplayTimes(gops-1, vfr)
	return t[len(t)-1] + (t[len(t)-1] - t[len(t)-2])
}

// assertPlanMatchesFullPass checks the plan serves the full pass's bytes (MPD up to its estimated bandwidth) and returns both.
func assertPlanMatchesFullPass(t *testing.T, src string, segMs int64) (string, *HLSPlan) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: segMs}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: segMs})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"init.mp4", "playlist.m3u8", "manifest.mpd"}
	if _, err := os.Stat(filepath.Join(dir, "iframe.m3u8")); err == nil {
		names = append(names, "iframe.m3u8")
	}
	fts := plan.fts()
	for n := 0; n < plan.NumSegments(); n++ {
		for i := range fts {
			names = append(names, renditionSegment(fts, i, n))
		}
	}
	for _, name := range names {
		got, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "manifest.mpd" {
			got, want = stripBandwidth(got), stripBandwidth(want)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes)\nplan: %q\nfull: %q", name, len(got), len(want), truncated(got), truncated(want))
		}
	}
	return dir, plan
}

func stripBandwidth(mpd []byte) []byte { return bandwidthAttr.ReplaceAll(mpd, nil) }

// segmentTiming returns a media segment's tfdt and its trun's per-sample durations and composition offsets.
func segmentTiming(t *testing.T, seg []byte) (tfdt int64, durs []int64, cts []int64) {
	t.Helper()
	var walk func(b []byte)
	walk = func(b []byte) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b[0:4]))
			typ := string(b[4:8])
			head := 8
			switch size {
			case 0:
				size = len(b)
			case 1:
				size, head = int(binary.BigEndian.Uint64(b[8:16])), 16
			}
			if size < head || size > len(b) {
				t.Fatalf("bad box %q size %d", typ, size)
			}
			body := b[head:size]
			switch typ {
			case "moof", "traf":
				walk(body)
			case "tfdt":
				if body[0] == 1 {
					tfdt = int64(binary.BigEndian.Uint64(body[4:12]))
				} else {
					tfdt = int64(binary.BigEndian.Uint32(body[4:8]))
				}
			case "trun":
				flags := binary.BigEndian.Uint32(body[0:4]) & 0xFFFFFF
				n := int(binary.BigEndian.Uint32(body[4:8]))
				p := 8
				if flags&0x1 != 0 {
					p += 4
				}
				if flags&0x4 != 0 {
					p += 4
				}
				for i := 0; i < n; i++ {
					var d, c int64
					if flags&0x100 != 0 {
						d = int64(binary.BigEndian.Uint32(body[p:]))
						p += 4
					}
					if flags&0x200 != 0 {
						p += 4
					}
					if flags&0x400 != 0 {
						p += 4
					}
					if flags&0x800 != 0 {
						c = int64(int32(binary.BigEndian.Uint32(body[p:])))
						p += 4
					}
					durs, cts = append(durs, d), append(cts, c)
				}
			}
			b = b[size:]
		}
	}
	walk(seg)
	return tfdt, durs, cts
}

// playlistDurations parses a media playlist's EXTINF values.
func playlistDurations(t *testing.T, pl []byte) []float64 {
	t.Helper()
	var out []float64
	for _, l := range strings.Split(string(pl), "\n") {
		if !strings.HasPrefix(l, "#EXTINF:") {
			continue
		}
		d, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(l, "#EXTINF:"), ","), 64)
		if err != nil {
			t.Fatalf("bad EXTINF %q: %v", l, err)
		}
		out = append(out, d)
	}
	return out
}

// Open-GOP sources: plan and full pass agree, the decode clock is continuous, every frame presents at its source time, playlists end at the true end.
func TestPlanHLSOpenGOPBoundaryTiming(t *testing.T) {
	const gops = 5
	for _, tc := range []struct {
		name       string
		vfr, audio bool
	}{{"cfr", false, true}, {"vfr", true, true}, {"video-only", false, false}, {"vfr-video-only", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			src := openGOPSource(t, gops, tc.vfr, tc.audio)
			dir, plan := assertPlanMatchesFullPass(t, src, openGOPMs)
			if plan.NumSegments() != gops {
				t.Fatalf("got %d segments, want one per GOP (%d)", plan.NumSegments(), gops)
			}
			shift, _ := initEditList(t, plan.InitSegment())
			end := openGOPEnd(gops, tc.vfr)

			var clock, total int64
			for n := 0; n < gops; n++ {
				seg, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
				if err != nil {
					t.Fatal(err)
				}
				tfdt, durs, cts := segmentTiming(t, seg)
				if tfdt != clock {
					t.Errorf("segment %d: tfdt %d, previous segment's decode clock ends at %d", n, tfdt, clock)
				}
				want := openGOPDisplayTimes(n, tc.vfr)
				if len(durs) != len(want) {
					t.Fatalf("segment %d: %d samples, want %d", n, len(durs), len(want))
				}
				var presented []int64
				dts := tfdt
				for i := range durs {
					if cts[i] < 0 {
						t.Errorf("segment %d sample %d: negative composition offset %d", n, i, cts[i])
					}
					presented = append(presented, dts+cts[i]-shift)
					dts += durs[i]
				}
				sortInt64s(presented)
				if fmt.Sprint(presented) != fmt.Sprint(want) {
					t.Errorf("segment %d presents at %v, source displays at %v", n, presented, want)
				}
				clock = dts
				total += dts - tfdt
			}
			if total != end {
				t.Errorf("decode clock spans %d ms, presentation is %d ms", total, end)
			}

			durs := playlistDurations(t, plan.MediaPlaylist())
			var sum float64
			for _, d := range durs {
				sum += d
			}
			lastBound := plan.bounds[gops-1]
			if last, want := durs[len(durs)-1], float64(end-lastBound)/1000; fmt.Sprintf("%.3f", last) != fmt.Sprintf("%.3f", want) {
				t.Errorf("last EXTINF %.3f, want %.3f (presentation ends at %d ms, last segment starts at %d)", last, want, end, lastBound)
			}
			if fmt.Sprintf("%.3f", sum) != fmt.Sprintf("%.3f", float64(end)/1000) {
				t.Errorf("EXTINF sum %.3f, presentation is %.3f s", sum, float64(end)/1000)
			}
			mpd, _, err := plan.Resource(context.Background(), "manifest.mpd")
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf(`<S t="%d" d="%d"/>`, lastBound, end-lastBound); !bytes.Contains(mpd, []byte(want)) {
				t.Errorf("manifest lacks the final %s:\n%s", want, mpd)
			}
		})
	}
}

func sortInt64s(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// The final segment ends at the highest PTS plus the final sample's duration, whichever sample is stored last.
func TestSegEndOrLastPresentationEnd(t *testing.T) {
	video := func(ts uint32, mul int64) *fragTrack {
		return &fragTrack{timescale: ts, outTrack: &outTrack{spec: codecSpec{video: true}}, samples: []fragSample{
			{ptsMs: 0, dtsTS: 0, durTS: 100 * mul, sync: true},
			{ptsMs: 300, dtsTS: 100 * mul, durTS: 100 * mul},
			{ptsMs: 100, dtsTS: 200 * mul, durTS: 100 * mul},
			{ptsMs: 200, dtsTS: 300 * mul, durTS: 100 * mul},
		}}
	}
	for _, tc := range []struct {
		ts  uint32
		mul int64
	}{{movieTimescale, 1}, {90000, 90}} {
		if got := segEndOrLast([]int64{0}, 0, []*fragTrack{video(tc.ts, tc.mul)}); got != 400 {
			t.Errorf("timescale %d: final segment ends at %d ms, want 400", tc.ts, got)
		}
	}
	if got := segEndOrLast([]int64{0, 200}, 0, []*fragTrack{video(movieTimescale, 1)}); got != 200 {
		t.Errorf("interior segment ends at %d, want the next boundary 200", got)
	}
}

// The peek ends on the first trailing picture, a keyframe or the bound, never starts for audio, and keeps the lowest PTS.
func TestNextPtsPeek(t *testing.T) {
	audio := startPeek(6000, false)
	if audio.pending() || audio.observe(6020, true) || audio.min != 6000 {
		t.Errorf("audio peek must be over at once with the crossing PTS: %+v", audio)
	}
	closed := startPeek(6000, true)
	if !closed.pending() || !closed.observe(6040, false) || closed.min != 6000 {
		t.Errorf("closed GOP: the first trailing picture must end the peek on the crossing PTS: %+v", closed)
	}
	open := startPeek(8333, true)
	for _, pts := range []int64{8300, 8233, 8267} {
		if open.observe(pts, false) {
			t.Fatalf("leading picture %d ended the peek", pts)
		}
	}
	if !open.observe(8433, false) || open.min != 8233 || open.observe(8400, false) {
		t.Errorf("open GOP: want the lowest leading PTS 8233 and a closed peek: %+v", open)
	}
	intra := startPeek(1000, true)
	if !intra.observe(900, true) || intra.min != 900 {
		t.Errorf("a keyframe must end the peek, folded in: %+v", intra)
	}
	deep := startPeek(1000, true)
	ended := 0
	for i := 1; i <= reorderLookahead+4; i++ {
		if deep.observe(1000-int64(i), false) {
			ended++
			if i != reorderLookahead {
				t.Errorf("peek ended after %d samples, want %d", i, reorderLookahead)
			}
		}
	}
	if ended != 1 || deep.min != 1000-reorderLookahead {
		t.Errorf("bounded peek: ended %d times, min %d: %+v", ended, deep.min, deep)
	}
}

// Block-ordered source: the track-by-track walk peeks the same way, byte-identical windows for the window's cost.
func TestPlanHLSBlockOrderedOpenGOP(t *testing.T) {
	ctx := context.Background()
	src := buildBlockOrderedSourceOpenGOP(t, 60, 20, true)
	assertPlanMatchesFullPass(t, src, 6000)

	tally := &readTally{}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Segment(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if starts := plan.trackPosAt(1); !plan.scatteredWindow(1, starts) {
		t.Fatalf("window 1 must be read track by track: %+v", starts)
	}
	*tally = readTally{}
	v, err := plan.Segment(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := plan.Resource(ctx, renditionSegment(plan.fts(), 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	window := int64(len(v) + len(a))
	if tally.bytes > window+2*(256<<10)+(512<<10) {
		t.Errorf("window 1 read %d KB for %d KB served", tally.bytes>>10, window>>10)
	}
}

// The tail probe fixes the final PTS pair from block headers alone: a last
// cluster of large frames costs their headers, not their payloads.
func TestPlanHLSTailProbeReadsHeadersOnly(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 25; i++ { // a light first cluster for the head probe
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i == 0, data: iframeTestFrame(i, i == 0)})
	}
	const bigFrames, bigSize = 12, 200 << 10
	for i := 0; i < bigFrames; i++ { // a heavy last cluster of 200 KiB frames
		blocks = append(blocks, genBlock{track: 1, pts: 1000 + int64(i)*40, key: i == 0, data: make([]byte, bigSize)})
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	tally := &readTally{}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 1000, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	if tally.bytes > bigFrames*bigSize/2 {
		t.Errorf("plan read %d KB; the tail probe is reading the last cluster's %d KB of payloads", tally.bytes>>10, bigFrames*bigSize>>10)
	}
	assertPlanMatchesFullPass(t, src, 1000)
	if plan.NumSegments() != 2 {
		t.Fatalf("got %d segments, want 2", plan.NumSegments())
	}
}

// A track that ends segments before the presentation does (an audio track
// shorter than the video) keeps its own final duration: init and trailing
// empty segments match the full pass.
func TestPlanHLSTrackEndingBeforeTail(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 300; i++ { // 12 s of video, keyframe every second
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	for i := 0; i < 7*50; i++ { // 7 s of 20 ms audio frames, then silence
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	_, plan := assertPlanMatchesFullPass(t, src, 2000)
	if plan.NumSegments() != 6 {
		t.Fatalf("got %d segments, want 6", plan.NumSegments())
	}
}

// jitteredGridSource builds 12 s of video with AAC audio on its exact 1024-sample
// grid, whose stored timecodes step back one frame from 3.7 s and return to
// nominal at 4.3 s - a transient straddling the 4 s boundary.
func jitteredGridSource(t testing.TB) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 300; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	for i := 0; int64(i)*1024*1000/44100 < 12000; i++ {
		ms := int64(i) * 1024 * 1000 / 44100
		if ms >= 3700 && ms < 4300 {
			ms -= 23
		}
		blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(blocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, DefaultDurationNs: 1024 * 1_000_000_000 / 44100},
	}, blocks)
}

// Grid audio whose timecodes step back: the window opening inside the
// transient keeps the full pass's continuous clock, served in order, cold
// after a seek, and twice over.
func TestPlanHLSGridAudioJitter(t *testing.T) {
	src := jitteredGridSource(t)
	dir, plan := assertPlanMatchesFullPass(t, src, 2000)
	fts := plan.fts()
	if len(fts) != 2 || plan.NumSegments() != 6 {
		t.Fatalf("fixture: %d renditions, %d segments", len(fts), plan.NumSegments())
	}
	ctx := context.Background()
	cold, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{2, 2, 3} {
		name := renditionSegment(fts, 1, n)
		got, _, err := cold.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("cold %s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
}

// buildKeyframeClusteredMKV writes blocks in the order given, opening a new
// cluster at every video keyframe the way muxers do - so audio written just
// ahead of a keyframe lands in the cluster before it.
func buildKeyframeClusteredMKV(t testing.TB, tracks []mkv.Track, blocks []genBlock) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kf.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	var blks []mkv.Block
	var durationMs int64
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
		durationMs = max(durationMs, gb.pts)
	}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}, tracks, durationMs); err != nil {
		t.Fatal(err)
	}
	start, video := 0, false
	for i := 1; i <= len(blks); i++ {
		if i == len(blks) || (video && blks[i].TrackNumber == 1 && blks[i].Keyframe) {
			if err := m.WriteClusterWithCues(blks[start].Timecode, scale, blks[start:i]); err != nil {
				t.Fatal(err)
			}
			start, video = i, false
		}
		if i < len(blks) && blks[i].TrackNumber == 1 {
			video = true
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Audio written 60 ms ahead of the video: the frames opening an audio window
// sit in the cluster before the boundary keyframe's. A cold request (nothing
// learned) must still serve the full pass's window.
func TestPlanHLSColdWindowAudioAhead(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	ai := 0
	for i := 0; i < 300; i++ { // 40 ms video, keyframe every second
		for ; int64(ai)*20 < int64(i)*40+60; ai++ { // 20 ms audio, written 60 ms ahead
			blocks = append(blocks, genBlock{track: 2, pts: int64(ai) * 20, key: true, data: []byte{0xAA, byte(ai)}})
		}
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	src := buildKeyframeClusteredMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	dir, plan := assertPlanMatchesFullPass(t, src, 2000)
	fts := plan.fts()
	ctx := context.Background()
	for _, n := range []int{3, 1, 5} {
		cold, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		name := renditionSegment(fts, 1, n)
		got, _, err := cold.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("cold %s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
}
