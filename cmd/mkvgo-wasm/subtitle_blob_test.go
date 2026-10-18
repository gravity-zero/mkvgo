//go:build js && wasm

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall/js"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestExtractSubtitleVTTTakesABlob pins that the WebVTT export reads a Blob by ranges, as the other Blob-backed
// exports do, and gives what it gives from the same bytes in memory.
func TestExtractSubtitleVTTTakesABlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, h := uint32(64), uint32(48)
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}},
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h}, {ID: 2, Type: mkv.SubtitleTrack, Codec: "srt"}}, 3000); err != nil {
		t.Fatal(err)
	}
	for tc := int64(0); tc < 3000; tc += 1000 {
		if err := mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte{0, 0, 0, 1, 0x65}},
			{TrackNumber: 2, Timecode: tc + 100, Keyframe: true, Duration: 500, Data: []byte("line at " + string(rune('0'+tc/1000)))},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u8 := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(u8, data)
	blob := js.Global().Get("Blob").New([]any{u8})
	fromMem, err1 := await(js.ValueOf(extractSubtitleVTTJS(js.Undefined(), []js.Value{u8, js.ValueOf(2)})))
	fromBlob, err2 := await(js.ValueOf(extractSubtitleVTTJS(js.Undefined(), []js.Value{blob, js.ValueOf(2)})))
	if err1 != nil || err2 != nil {
		t.Fatalf("Uint8Array: %v; Blob: %v", err1, err2)
	}
	if fromBlob.String() != fromMem.String() || !strings.Contains(fromBlob.String(), "line at 2") {
		t.Errorf("Blob gave\n%s\nUint8Array gave\n%s", fromBlob.String(), fromMem.String())
	}
}
