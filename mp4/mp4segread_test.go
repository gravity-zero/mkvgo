package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// countingReader counts the Read calls made on a byte slice.
type countingReader struct {
	*bytes.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.Reader.Read(p)
}

// A segment's samples are read by runs, straight into the caller's buffer:
// one read for a run of back-to-back samples, one buffered stream over runs
// that small gaps separate - never a read per sample (144 for one 2160p video
// segment, 187 for its audio, before).
func TestReadSampleRunsReadsRunsNotSamples(t *testing.T) {
	file := make([]byte, 1<<20)
	for i := range file {
		file[i] = byte(i*7 + i>>8)
	}
	build := func(sizes []uint32, offs []int64) ([]fragSample, []byte) {
		samples := make([]fragSample, len(sizes))
		var want []byte
		for i, size := range sizes {
			samples[i].size = size
			want = append(want, file[offs[i]:offs[i]+int64(size)]...)
		}
		return samples, want
	}
	cases := []struct {
		name  string
		sizes []uint32
		offs  []int64
		reads int
	}{
		{"one run", []uint32{10, 20, 30}, []int64{100, 110, 130}, 1},
		{"runs far apart", []uint32{10, 20, 30, 5, 5, 40, 1},
			[]int64{100, 110, 130, 200_000, 200_005, 600_000, 600_040}, 3},
		{"runs a few bytes apart", []uint32{10, 20, 30, 5, 5, 40, 1},
			[]int64{100, 110, 130, 1000, 1005, 3000, 3040}, 1},
		{"near runs then a far one", []uint32{10, 20, 5, 40},
			[]int64{100, 110, 9000, 500_000}, 2},
		{"interleaved sample by sample over more than the read-ahead", func() []uint32 {
			s := make([]uint32, 200)
			for i := range s {
				s[i] = 3000
			}
			return s
		}(), func() []int64 {
			o := make([]int64, 200)
			for i := range o {
				o[i] = 1000 + int64(i)*4500
			}
			return o
		}(), 4},
		{"a sample stored before the previous one", []uint32{10, 10}, []int64{5000, 100}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples, want := build(tc.sizes, tc.offs)
			src := &countingReader{Reader: bytes.NewReader(file)}
			got := make([]byte, len(want))
			if err := readSampleRuns(context.Background(), src, samples, tc.offs, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("the bytes read are not the samples' bytes")
			}
			if src.reads != tc.reads {
				t.Errorf("%d reads, want %d", src.reads, tc.reads)
			}
		})
	}

	// A sample the source does not hold in full is an error, never a segment
	// padded with whatever the buffer held - alone or inside a span.
	for _, offs := range [][]int64{{1<<20 - 10}, {1<<20 - 500, 1<<20 - 10}} {
		samples := make([]fragSample, len(offs))
		for i := range samples {
			samples[i].size = 20
		}
		err := readSampleRuns(context.Background(), bytes.NewReader(file), samples, offs, make([]byte, 20*len(offs)))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("a sample running past the end of the source (%d samples): err = %v, want io.ErrUnexpectedEOF", len(offs), err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	samples, _ := build([]uint32{10}, []int64{100})
	if err := readSampleRuns(ctx, bytes.NewReader(file), samples, []int64{100}, make([]byte, 10)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
}
