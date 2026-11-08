package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

const ulawFixture = "../internal/testdata/aac_ulaw.mov" // track 1 mu-law (no Matroska form), track 2 AAC

// ttmlFixture writes a fragmented MP4 with an AAC track 1 and a TTML (stpp) subtitle track 2, which Matroska has
// no form for, and returns its path.
func ttmlFixture(t *testing.T) string {
	t.Helper()
	stpp := box("stpp", append(make([]byte, 8), "\x00\x00\x00"...)) // an XMLSubtitleSampleEntry: namespace, schema, mime
	ttml := soundTrak(2, 1000, 1000, stpp, emptyTables()...)
	ttml = bytes.Replace(ttml, []byte("soun"), []byte("subt"), 1)
	moof, mdat := oneFragment(1, 0, []uint32{10, 10}, []uint32{1024, 1024}, nil, 1)
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 48000, 0, audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC)), emptyTables()...),
		ttml, container("mvex", trexBox(1), trexBox(2)))...)
	data = append(append(data, moof...), mdat...)
	src := filepath.Join(t.TempDir(), "ttml.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return src
}

// TestStrictRefusesAnyDroppedTrack: Options.Strict fails a remux, and a walk of the blocks, on any track it would
// drop, a subtitle Matroska has no form for included, where the default drops it with its reason; SkipUnsupported
// does not override it. The track is named with the option that refused it.
func TestStrictRefusesAnyDroppedTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ttml := ttmlFixture(t)
	if err := RemuxFromMP4(ctx, ttml, filepath.Join(dir, "a.mkv")); err != nil {
		t.Fatalf("default: %v, want the subtitle dropped, not the remux refused", err)
	}
	err := RemuxFromMP4(ctx, ttml, filepath.Join(dir, "b.mkv"), Options{Strict: true})
	if err == nil || !strings.Contains(err.Error(), "track 2 (stpp)") || !strings.Contains(err.Error(), "Options.Strict") {
		t.Errorf("strict, TTML: err = %v, want track 2 (stpp) refused by Options.Strict", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.mkv")); !os.IsNotExist(err) {
		t.Error("the refused remux left an output")
	}
	err = RemuxFromMP4(ctx, ulawFixture, filepath.Join(dir, "c.mkv"), Options{Strict: true, SkipUnsupported: true})
	if err == nil || !strings.Contains(err.Error(), "track 1 (ulaw)") || !strings.Contains(err.Error(), "Options.Strict") {
		t.Errorf("strict with SkipUnsupported: err = %v, want track 1 (ulaw) refused by Options.Strict", err)
	}
	_, err = ForEachBlock(ctx, ttml, func(mkv.Block) error { return nil }, Options{Strict: true})
	if err == nil || !strings.Contains(err.Error(), "track 2 (stpp)") {
		t.Errorf("strict walk: err = %v, want track 2 (stpp) refused", err)
	}
}

// TestOpenMetaNamesWhatMatroskaCannotCarry: a probe says, per track, why the remux to Matroska would not carry it
// (Track.Unsupported, the reason the remux drops or refuses it by), "" for a track it carries.
func TestOpenMetaNamesWhatMatroskaCannotCarry(t *testing.T) {
	ctx := context.Background()
	c, _, err := OpenMeta(ctx, ulawFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 2 || !strings.Contains(c.Tracks[0].Unsupported, `"ulaw" has no Matroska form`) || c.Tracks[1].Unsupported != "" {
		t.Errorf("tracks = %+v, want the mu-law track's reason and none on the AAC", c.Tracks)
	}
	c, _, err = OpenMeta(ctx, ttmlFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 2 || c.Tracks[1].Type != mkv.SubtitleTrack || !strings.Contains(c.Tracks[1].Unsupported, "stpp") {
		t.Errorf("tracks = %+v, want the stpp subtitle's reason", c.Tracks)
	}
	esds := esdsBox(0x40, fakeASC)
	moof, mdat := oneFragment(1, 0, []uint32{10, 10}, []uint32{1024, 1024}, nil, 1)
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 44100, 0, audioSampleEntry("enca", &stereo48k, append(esds, sinfBox("mp4a", "cbcs")...)), emptyTables()...),
		container("mvex", trexBox(1)),
	)...)
	data = append(append(data, moof...), mdat...)
	src := filepath.Join(t.TempDir(), "enc.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err = OpenMeta(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 1 || !strings.Contains(c.Tracks[0].Unsupported, "encrypted (cbcs)") {
		t.Errorf("tracks = %+v, want the encryption named as the reason", c.Tracks)
	}
}
