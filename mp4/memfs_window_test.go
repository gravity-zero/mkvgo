package mp4

import (
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// The wasm build serves through mkv.MemFS (a Uint8Array input) or a Blob-backed
// FS (a File input), where every Read is work the browser has to do - a copy out
// of the heap, or an actual range request against the Blob. The shared window
// walk is therefore worth MORE there than on a server, not less: a viewer's audio
// segment costs no read at all. This gates that the saving survives the FS the
// browser actually uses.
func TestWasmMemFSPathReadsTheWindowOnce(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	build := func(budget int64) (*HLSPlan, *readTally) {
		m := mkv.NewMemFS()
		m.Put("in", raw)
		fs := m.FS()
		tally := &readTally{}
		open := fs.Open
		fs.Open = func(path string) (mkv.ReadSeekCloser, error) {
			f, err := open(path)
			if err != nil {
				return nil, err
			}
			return &countingFile{f: f, t: tally}, nil
		}
		p, err := PlanHLS(context.Background(), "in", Options{SegmentMs: 6000, FS: fs, WindowCacheBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		return p, tally
	}

	ctx := context.Background()
	serve := func(p *HLSPlan, tally *readTally) (int64, int) {
		*tally = readTally{} // the plan is built: measure the serving alone
		audio := p.videoIndex() + 1
		for i := 0; i < p.NumSegments(); i++ {
			if _, err := p.Segment(ctx, i); err != nil {
				t.Fatal(err)
			}
			if _, err := p.segmentTrack(ctx, audio, i); err != nil {
				t.Fatal(err)
			}
		}
		return tally.bytes, tally.reads
	}

	shared, sharedTally := build(0)
	sharedBytes, sharedReads := serve(shared, sharedTally)

	solo, soloTally := build(-1) // sharing off: every rendition re-walks its window
	soloBytes, soloReads := serve(solo, soloTally)

	if sharedBytes*3 > soloBytes*2 { // expect ~half, allow slack
		t.Errorf("through MemFS the shared walk read %d bytes and separate walks %d: the browser path is not getting the saving",
			sharedBytes, soloBytes)
	}
	t.Logf("MemFS (the wasm input path): shared walk %.2f MiB in %d reads; a walk per rendition %.2f MiB in %d reads",
		float64(sharedBytes)/(1<<20), sharedReads, float64(soloBytes)/(1<<20), soloReads)
}
