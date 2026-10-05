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
// one read for a run of back-to-back samples - never a read per sample (144
// for one 2160p video segment, before) - and one buffered stream over runs
// whose gaps are no longer than the run they lead to. A small run behind a
// large gap is read on its own: bridging it reads the gap for nothing.
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
		{"a small gap before a larger run is read through", []uint32{10, 20, 5000, 40},
			[]int64{100, 110, 1000, 500_000}, 2},
		{"a gap longer than the run behind it is not", []uint32{10, 20, 30, 5, 5, 40, 1},
			[]int64{100, 110, 130, 1000, 1005, 3000, 3040}, 3},
		{"video interleaved sample by sample: 3000-byte samples, 1500-byte gaps", func() []uint32 {
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
		{"audio interleaved sample by sample: 1500-byte samples, 3000-byte gaps", func() []uint32 {
			s := make([]uint32, 50)
			for i := range s {
				s[i] = 1500
			}
			return s
		}(), func() []int64 {
			o := make([]int64, 50)
			for i := range o {
				o[i] = 1000 + int64(i)*4500
			}
			return o
		}(), 50},
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
	// padded with whatever the buffer held - alone or inside a stream.
	for _, tc := range []struct {
		sizes []uint32
		offs  []int64
	}{
		{[]uint32{20}, []int64{1<<20 - 10}},
		{[]uint32{20, 800}, []int64{1<<20 - 900, 1<<20 - 400}},
	} {
		samples := make([]fragSample, len(tc.sizes))
		var total int
		for i, size := range tc.sizes {
			samples[i].size = size
			total += int(size)
		}
		err := readSampleRuns(context.Background(), bytes.NewReader(file), samples, tc.offs, make([]byte, total))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("a sample running past the end of the source (%d samples): err = %v, want io.ErrUnexpectedEOF", len(tc.sizes), err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	samples, _ := build([]uint32{10}, []int64{100})
	if err := readSampleRuns(ctx, bytes.NewReader(file), samples, []int64{100}, make([]byte, 10)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
}
