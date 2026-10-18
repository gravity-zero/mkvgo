package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// failOnceFile fails its first read once armed, as a cancelled request or a 503 does, then reads on.
type failOnceFile struct {
	*os.File
	armed *atomic.Bool
}

func (f failOnceFile) Read(p []byte) (int, error) {
	if f.armed.CompareAndSwap(true, false) {
		return 0, errors.New("connection reset")
	}
	return f.File.Read(p)
}

// TestSubtitleScanDoesNotCacheATransientReadError pins that a read failing once does not break a subtitle track for
// the life of the plan: the next request scans again and serves the cues.
func TestSubtitleScanDoesNotCacheATransientReadError(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 3; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 2000, key: true, data: []byte(fmt.Sprintf("cue numero %d", i+1))})
	}
	sortGenBlocks(gblocks)
	src := buildPlanFixture(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, gblocks, nil)
	var armed atomic.Bool
	fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return failOnceFile{f, &armed}, nil
	}, Stat: os.Stat}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, _, err := plan.Resource(context.Background(), "sub1.vtt"); err == nil {
		t.Fatal("the failing read was not reported")
	}
	vtt, _, err := plan.Resource(context.Background(), "sub1.vtt")
	if err != nil || !bytes.Contains(vtt, []byte("cue numero 3")) {
		t.Fatalf("after one failed read the track answers %v:\n%s", err, vtt)
	}
}
