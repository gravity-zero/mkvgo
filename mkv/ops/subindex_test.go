package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildIndexFixture writes a source with a video track and two subtitle tracks
// (one text, one bitmap), so a test can tell "indexed" from "extractable".
func buildIndexFixture(t *testing.T, dir string) string {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "und"},
		subtitleTrack(2, "srt"),
		subtitleTrack(3, "pgs"),
	}
	payload := make([]byte, 512<<10)
	var blocks []mkv.Block
	for i := 0; i < 8; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i * 100), Keyframe: true, Data: payload})
		blocks = append(blocks, mkv.Block{
			TrackNumber: 2, Timecode: int64(i * 100), Duration: 50,
			Data: []byte("cue " + strconv.Itoa(i)),
		})
		blocks = append(blocks, mkv.Block{TrackNumber: 3, Timecode: int64(i * 100), Data: []byte{0x14, byte(i)}})
	}
	return buildMinimalMKV(t, dir, "indexed.mkv", tracks, blocks, 1000)
}

// The whole point: an index built once serves the same WebVTT a full walk
// produces, byte for byte, while reading a fraction of the file.
func TestSubtitleIndex_ServesSameBytesAsFullWalk(t *testing.T) {
	dir := t.TempDir()
	path := buildIndexFixture(t, dir)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("BuildSubtitleIndex: %v", err)
	}
	if got := ix.Tracks(); len(got) != 2 {
		t.Fatalf("indexed tracks = %v, want the two subtitle tracks", got)
	}
	if n := ix.Blocks(2); n != 8 {
		t.Errorf("track 2 has %d indexed blocks, want 8", n)
	}
	if ix.SourceSize() != st.Size() {
		t.Errorf("SourceSize = %d, want %d", ix.SourceSize(), st.Size())
	}

	var full bytes.Buffer
	if err := ExtractSubtitleWebVTT(context.Background(), path, 2, &full); err != nil {
		t.Fatal(err)
	}

	cfs := &countingOpenFS{}
	var served bytes.Buffer
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 2, ix, &served, mkv.Options{FS: cfs.fs()}); err != nil {
		t.Fatalf("ExtractSubtitleWebVTTFrom: %v", err)
	}
	if !bytes.Equal(full.Bytes(), served.Bytes()) {
		t.Errorf("served output differs from the full walk:\n--- full ---\n%s\n--- served ---\n%s", full.String(), served.String())
	}
	if !strings.Contains(served.String(), "cue 7") {
		t.Errorf("last cue missing:\n%s", served.String())
	}
	// Serving must seek, not walk: the block reads are a small fraction of the
	// file. The last handle is the block walk (the first is the metadata pass).
	walk := cfs.reads[len(cfs.reads)-1]
	if limit := st.Size() / 10; walk.n > limit {
		t.Errorf("serving from the index read %d of %d bytes - it must seek to the blocks, not walk",
			walk.n, st.Size())
	}
}

// The wire format must survive a round trip exactly, and re-encode identically.
func TestSubtitleIndex_MarshalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := buildIndexFixture(t, dir)

	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := ix.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	var back SubtitleIndex
	if err := back.UnmarshalBinary(blob); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	again, err := back.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, again) {
		t.Error("re-encoding a decoded index does not reproduce the same bytes")
	}

	var served, direct bytes.Buffer
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 2, &back, &served); err != nil {
		t.Fatalf("serving from the decoded index: %v", err)
	}
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 2, ix, &direct); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(served.Bytes(), direct.Bytes()) {
		t.Error("the decoded index serves different bytes than the one it was encoded from")
	}
}

// A buffer that is not an index, is truncated, or carries an unknown version
// must be rejected outright - never half-decoded into a usable-looking index.
func TestSubtitleIndex_UnmarshalRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	ix, err := BuildSubtitleIndex(context.Background(), buildIndexFixture(t, dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	good, err := ix.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	bad := map[string][]byte{
		"empty":            {},
		"wrong magic":      []byte("NOTANIDXnope"),
		"header only":      good[:len(subIndexMagic)],
		"unknown version":  append(append([]byte{}, []byte(subIndexMagic)...), 99),
		"truncated body":   good[:len(good)-1],
		"truncated middle": good[:len(good)/2],
	}
	for name, data := range bad {
		var got SubtitleIndex
		if err := got.UnmarshalBinary(data); err == nil {
			t.Errorf("%s: UnmarshalBinary accepted it (%d tracks)", name, len(got.Tracks()))
		}
	}
	// A huge declared entry count must not be believed (nor allocated for).
	huge := append([]byte{}, good...)
	for i := range huge {
		if i > len(subIndexMagic)+8 && huge[i] == 0x08 {
			huge[i] = 0xFF
			break
		}
	}
	var got SubtitleIndex
	_ = got.UnmarshalBinary(huge) // must not panic or hang; an error is fine
}

// An index used against a file it was not built from must be refused, not
// followed to whatever those offsets now hold.
func TestSubtitleIndex_StaleIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := buildIndexFixture(t, dir)
	ix, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("different size", func(t *testing.T) {
		grown := path + ".grown"
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(grown, append(data, 0, 0, 0, 0), 0o644); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		err = ExtractSubtitleWebVTTFrom(context.Background(), grown, 2, ix, &b)
		if !errors.Is(err, ErrIndexStale) {
			t.Errorf("err = %v, want ErrIndexStale", err)
		}
	})

	t.Run("same size, shifted content", func(t *testing.T) {
		// Same length, different bytes at the recorded offsets: only the
		// per-block check can catch this one.
		shifted := path + ".shifted"
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		entries := ix.entries[2]
		if len(entries) == 0 {
			t.Fatal("no indexed entries to corrupt")
		}
		off := entries[len(entries)-1].pos.Off
		for i := off; i < off+8 && int(i) < len(data); i++ {
			data[i] = 0x00
		}
		if err := os.WriteFile(shifted, data, 0o644); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		err = ExtractSubtitleWebVTTFrom(context.Background(), shifted, 2, ix, &b)
		if !errors.Is(err, ErrIndexStale) {
			t.Errorf("err = %v, want ErrIndexStale", err)
		}
	})
}

// A track the index does not cover, and one that is not text, must both be
// refused with their own error rather than served empty.
func TestSubtitleIndex_TrackSelection(t *testing.T) {
	dir := t.TempDir()
	path := buildIndexFixture(t, dir)

	only2, err := BuildSubtitleIndex(context.Background(), path, []uint64{2})
	if err != nil {
		t.Fatal(err)
	}
	if got := only2.Tracks(); len(got) != 1 || got[0] != 2 {
		t.Errorf("Tracks() = %v, want [2]", got)
	}
	if only2.Blocks(3) != 0 {
		t.Error("track 3 was indexed although it was not asked for")
	}

	var b bytes.Buffer
	// Track 3 is a subtitle track, but bitmap: rejected on the codec, the same
	// way the walking extractor rejects it.
	if err := ExtractSubtitleWebVTTFrom(context.Background(), path, 3, only2, &b); err == nil {
		t.Error("a bitmap subtitle track was accepted")
	}

	all, err := BuildSubtitleIndex(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Indexed, but still not convertible: the codec check must come first and
	// say so, rather than reporting it as missing from the index.
	err = ExtractSubtitleWebVTTFrom(context.Background(), path, 3, all, &b)
	if err == nil || errors.Is(err, ErrTrackNotIndexed) {
		t.Errorf("err = %v, want the not-text refusal", err)
	}
	// A track that exists but was left out of a partial index.
	err = ExtractSubtitleWebVTTFrom(context.Background(), path, 2, &SubtitleIndex{
		fileSize: only2.fileSize, tcScale: only2.tcScale, entries: map[uint64][]subEntry{},
	}, &b)
	if !errors.Is(err, ErrTrackNotIndexed) {
		t.Errorf("err = %v, want ErrTrackNotIndexed", err)
	}
	if _, err := BuildSubtitleIndex(context.Background(), path, []uint64{99}); err == nil {
		t.Error("indexing a track that does not exist was accepted")
	}
}

// Building an index must not read a single payload byte - of any track, its own
// included: it records positions.
func TestSubtitleIndex_BuildReadsNoPayload(t *testing.T) {
	dir := t.TempDir()
	path := buildIndexFixture(t, dir)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cfs := &countingOpenFS{}
	if _, err := BuildSubtitleIndex(context.Background(), path, nil, mkv.Options{FS: cfs.fs()}); err != nil {
		t.Fatal(err)
	}
	walk := cfs.reads[len(cfs.reads)-1]
	if limit := st.Size() / 3; walk.n > limit {
		t.Errorf("the index build read %d of %d bytes - it must walk headers, not payloads",
			walk.n, st.Size())
	}
}

// A small buffer declaring a huge entry count must be refused on the spot, not
// believed far enough to size a slice from it.
func TestSubtitleIndex_UnmarshalBoundsDeclaredCount(t *testing.T) {
	var b []byte
	b = append(b, subIndexMagic...)
	b = append(b, subIndexVersion)
	b = binary.AppendVarint(b, 1<<20) // fileSize
	b = binary.AppendVarint(b, 1000000)
	b = binary.AppendUvarint(b, 0)     // no segment UID
	b = binary.AppendUvarint(b, 1)     // one track
	b = binary.AppendUvarint(b, 2)     // track ID
	b = binary.AppendUvarint(b, 1<<40) // ... claiming a trillion entries
	var ix SubtitleIndex
	if err := ix.UnmarshalBinary(b); err == nil {
		t.Fatal("a declared count larger than the buffer was accepted")
	}
	if n := ix.Blocks(2); n != 0 {
		t.Errorf("the rejected index left %d entries behind", n)
	}
}
