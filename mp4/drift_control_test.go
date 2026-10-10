package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// driftFrame is one source frame: its play time and whether it opens a block.
type driftFrame struct {
	tc    int64
	start bool
}

// sourceFrames walks the source header-only: per track its frames in order, and the renditions' track order (video, then audio).
func sourceFrames(t testing.TB, src string) (map[uint64][]driftFrame, []uint64) {
	t.Helper()
	c, err := reader.OpenMeta(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var order []uint64
	for _, tr := range c.Tracks {
		if tr.Type == mkv.VideoTrack {
			order = append(order, tr.ID)
		}
	}
	for _, tr := range c.Tracks {
		if tr.Type == mkv.AudioTrack {
			order = append(order, tr.ID)
		}
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReaderAt(f, c.Info.TimecodeScale, c.SegmentStart)
	if err != nil {
		t.Fatal(err)
	}
	br.SetTrackDefaultDurations(reader.TrackDefaultDurations(c.Tracks))
	br.SetHeaderOnly(true)
	frames := map[uint64][]driftFrame{}
	lastBlock := map[uint64]int64{}
	for {
		b, err := br.Next()
		if err != nil {
			break
		}
		prev, seen := lastBlock[b.TrackNumber]
		frames[b.TrackNumber] = append(frames[b.TrackNumber], driftFrame{tc: b.Timecode, start: !seen || b.BlockTimecode != prev})
		lastBlock[b.TrackNumber] = b.BlockTimecode
	}
	return frames, order
}

// segmentPTS returns a media segment's per-sample presentation times in ticks (tfdt + decode durations + composition offsets).
func segmentPTS(seg []byte) []int64 {
	var tfdt int64
	var durs, cts []int64
	var walk func(b []byte)
	walk = func(b []byte) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b[:4]))
			typ := string(b[4:8])
			head := 8
			if size == 1 {
				size, head = int(binary.BigEndian.Uint64(b[8:16])), 16
			}
			if size == 0 {
				size = len(b)
			}
			if size < head || size > len(b) {
				return
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
				flags := binary.BigEndian.Uint32(body[:4]) & 0xFFFFFF
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
	pts := make([]int64, len(durs))
	clock := tfdt
	for i := range durs {
		pts[i] = clock + cts[i]
		clock += durs[i]
	}
	return pts
}

// mdhdTimescale reads the first mdhd's timescale of an init segment.
func mdhdTimescale(init []byte) int64 {
	i := strings.Index(string(init), "mdhd")
	if i < 0 {
		return 0
	}
	if init[i+4] == 1 {
		return int64(binary.BigEndian.Uint32(init[i+24 : i+28]))
	}
	return int64(binary.BigEndian.Uint32(init[i+16 : i+20]))
}

// assertNoDrift checks every rendition of an HLS output against the source: at
// each frame that opens a block, the output presentation time (relative to the
// first) must sit within one frame duration plus a millisecond of the source
// timecode (relative to the first), over the whole presentation. fetch returns
// a resource's bytes; mode names the path under test in failures.
func assertNoDrift(t testing.TB, src, mode string, fetch func(name string) []byte) {
	t.Helper()
	frames, order := sourceFrames(t, src)
	master := string(fetch("master.m3u8"))
	var playlists []string
	for _, l := range strings.Split(master, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasSuffix(l, ".m3u8") && !strings.HasPrefix(l, "#") {
			playlists = append(playlists, l)
		}
		if i := strings.Index(l, `URI="`); i >= 0 && strings.Contains(l, "TYPE=AUDIO") {
			u := l[i+5:]
			playlists = append(playlists, u[:strings.Index(u, `"`)])
		}
	}
	sort.Strings(playlists)
	for _, pl := range playlists {
		var id uint64
		switch {
		case pl == "playlist.m3u8":
			id = order[0]
		case strings.HasPrefix(pl, "audio"):
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(pl, "audio"), ".m3u8"))
			if n < 1 || n >= len(order) {
				continue
			}
			id = order[n]
		default:
			continue
		}
		var initName string
		var segs []string
		for _, l := range strings.Split(string(fetch(pl)), "\n") {
			switch {
			case strings.HasPrefix(l, `#EXT-X-MAP:URI="`):
				initName = strings.TrimSuffix(strings.TrimPrefix(l, `#EXT-X-MAP:URI="`), `"`)
			case strings.HasSuffix(strings.TrimSpace(l), ".m4s"):
				segs = append(segs, strings.TrimSpace(l))
			}
		}
		ts := mdhdTimescale(fetch(initName))
		var pts []int64
		for _, s := range segs {
			pts = append(pts, segmentPTS(fetch(s))...)
		}
		src := frames[id]
		if len(pts) != len(src) {
			t.Errorf("%s %s: %d output samples for %d source frames", mode, pl, len(pts), len(src))
			continue
		}
		var gaps []float64
		last := -1
		for i, f := range src {
			if f.start {
				if last >= 0 && f.tc > src[last].tc {
					gaps = append(gaps, float64(f.tc-src[last].tc)/float64(i-last))
				}
				last = i
			}
		}
		frameMs := 0.0
		if len(gaps) > 0 {
			sort.Float64s(gaps)
			frameMs = gaps[len(gaps)/2]
		}
		var maxDev float64
		var at int64
		for i, f := range src {
			if !f.start {
				continue
			}
			dev := float64(pts[i]-pts[0])*1000/float64(ts) - float64(f.tc-src[0].tc)
			if dev < 0 {
				dev = -dev
			}
			if dev > maxDev {
				maxDev, at = dev, f.tc
			}
		}
		if maxDev > frameMs+1 {
			t.Errorf("%s %s: output drifts %.3f ms from the source at %d ms (frame %.3f ms)", mode, pl, maxDev, at, frameMs)
		}
	}
}

// assertNoDriftBothModes runs the drift control on the full pass's output and on the on-demand plan's resources.
func assertNoDriftBothModes(t testing.TB, src string, segMs int64) {
	t.Helper()
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "hls")
	if err := RemuxToHLS(ctx, src, out, Options{SegmentMs: segMs}); err != nil {
		t.Fatal(err)
	}
	assertNoDrift(t, src, "full pass", func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("full pass lacks %s: %v", name, err)
		}
		return b
	})
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: segMs})
	if err != nil {
		t.Fatal(err)
	}
	assertNoDrift(t, src, "plan", func(name string) []byte {
		b, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatalf("plan lacks %s: %v", name, err)
		}
		return b
	})
}

// The output timeline follows the source's timecodes to within a frame over
// the whole presentation, in both modes, for laced audio with and without a
// DefaultDuration and for an open-GOP video.
func TestOutputNeverDriftsFromTheSource(t *testing.T) {
	for name, build := range map[string]func(testing.TB) string{
		"laced AAC with DefaultDuration":    func(tb testing.TB) string { p, _ := buildLacedFixtureOpt(tb, true); return p },
		"laced AAC without DefaultDuration": func(tb testing.TB) string { p, _ := buildLacedFixtureOpt(tb, false); return p },
	} {
		t.Run(name, func(t *testing.T) { assertNoDriftBothModes(t, build(t), 2000) })
	}
}

// buildOpusLacedFixture writes laced 21.33 ms frames under a codec no table
// knows (Opus here, whose real frames are whole milliseconds), without a
// DefaultDuration, with trusted statistics naming the frame count: the
// whole-track stride must settle the timeline where two neighbouring blocks
// cannot.
func buildOpusLacedFixture(t testing.TB, withStats bool) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opus.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale, durMs = 1_000_000, 60_000
	sr := 48000.0
	ch := uint8(2)
	w, h := uint32(320), uint32(240)
	opusHead := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0, 0x80, 0xBB, 0, 0, 0, 0, 0}
	tracks := []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, UID: 22, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead, SampleRate: &sr, Channels: &ch},
	}
	blocks := 0
	for lacedAudioBlockPts(blocks+1) < durMs {
		blocks++
	}
	frames := blocks * 8
	var tags []mkv.Tag
	if withStats {
		tags = []mkv.Tag{{TargetID: 22, SimpleTags: []mkv.SimpleTag{
			{Name: "_STATISTICS_WRITING_APP", Value: "mkvgo-test"}, {Name: "DURATION", Value: "00:00:59.900000000"}, {Name: "NUMBER_OF_FRAMES", Value: strconv.Itoa(frames)}}}}
	}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}, Tags: tags}
	if err := m.WriteMetadata(c, tracks, durMs); err != nil {
		t.Fatal(err)
	}
	audioBlocks := 0
	for ts := int64(0); ts < durMs; ts += 1000 {
		var cl bytes.Buffer
		cl.Write(rawUintElem(mkv.IDTimestamp, uint64(ts), 2))
		for i := 0; i < 25; i++ {
			writeRawBlock(&cl, 1, int16(int64(i)*40), i == 0, append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, byte(ts/1000), byte(i)))
		}
		for audioBlocks < blocks {
			pts := lacedAudioBlockPts(audioBlocks)
			if pts >= ts+1000 {
				break
			}
			fr := make([][]byte, 8)
			for i := range fr {
				fr[i] = []byte{0xFC, byte(audioBlocks), byte(i)}
			}
			writeLacedRawBlock(&cl, 2, int16(pts-ts), fr)
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
	return path, frames
}

// A codec no header or table sizes, laced without a DefaultDuration: with a
// trusted frame count the stride is measured over the whole track and the
// audio lasts exactly its frames in both modes; without one both modes agree
// on the local measurement.
func TestUnknownCodecStrideMeasuredOverTheWholeTrack(t *testing.T) {
	ctx := context.Background()
	for _, withStats := range []bool{true, false} {
		src, frames := buildOpusLacedFixture(t, withStats)
		out := filepath.Join(t.TempDir(), "hls")
		if err := RemuxToHLS(ctx, src, out, Options{SegmentMs: 2000}); err != nil {
			t.Fatal(err)
		}
		full, err := os.ReadFile(filepath.Join(out, "init_a1.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		if withStats {
			init, _, err := plan.Resource(ctx, "init_a1.mp4")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(init, full) {
				t.Errorf("plan init duration %d, full pass %d", mdhdDuration(t, init), mdhdDuration(t, full))
			}
			if want := int64(frames) * 1024; mdhdDuration(t, full) != want {
				t.Errorf("with trusted statistics: audio duration %d ticks, want %d (%d frames of 1024)", mdhdDuration(t, full), want, frames)
			}
			assertNoDriftBothModes(t, src, 2000)
			continue
		}
		// Without a count both modes measure the same local stride and open
		// alike; further on they part, the full pass on a running clock the
		// unconfirmed stride lets drift, the plan on each window's timecodes.
		// This is the one class left to a trusted count: a codec no header or
		// table sizes, laced, without a DefaultDuration.
		for _, name := range []string{"seg_a1_00001.m4s", "seg_a1_00002.m4s"} {
			got, _, err := plan.Resource(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("without statistics: %s differs between the plan and the full pass", name)
			}
		}
	}
}
