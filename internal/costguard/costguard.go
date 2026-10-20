// Package costguard holds the tests to what an input may cost: a fuzzed input a time budget that grows with its size,
// and a path a linear growth, so a crafted file cannot make the toolkit spend what its bytes do not account for.
package costguard

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// Budget is what an input of size bytes may cost: a second for setup plus 20 ms a KiB, both wide, and raceFactor times
// that under the race detector.
func Budget(size int) time.Duration {
	return time.Duration(raceFactor) * (time.Second + time.Duration(size)*20*time.Microsecond)
}

// fuzzing reports a -fuzz run: the seeds that run as ordinary tests (CI, -race, -parallel) are not timed.
func fuzzing() bool {
	f := flag.Lookup("test.fuzz")
	return f != nil && f.Value.String() != ""
}

// Fuzz runs one fuzz input's body, fn, in the test's goroutine and kills the process when it outlasts Budget(size):
// the fuzzer keeps the input as it keeps a crasher, a hang included, which a check after fn could not catch.
func Fuzz(t testing.TB, size int, fn func()) {
	t.Helper()
	if !fuzzing() {
		fn()
		return
	}
	budget := Budget(size)
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(budget):
			fmt.Fprintf(os.Stderr, "costguard: a %d-byte input outlasted its %v budget\n", size, budget)
			panic(fmt.Sprintf("costguard: a %d-byte input outlasted its %v budget", size, budget))
		}
	}()
	defer close(done)
	fn()
}

// Linear fails t when the measured phase of fn at 4n items costs more than 8 times its cost at n: a linear path costs
// about 4 times, n log n about 4.5, a quadratic one 16. Each size keeps the best of three runs, and a ratio past 8 is
// measured again, the best of five, so a collection pause fails nothing a quadratic path would not. fn builds its
// input of n items, then runs and times the phase under test, and returns that time, so a linear setup cannot hide
// what it feeds; n is chosen so that phase lasts 10 ms or more. Skipped under the race detector, whose overhead is not
// proportional.
func Linear(t testing.TB, n int, fn func(n int) time.Duration) {
	t.Helper()
	if raceFactor > 1 {
		t.Skip("costguard.Linear does not time under the race detector")
	}
	best := func(n, runs int) time.Duration {
		b := time.Duration(1<<63 - 1)
		for range runs {
			runtime.GC() // each run starts from a collected heap, not from the garbage of the one before
			b = min(b, fn(n))
		}
		return max(b, time.Microsecond)
	}
	small, large := best(n, 3), best(4*n, 3)
	if float64(large)/float64(small) > 8 {
		small, large = min(small, best(n, 5)), min(large, best(4*n, 5))
	}
	if os.Getenv("COSTGUARD_VERBOSE") != "" {
		t.Logf("%v for %d, %v for %d", small, n, large, 4*n)
	}
	if ratio := float64(large) / float64(small); ratio > 8 {
		t.Errorf("4 times the input cost %.1f times as much (%v for %d, %v for %d): the cost grows faster than the input", ratio, small, n, large, 4*n)
	}
}

// Since is time.Since, for an fn handed to Linear that times all it does.
func Since(start time.Time) time.Duration { return time.Since(start) }
