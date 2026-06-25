package reader

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// clusterTS builds a Cluster opening with the given Timestamp (in TimecodeScale
// units, here ms since the fixture uses scale 1e6), padded with a Void so the
// Clusters spread across enough bytes for the byte-offset sampler to separate them.
func clusterTS(ts uint64, pad int) []byte {
	return masterElem(mkv.IDCluster,
		uintElem(mkv.IDTimestamp, ts, 2),
		masterElem(mkv.IDVoid, make([]byte, pad)),
	)
}

// TestSampledKeyframes covers the Cues-less keyframe index: a file with Clusters
// but no Cues yields no Keyframes by default, but WithSampledKeyframes recovers a
// coarse index from the Cluster timestamps.
func TestSampledKeyframes(t *testing.T) {
	children := [][]byte{infoElem(), tracksElem()}
	for _, ts := range []uint64{0, 1000, 2000, 3000} {
		children = append(children, clusterTS(ts, 400))
	}
	file := segmentMKV(children...)

	// Default: no Cues → no Keyframes.
	c, err := ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Keyframes) != 0 {
		t.Errorf("without the option, Keyframes = %v, want none", c.Keyframes)
	}

	// Opted in: Cluster timestamps sampled into a keyframe index.
	c, err = ReadMeta(context.Background(), bytes.NewReader(file), "x.mkv", WithSampledKeyframes(200))
	if err != nil {
		t.Fatalf("ReadMeta sampled: %v", err)
	}
	want := []int64{0, 1000, 2000, 3000}
	if !reflect.DeepEqual(c.Keyframes, want) {
		t.Errorf("sampled Keyframes = %v, want %v", c.Keyframes, want)
	}

	// A file that already has Cues must not be sampled: the option is a no-op, so
	// the keyframe index is identical with and without it. (Cues precede Tracks so
	// the metadata pass reads them inline rather than skipping past them.)
	withCues := segmentMKV(infoElem(), cuesElem(3), tracksElem(), clusterTS(0, 100))
	base, err := ReadMeta(context.Background(), bytes.NewReader(withCues), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta base: %v", err)
	}
	opt, err := ReadMeta(context.Background(), bytes.NewReader(withCues), "x.mkv", WithSampledKeyframes(200))
	if err != nil {
		t.Fatalf("ReadMeta opt: %v", err)
	}
	if len(base.Keyframes) == 0 {
		t.Fatal("the Cues should have produced a keyframe index")
	}
	if !reflect.DeepEqual(base.Keyframes, opt.Keyframes) {
		t.Errorf("with Cues present the option must not change Keyframes: base=%v opt=%v", base.Keyframes, opt.Keyframes)
	}
}
