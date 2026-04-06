package matroska

import (
	"context"
	"os"
	"testing"
)

func TestValidateFixture(t *testing.T) {
	requireFixture(t)
	issues, err := Validate(context.Background(), fixturePath)
	assertNoErr(t, err)
	for _, i := range issues {
		t.Logf("%s", i)
	}
	// fixture is well-formed, should have no errors
	for _, i := range issues {
		if i.Severity == SeverityError {
			t.Errorf("unexpected error: %s", i)
		}
	}
}

func TestValidateMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/meta_only.mkv"

	f, err := os.Create(path)
	assertNoErr(t, err)
	assertNoErr(t, Write(f, &Container{
		Info:   SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []Track{{ID: 1, Type: VideoTrack, Codec: "hevc", Language: "und"}},
	}))
	f.Close()

	issues, err := Validate(context.Background(), path)
	assertNoErr(t, err)

	hasWarning := func(substr string) bool {
		for _, i := range issues {
			if i.Severity == SeverityWarning && contains(i.Message, substr) {
				return true
			}
		}
		return false
	}
	if !hasWarning("CodecPrivate") {
		t.Error("expected warning about missing CodecPrivate")
	}
	if !hasWarning("dimensions") {
		t.Error("expected warning about missing video dimensions")
	}
}

func TestValidateNoTracks(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/empty.mkv"

	f, err := os.Create(path)
	assertNoErr(t, err)
	assertNoErr(t, Write(f, &Container{
		Info: SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
	}))
	f.Close()

	issues, err := Validate(context.Background(), path)
	assertNoErr(t, err)

	hasError := false
	for _, i := range issues {
		if i.Severity == SeverityError && contains(i.Message, "no tracks") {
			hasError = true
		}
	}
	if !hasError {
		t.Error("expected error about no tracks")
	}
}

func TestValidateDuplicateTrackIDs(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dup.mkv"

	f, err := os.Create(path)
	assertNoErr(t, err)
	assertNoErr(t, Write(f, &Container{
		Info: SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []Track{
			{ID: 1, Type: VideoTrack, Codec: "hevc", Language: "und"},
			{ID: 1, Type: AudioTrack, Codec: "aac", Language: "eng"},
		},
	}))
	f.Close()

	issues, err := Validate(context.Background(), path)
	assertNoErr(t, err)

	hasDup := false
	for _, i := range issues {
		if i.Severity == SeverityError && contains(i.Message, "duplicate") {
			hasDup = true
		}
	}
	if !hasDup {
		t.Error("expected error about duplicate track IDs")
	}
}
