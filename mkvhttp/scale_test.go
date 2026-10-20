package mkvhttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

// scaleSource is n seconds of video, a keyframe every two, under a cue a second stating no duration, in 10 s clusters.
func scaleSource(t testing.TB, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scale.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, h := uint32(320), uint32(240)
	avcc := []byte{0x01, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
	}
	if err := mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "t", WritingApp: "t"}}, tracks, int64(n)*1000); err != nil {
		t.Fatal(err)
	}
	for at := 0; at < n; at += 10 {
		var blocks []mkv.Block
		for i := at; i < min(at+10, n); i++ {
			blocks = append(blocks,
				mkv.Block{TrackNumber: 1, Timecode: int64(i) * 1000, Keyframe: i%2 == 0, Data: []byte{0, 0, 0, 1, 0x65, byte(i)}},
				mkv.Block{TrackNumber: 2, Timecode: int64(i)*1000 + 100, Keyframe: true, Data: []byte(fmt.Sprintf("cue %d", i))})
		}
		if err := mw.WriteClusterWithCues(int64(at)*1000, 1000000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Serving a whole resource through the handler - the full WebVTT rendition here, read through its handle - costs about
// four times as much for a track four times as long.
func TestHandlerWholeResourceScalesLinearly(t *testing.T) {
	costguard.Linear(t, 40000, func(n int) time.Duration {
		plan, err := mp4.PlanHLS(context.Background(), scaleSource(t, n), mp4.Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(Handler(plan))
		defer srv.Close()
		start := time.Now()
		resp, err := http.Get(srv.URL + "/sub1.vtt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET sub1.vtt: %d", resp.StatusCode)
		}
		return costguard.Since(start)
	})
}
