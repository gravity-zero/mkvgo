package writer

import (
	"io"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// discardSeeker is an io.WriteSeeker that keeps nothing.
type discardSeeker struct{ pos int64 }

func (d *discardSeeker) Write(p []byte) (int, error) { d.pos += int64(len(p)); return len(p), nil }

func (d *discardSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		d.pos = off
	case io.SeekCurrent:
		d.pos += off
	}
	return d.pos, nil
}

// TestClusterWriteReusesItsBuffer: an MKVWriter assembles every cluster in one
// buffer it keeps; a fresh buffer per cluster, grown by doubling, allocated
// about twice the media it wrote (4.4 GB for a 1.8 GB remux).
func TestClusterWriteReusesItsBuffer(t *testing.T) {
	mw := NewMKVWriter(&discardSeeker{})
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "V_VP9", Width: new(uint32), Height: new(uint32)}
	if err := mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}, []mkv.Track{video}, 64_000); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 256<<10)
	const clusters = 64
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := int64(0); i < clusters; i++ {
		blocks := []mkv.Block{{TrackNumber: 1, Timecode: i * 1000, Keyframe: true, Data: payload}}
		if err := mw.WriteClusterWithCues(i*1000, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	written := int64(clusters * len(payload))
	if alloc := int64(after.TotalAlloc - before.TotalAlloc); alloc > written/8 {
		t.Errorf("writing %d bytes of clusters allocated %d bytes, want <= %d", written, alloc, written/8)
	}
}
