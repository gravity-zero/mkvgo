package commands_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
)

// zz_fixtures_test.go - shared test fixtures for the CLI command integration
// tests. richMKV/richContainer give a metadata-rich Matroska file (no media
// frames) for the inspection and editing commands; sampleMKV / sampleMP4 give a
// real file with frames (copied from internal/testdata) for the commands that
// process media (demux, mux, remux, extract, reindex, …). The stdout-capturing
// `capture` helper and `writeMKV` live in the existing test files.

// mustFatal asserts that fn triggers Fatal (the process-exit hook). It overrides
// the hook to panic, runs fn, and recovers - so the CLI error paths are exercised
// in-process. out is whatever fn printed to stderr before exiting.
func mustFatal(t *testing.T, fn func()) {
	t.Helper()
	called := false
	restore := commands.SetExit(func(int) { called = true; panic("exit") })
	defer restore()
	defer func() {
		_ = recover() // swallow the panic raised by the overridden exit hook
		if !called {
			t.Error("expected the command to call Fatal/os.Exit, but it did not")
		}
	}()
	fn()
	t.Error("the command returned without calling Fatal")
}

// richContainer is a metadata-rich Matroska container: video + audio + subtitle
// tracks, two chapters, an attachment and tags.
func richContainer() *matroska.Container {
	return &matroska.Container{
		Info: matroska.SegmentInfo{
			TimecodeScale: 1_000_000,
			Title:         "Fixture Title",
			MuxingApp:     "mkvgo-test",
			WritingApp:    "mkvgo-test",
			Duration:      5000,
		},
		Tracks: []matroska.Track{
			{ID: 1, Type: matroska.VideoTrack, Codec: "h264", Language: "und", IsDefault: true,
				Width: ptrU32(1920), Height: ptrU32(1080)},
			{ID: 2, Type: matroska.AudioTrack, Codec: "aac", Language: "eng", IsDefault: true,
				Channels: ptrU8(2), SampleRate: ptrF64(48000)},
			{ID: 3, Type: matroska.SubtitleTrack, Codec: "srt", Language: "fre", Name: "Français"},
		},
		Chapters: []matroska.Chapter{
			{ID: 1, Title: "Intro", StartMs: 0, EndMs: 2000},
			{ID: 2, Title: "Main", StartMs: 2000, EndMs: 5000},
		},
		Attachments: []matroska.Attachment{
			{ID: 1, Name: "cover.jpg", MIMEType: "image/jpeg", Size: 3, Data: []byte{1, 2, 3}},
		},
		Tags: []matroska.Tag{
			{TargetType: "MOVIE", SimpleTags: []matroska.SimpleTag{{Name: "TITLE", Value: "Fixture Title"}}},
		},
		DurationMs: 5000,
	}
}

// richMKV writes richContainer to a temp .mkv and returns the path.
func richMKV(t *testing.T) string {
	t.Helper()
	return writeMKV(t, richContainer())
}

// sampleMKV copies the real testdata sample (h264 + aac, ~1s of frames) into a
// fresh temp file and returns its path, so commands that write output never touch
// the committed fixture.
func sampleMKV(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read sample.mkv: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "sample.mkv")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write sample copy: %v", err)
	}
	return dst
}
