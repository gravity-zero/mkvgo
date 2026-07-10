package mp4

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// measureServe reports the average allocations and bytes allocated per call of
// fn, over `iters` iterations. Bytes come from a runtime.MemStats delta (which
// AllocsPerRun does not expose). GC is forced first so the delta is clean.
func measureServe(tb testing.TB, iters int, fn func()) (allocs float64, bytesPerOp uint64) {
	tb.Helper()
	// Warm once so any lazy one-time caching (header peek) is not charged.
	fn()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for i := 0; i < iters; i++ {
		fn()
	}
	runtime.ReadMemStats(&m1)
	allocs = float64(m1.Mallocs-m0.Mallocs) / float64(iters)
	bytesPerOp = (m1.TotalAlloc - m0.TotalAlloc) / uint64(iters)
	return allocs, bytesPerOp
}

// TestServeAllocDoesNotScaleWithSourceSize is the core copy-free anti-regression
// gate. Serving a media segment must cost O(segment), not O(source): the
// packager seeks to the segment's byte range and repackages only its samples,
// never buffering the skipped body. We serve the middle segment of a short
// source and of a 10x-longer one (same segment duration, so segments are the
// same size) and assert the per-serve allocation does NOT grow with source
// length. A regression that reads from the start, or holds the whole index/body
// per request, makes the ratio blow up toward 10x and fails here - and it is a
// ratio, so it needs no machine-specific or baseline-specific constant.
func TestServeAllocDoesNotScaleWithSourceSize(t *testing.T) {
	ctx := context.Background()
	serveMiddle := func(seconds int) (float64, uint64) {
		plan, err := PlanHLS(ctx, buildServingSource(t, seconds), Options{SegmentMs: 1000})
		if err != nil {
			t.Fatal(err)
		}
		mid := plan.NumSegments() / 2
		return measureServe(t, 20, func() {
			if _, err := plan.Segment(ctx, mid); err != nil {
				t.Fatal(err)
			}
		})
	}
	smallA, smallB := serveMiddle(20)
	largeA, largeB := serveMiddle(200) // 10x the source

	const maxRatio = 2.0 // generous: real value is ~1x; a source-scaling regression is ~10x
	if ratio := largeA / smallA; ratio > maxRatio {
		t.Errorf("segment-serve allocations scale with source size: %.0f allocs at 20s vs %.0f at 200s (ratio %.2f > %.1f) - serving is no longer O(segment)",
			smallA, largeA, ratio, maxRatio)
	}
	if smallB > 0 {
		if ratio := float64(largeB) / float64(smallB); ratio > maxRatio {
			t.Errorf("segment-serve bytes scale with source size: %d B at 20s vs %d B at 200s (ratio %.2f > %.1f)",
				smallB, largeB, ratio, maxRatio)
		}
	}
	t.Logf("serve middle segment: 20s src = %.0f allocs / %d B ; 200s src = %.0f allocs / %d B", smallA, smallB, largeA, largeB)
}

// TestServeSegmentAllocationCeiling is a coarse absolute backstop with wide
// headroom over the measured baseline (~445 allocs, ~291 KB for a 720p 1s
// segment). It catches a gross per-serve allocation regression that the ratio
// test might miss if it regressed both sizes equally. The ceiling is not a
// tight budget - it is a "something went very wrong" tripwire.
func TestServeSegmentAllocationCeiling(t *testing.T) {
	ctx := context.Background()
	plan, err := PlanHLS(ctx, buildServingSource(t, 60), Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	n := plan.NumSegments()
	i := 0
	allocs, bytesPerOp := measureServe(t, 30, func() {
		if _, err := plan.Segment(ctx, i%n); err != nil {
			t.Fatal(err)
		}
		i++
	})
	const maxAllocs = 1500           // baseline ~445
	const maxBytes = 2 * 1024 * 1024 // baseline ~291 KB
	if allocs > maxAllocs {
		t.Errorf("serve segment allocs/op = %.0f, over ceiling %d", allocs, maxAllocs)
	}
	if bytesPerOp > maxBytes {
		t.Errorf("serve segment B/op = %d, over ceiling %d", bytesPerOp, maxBytes)
	}
	t.Logf("serve segment steady-state: %.0f allocs/op, %d B/op", allocs, bytesPerOp)
}

// TestServingMemoryPerStream measures the heap a single idle stream retains -
// an HLSPlan keeps its index, playlists and init segments resident, but never
// the media body (that is streamed per segment). This retained footprint is the
// real ceiling on concurrent streams: capacity is roughly the memory budget
// divided by it. We build many plans, keep them referenced, and measure the
// live-heap delta per plan after a GC. The ceiling bounds per-stream memory (so
// a regression that retained the source body would collapse the stream count),
// and the log reports the derived "streams per GB" - the portable answer to how
// many simultaneous streams one process serves.
func TestServingMemoryPerStream(t *testing.T) {
	ctx := context.Background()
	src := buildServingSource(t, 60)
	const k = 40

	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	plans := make([]*HLSPlan, k)
	for i := range plans {
		p, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
		if err != nil {
			t.Fatal(err)
		}
		plans[i] = p
	}

	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(plans)

	retainedPerPlan := (m1.HeapAlloc - m0.HeapAlloc) / k
	streamsPerGB := (1 << 30) / maxU64(retainedPerPlan, 1)

	const maxRetained = 4 * 1024 * 1024 // 720p/60s plan retains well under this
	if retainedPerPlan > maxRetained {
		t.Errorf("retained heap per idle stream = %d B, over ceiling %d B - per-stream footprint regressed (fewer concurrent streams)", retainedPerPlan, maxRetained)
	}
	t.Logf("retained per idle stream = %d B (~%d KB) -> ~%d idle streams per GB of plan memory (60s 720p source)", retainedPerPlan, retainedPerPlan>>10, streamsPerGB)
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// TestConcurrentServingByteIdentical proves the on-demand serving path is safe
// under concurrency: many clients serving many plans at once must each get bytes
// identical to a single-threaded golden. Run under `-race` in CI, it is the
// data-race gate for the shared source cursor/mutex in the plan. It mixes HLS
// copy-rung segments, ABR variants and the DASH manifest - the qualities served
// from one source with no copy.
func TestConcurrentServingByteIdentical(t *testing.T) {
	ctx := context.Background()
	src := buildServingSource(t, 30)

	// A handful of independent plans (independent streams) plus one shared ABR
	// presentation whose variants are served concurrently (contended cursor).
	const nPlans = 6
	type job struct {
		name    string
		serve   func(res string) ([]byte, error)
		golden  map[string][]byte
		reslist []string
	}
	newHLSJob := func(label string) job {
		p, err := PlanHLS(ctx, src, Options{SegmentMs: 1000})
		if err != nil {
			t.Fatal(err)
		}
		j := job{name: label, golden: map[string][]byte{}}
		j.serve = func(res string) ([]byte, error) { b, _, err := p.Resource(ctx, res); return b, err }
		j.reslist = p.Resources()
		return j
	}
	abr, err := PlanABR(ctx, []string{src, src}, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	abrJob := job{name: "abr", golden: map[string][]byte{}}
	abrJob.serve = func(res string) ([]byte, error) { b, _, err := abr.Resource(ctx, res); return b, err }
	abrJob.reslist = abr.Resources()

	jobs := []job{abrJob}
	for k := 0; k < nPlans; k++ {
		jobs = append(jobs, newHLSJob(fmt.Sprintf("hls%d", k)))
	}

	// Golden: serve every resource of every job once, single-threaded.
	for ji := range jobs {
		for _, res := range jobs[ji].reslist {
			b, err := jobs[ji].serve(res)
			if err != nil {
				t.Fatalf("%s golden %s: %v", jobs[ji].name, res, err)
			}
			cp := make([]byte, len(b))
			copy(cp, b)
			jobs[ji].golden[res] = cp
		}
	}

	// Concurrent: many goroutines each replay a job's full resource list and
	// compare against the golden. Several goroutines target the same job so the
	// shared cursor is genuinely contended.
	var wg sync.WaitGroup
	errCh := make(chan string, 256)
	const workersPerJob = 4
	for ji := range jobs {
		for w := 0; w < workersPerJob; w++ {
			wg.Add(1)
			go func(j job, seed int) {
				defer wg.Done()
				// Rotate the start so workers on the same job interleave differently.
				rl := j.reslist
				for off := 0; off < len(rl); off++ {
					res := rl[(off+seed)%len(rl)]
					b, err := j.serve(res)
					if err != nil {
						errCh <- fmt.Sprintf("%s %s: serve error %v", j.name, res, err)
						return
					}
					want := j.golden[res]
					if len(b) != len(want) {
						errCh <- fmt.Sprintf("%s %s: len %d != golden %d", j.name, res, len(b), len(want))
						return
					}
					for i := range b {
						if b[i] != want[i] {
							errCh <- fmt.Sprintf("%s %s: byte %d differs under concurrency", j.name, res, i)
							return
						}
					}
				}
			}(jobs[ji], w)
		}
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Error(msg)
	}
}
