package ops

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

type countedFile struct {
	*os.File
	n *int64
}

func (f countedFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	*f.n += int64(n)
	return n, err
}

// AudioStartDelays reads the first clusters, not what the muxer stored before them: the attachments (a subtitle's
// fonts, tens of MB) were read through on every call, from a NAS or over the network as much as from a disk.
func TestAudioStartDelaysSkipsTheAttachments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fonts.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	font := make([]byte, 20<<20)
	c := &mkv.Container{
		Info:        mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Attachments: []mkv.Attachment{{ID: 1, Name: "font.ttf", MIMEType: "font/ttf", Size: int64(len(font)), Data: font}},
	}
	if err := mw.WriteMetadata(c, []mkv.Track{videoTrack(1), audioTrack(2)}, 4000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		if err := mw.WriteClusterWithCues(ts, 1000000, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var read int64
	fs := &mkv.FS{Open: func(p string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return countedFile{File: f, n: &read}, nil
	}}
	delays, err := AudioStartDelays(context.Background(), path, mkv.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	if delays[2] != 300_000_000 {
		t.Errorf("delays = %+v, want track 2 = 300ms", delays)
	}
	if read > 2<<20 {
		t.Errorf("%d bytes read for a file whose clusters weigh a few hundred: the 20 MiB attachment was read through", read)
	}
}

// A cluster's declared size allocates nothing the file does not hold: a few bytes declaring 200 MiB allocated it.
func TestAudioStartDelaysAllocatesNoDeclaredSize(t *testing.T) {
	data := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x80, // EBML header, empty
		0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // Segment, unknown size
		0x1F, 0x43, 0xB6, 0x75, 0x01, 0x00, 0x00, 0x00, 0x0C, 0x80, 0x00, 0x00, // Cluster declaring 200 MiB
		0xE7, 0x81, 0x00} // its Timestamp, then the file ends
	path := filepath.Join(t.TempDir(), "forged.mkv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, _, err := scanFirstBlockTimes(context.Background(), path, mkv.FSFrom(nil), map[uint64]bool{2: true}, map[uint64]bool{1: true}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Errorf("%d bytes allocated for a %d-byte file", got, len(data))
	}
}
