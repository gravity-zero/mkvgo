package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// readTally counts the real I/O a serving path drives against the source file:
// bytes handed back by Read (the rchar a kernel would charge the process),
// the Read call count, and the Seek count. Read amplification - bytes read per
// byte served - is what caps a direct-play server: its ceiling is the storage
// bandwidth, so every byte read and thrown away is a viewer not served.
type readTally struct {
	bytes int64
	reads int
	seeks int
}

type countingFile struct {
	f mkv.ReadSeekCloser
	t *readTally
}

func (c *countingFile) Read(p []byte) (int, error) {
	n, err := c.f.Read(p)
	c.t.bytes += int64(n)
	c.t.reads++
	return n, err
}

func (c *countingFile) Seek(off int64, whence int) (int64, error) {
	c.t.seeks++
	return c.f.Seek(off, whence)
}

func (c *countingFile) Close() error { return c.f.Close() }

func countingFS(t *readTally) *mkv.FS {
	return &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &countingFile{f: f, t: t}, nil
	}}
}

// buildInterleavedSource writes the layout a real muxer produces and a serving
// path pays for: several seconds of video per cluster (so one cluster holds
// more than one HLS segment), a cue per video keyframe (not per cluster - that
// is what puts two segment starts inside one cluster), and heavy interleaved
// audio (two tracks, the VF+VO of a real release) whose bytes a video segment
// must step over.
//
// Sizes are scaled down but keep the proportions that decide the I/O: a
// keyframe an order above a delta frame, delta frames and audio blocks both
// well under the reader's seek-vs-read threshold.
func buildInterleavedSource(tb testing.TB, seconds int) string {
	tb.Helper()
	const (
		fps        = 25
		gopSec     = 2  // keyframe (and cue) every 2s
		clusterSec = 10 // one cluster spans several segments
		keyBytes   = 60 << 10
		deltaBytes = 12 << 10
		audioBytes = 3 << 10
		scale      = 1_000_000
	)
	sample := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed + byte(i)
		}
		return b
	}
	w, h := uint32(1920), uint32(1080)
	sr := 48000.0
	ch := uint8(6)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
		{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"},
		{ID: 4, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Name: "VF"},
	}

	path := filepath.Join(tb.TempDir(), "interleaved.mkv")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		tb.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, int64(seconds)*1000); err != nil {
		tb.Fatal(err)
	}

	for cs := 0; cs < seconds; cs += clusterSec {
		clusterTS := int64(cs) * 1000
		clusterPos := m.RelPos()
		var blks []mkv.Block
		for f := cs * fps; f < (cs+clusterSec)*fps && f < seconds*fps; f++ {
			ms := int64(f) * 1000 / fps
			key := f%(gopSec*fps) == 0
			data := sample(deltaBytes, byte(f))
			if key {
				data = sample(keyBytes, byte(f))
				// A real muxer cues every video keyframe, so a 10s cluster
				// carries 5 cue points - and two HLS segment starts.
				m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: ms, Track: 1, ClusterPos: clusterPos})
			}
			blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: ms, Keyframe: key, Data: data})
			blks = append(blks, mkv.Block{TrackNumber: 2, Timecode: ms, Keyframe: true, Data: sample(audioBytes, byte(f+1))})
			blks = append(blks, mkv.Block{TrackNumber: 3, Timecode: ms, Keyframe: true, Data: sample(audioBytes, byte(f+2))})
			if f%fps == 0 {
				blks = append(blks, mkv.Block{TrackNumber: 4, Timecode: ms, Keyframe: true, Data: []byte("subtitle line")})
			}
		}
		if err := writer.WriteCluster(m.W, clusterTS, scale, blks); err != nil {
			tb.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		tb.Fatal(err)
	}
	return path
}

// TestServingReadAmplificationCeiling gates what serving consecutive segments
// costs in source bytes read. A cluster holds several segments here (as in any
// real muxing), so a walk that could only open on the cluster header re-read
// its prefix once per segment inside it: the source went over the bus a
// multiple of the bytes served. Playback order must now read each byte once -
// the residue above 1.0 is the interleaved audio a video window steps over,
// nothing more.
func TestServingReadAmplificationCeiling(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	var tally readTally
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, FS: countingFS(&tally)})
	if err != nil {
		t.Fatal(err)
	}

	tally = readTally{} // plan built: measure the serving alone
	var served int64
	n := plan.NumSegments()
	for i := 0; i < n; i++ {
		data, err := plan.Segment(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		served += int64(len(data))
	}
	amp := float64(tally.bytes) / float64(served)
	t.Logf("source %.1f MiB, %d segments: %.2f MiB served, %.2f MiB read (%d reads) -> %.2fx",
		float64(st.Size())/(1<<20), n, float64(served)/(1<<20), float64(tally.bytes)/(1<<20), tally.reads, amp)

	// The window's own audio+subtitle bytes are the only legitimate surplus
	// (~24% of this source): they are interleaved between the video blocks
	// the window does serve, so a walk steps over them where it cannot seek
	// past them. Anything approaching 2x means a segment is reading a
	// neighbour's bytes again.
	if amp > 1.75 {
		t.Errorf("read amplification %.2fx: serving reads the source several times over", amp)
	}
	// Same statement from the source's side, and the one that decides a
	// direct-play server's ceiling: playing a file from end to end must read
	// it ONCE. Serving through cluster starts alone read it 1.7 times over.
	if over := float64(tally.bytes) / float64(st.Size()); over > 1.15 {
		t.Errorf("a full playthrough read the source %.2f times over", over)
	}
}

// A COLD plan hit concurrently is the race the learned starts introduce: two
// goroutines walk the same stretch at once and both try to record the block a
// window opens on. They must agree (a boundary is a property of the source, not
// of who found it), and every segment must still come out byte-identical to the
// cold reference. The existing concurrency gate warms the plan single-threaded
// before it forks, so it never runs this path.
func TestConcurrentColdPlanLearnsConsistently(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	opts := Options{SegmentMs: 6000}

	ref, err := PlanHLS(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	n := ref.NumSegments()
	want := make([][]byte, n)
	for i := 0; i < n; i++ {
		fresh, err := PlanHLS(ctx, src, opts) // cold every time: cluster path
		if err != nil {
			t.Fatal(err)
		}
		if want[i], err = fresh.Segment(ctx, i); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := PlanHLS(ctx, src, opts) // never served: the cache is empty
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for k := 0; k < n; k++ {
				i := (k + seed) % n // workers enter the segment ring at different points
				got, err := plan.Segment(ctx, i)
				if err != nil {
					errs <- fmt.Sprintf("segment %d: %v", i, err)
					return
				}
				if !bytes.Equal(got, want[i]) {
					errs <- fmt.Sprintf("segment %d: %d bytes, differs from the cold segment (%d bytes)", i, len(got), len(want[i]))
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestSegmentBytesIdenticalColdAndLearned is the correctness gate under the
// optimisation: a segment served through a learned opening block must be the
// same bytes as the same segment served cold, straight from its cluster (a
// fresh plan, no walk before it). Every serving order - sequential, a seek
// backwards, a repeat - must agree with the cold bytes, for the video
// rendition and for an audio one (whose blocks a source may lace).
func TestSegmentBytesIdenticalColdAndLearned(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	opts := Options{SegmentMs: 6000}

	cold, err := PlanHLS(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	n := cold.NumSegments()
	tracks := len(cold.tracks) // video + the two audio renditions

	// Cold reference: one fresh plan per segment, so no walk has ever revealed
	// where a window opens - every one of these takes the cluster path.
	want := make([][][]byte, tracks)
	for ti := 0; ti < tracks; ti++ {
		want[ti] = make([][]byte, n)
		for i := 0; i < n; i++ {
			fresh, err := PlanHLS(ctx, src, opts)
			if err != nil {
				t.Fatal(err)
			}
			data, err := fresh.segmentTrack(ctx, ti, i)
			if err != nil {
				t.Fatalf("cold track %d segment %d: %v", ti, i, err)
			}
			want[ti][i] = data
		}
	}

	// Orders that exercise the learned starts: straight playback (each walk
	// hands the next its opening block), a backwards seek onto segments whose
	// start is known, a jump into unvisited ground, and repeats.
	orders := map[string][]int{
		"sequential": {0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		"seek-back":  {5, 6, 1, 2, 9, 0, 5, 6},
		"reverse":    {9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
	}
	for name, order := range orders {
		for ti := 0; ti < tracks; ti++ {
			plan, err := PlanHLS(ctx, src, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range order {
				if i >= n {
					continue
				}
				got, err := plan.segmentTrack(ctx, ti, i)
				if err != nil {
					t.Fatalf("%s track %d segment %d: %v", name, ti, i, err)
				}
				if !bytes.Equal(got, want[ti][i]) {
					t.Fatalf("%s track %d segment %d: %d bytes, differs from the cold segment (%d bytes)",
						name, ti, i, len(got), len(want[ti][i]))
				}
			}
		}
	}
}
