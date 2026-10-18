package mp4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// trackDigests hashes, per probe track, its sample entry, timing tables, edit list and every sample's bytes in decode order.
func trackDigests(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	payload, err := readMoov(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}
	mv, err := parseMoov(payload, fi.Size(), sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	boxes, _ := iterBoxes(payload)
	var traks [][]byte
	for _, b := range boxes {
		if b.typ == "trak" {
			traks = append(traks, b.payload)
		}
	}
	var out []string
	for _, tr := range mv.tracks {
		h := sha256.New()
		trak := traks[tr.trakIndex]
		stbl, err := boxAt(trak, "mdia", "minf", "stbl")
		if err != nil {
			t.Fatal(err)
		}
		sb, _ := iterBoxes(stbl)
		for _, typ := range []string{"stsd", "stts", "ctts", "stss", "stsz", "sdtp", "sgpd", "sbgp"} {
			if b, ok := findMemBox(sb, typ); ok {
				h.Write([]byte(typ))
				h.Write(b.payload)
			}
		}
		if edts, err := boxAt(trak, "edts"); err == nil {
			h.Write(edts)
		}
		for _, s := range tr.samples {
			buf := make([]byte, s.size)
			if _, err := f.ReadAt(buf, s.offset); err != nil {
				t.Fatal(err)
			}
			h.Write(buf)
		}
		out = append(out, hex.EncodeToString(h.Sum(nil)))
	}
	return out
}

func topLevelTypes(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	boxes, err := iterBoxes(data)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range boxes {
		out = append(out, b.typ)
	}
	return out
}

func TestRewriteKeepsEverySampleAndGoesFastStart(t *testing.T) {
	for _, name := range []string{"synth", "quicktime.mov", "interleaved.mp4", "bframes_aac.mp4", "bframes_aac.mov"} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join("..", "internal", "testdata", name)
			if name == "synth" {
				src = editFixture(t)
			}
			dst := filepath.Join(t.TempDir(), "out.mp4")
			if err := Rewrite(context.Background(), src, dst); err != nil {
				t.Fatal(err)
			}
			want, got := trackDigests(t, src), trackDigests(t, dst)
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("track digests differ:\n src %v\n dst %v", want, got)
			}
			types := strings.Join(topLevelTypes(t, dst), ",")
			if !strings.HasSuffix(types, "moov,mdat") {
				t.Errorf("top-level layout = %s, want ...moov,mdat", types)
			}
			if a, b := topLevelTypes(t, src)[0], topLevelTypes(t, dst)[0]; a == "ftyp" && b != "ftyp" {
				t.Errorf("ftyp lost")
			}
			sm, dm := mustMeta(t, src), mustMeta(t, dst)
			sm.Path, dm.Path = "", ""
			if !bytes.Equal(jsonOf(t, sm), jsonOf(t, dm)) {
				t.Errorf("metadata differs:\n src %s\n dst %s", jsonOf(t, sm), jsonOf(t, dm))
			}
		})
	}
}

func TestRewriteCompactsAnEditedFile(t *testing.T) {
	src := editFixture(t)
	for i := 0; i < 3; i++ {
		if err := EditInPlace(context.Background(), src, func(c *mkv.Container) { c.Info.Title = strings.Repeat("x", i+1) }); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := Rewrite(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if types := strings.Join(topLevelTypes(t, dst), ","); strings.Contains(types, "free") {
		t.Errorf("retired moovs carried over: %s", types)
	}
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if di.Size() >= si.Size() {
		t.Errorf("output %d bytes, source %d", di.Size(), si.Size())
	}
	mustVerify(t, dst)
	if mustMeta(t, dst).Info.Title != "xxx" {
		t.Error("title lost")
	}
}

func TestRewriteCo64(t *testing.T) {
	old := co64Above
	co64Above = 0
	defer func() { co64Above = old }()
	src := editFixture(t)
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := Rewrite(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	stbl, err := boxAt(firstTrak(t, dst), "mdia", "minf", "stbl")
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := iterBoxes(stbl)
	if !hasMemBox(sb, "co64") || hasMemBox(sb, "stco") {
		t.Error("no co64 in the output")
	}
	if strings.Join(trackDigests(t, src), ",") != strings.Join(trackDigests(t, dst), ",") {
		t.Error("samples differ")
	}
	mustVerify(t, dst)
}

func firstTrak(t *testing.T, path string) []byte {
	t.Helper()
	for _, b := range moovBoxesOf(t, path) {
		if b.typ == "trak" {
			return b.payload
		}
	}
	t.Fatal("no trak")
	return nil
}

func TestRemoveTrack(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 3000}}
	}); err != nil {
		t.Fatal(err)
	}
	want := trackDigests(t, src)
	for _, tc := range []struct {
		remove uint64
		keep   int
		typ    mkv.TrackType
	}{{2, 0, mkv.VideoTrack}, {1, 1, mkv.AudioTrack}} {
		dst := filepath.Join(t.TempDir(), "out.mp4")
		if err := RemoveTrack(context.Background(), src, dst, []uint64{tc.remove}); err != nil {
			t.Fatal(err)
		}
		c := mustMeta(t, dst)
		if len(c.Tracks) != 1 || c.Tracks[0].Type != tc.typ {
			t.Fatalf("removing %d left %+v", tc.remove, c.Tracks)
		}
		if got := trackDigests(t, dst); got[0] != want[tc.keep] {
			t.Errorf("removing %d changed the kept track's samples", tc.remove)
		}
		if len(c.Chapters) != 2 || c.Chapters[1].Title != "B" {
			t.Errorf("chapters after removing %d: %+v", tc.remove, c.Chapters)
		}
		if refs := chapterTrackRefs(moovBoxesOf(t, dst)); len(refs) != 1 {
			t.Errorf("chapter track references after removing %d: %v", tc.remove, refs)
		}
		if di, si := fileSize(t, dst), fileSize(t, src); di >= si {
			t.Errorf("output %d bytes is not smaller than the source %d", di, si)
		}
	}
	for _, ids := range [][]uint64{{3}, {0}, {1, 2}} {
		err := RemoveTrack(context.Background(), src, filepath.Join(t.TempDir(), "x.mp4"), ids)
		if err == nil {
			t.Errorf("removing %v: no error", ids)
		}
	}
}

func fileSize(t *testing.T, p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestEditMetadataAndSetChaptersToNewFile(t *testing.T) {
	src := editFixture(t)
	orig, _ := os.ReadFile(src)
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := EditMetadata(context.Background(), src, dst, func(c *mkv.Container) {
		c.Info.Title = "Edited"
		c.Tracks[1].Language = "eng"
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "Only", StartMs: 0}}
	}); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(src); !bytes.Equal(now, orig) {
		t.Fatal("the source changed")
	}
	c := mustMeta(t, dst)
	if c.Info.Title != "Edited" || c.Tracks[1].Language != "eng" || len(c.Chapters) != 1 {
		t.Errorf("edited metadata = %q %q %+v", c.Info.Title, c.Tracks[1].Language, c.Chapters)
	}
	if strings.Join(trackDigests(t, src), ",") != strings.Join(trackDigests(t, dst), ",") {
		t.Error("samples differ")
	}
	mustVerify(t, dst)

	dst2 := filepath.Join(t.TempDir(), "out2.mp4")
	if err := SetChapters(context.Background(), dst, dst2, nil); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, dst2); len(c.Chapters) != 0 {
		t.Errorf("chapters left: %+v", c.Chapters)
	}
	if refs := chapterTrackRefs(moovBoxesOf(t, dst2)); len(refs) != 0 {
		t.Errorf("chapter track left: %v", refs)
	}
	err := EditMetadata(context.Background(), src, filepath.Join(t.TempDir(), "x.mp4"), func(c *mkv.Container) { c.Tracks[0].Codec = "av1" })
	if err == nil || !strings.Contains(err.Error(), "Codec cannot be changed") {
		t.Errorf("err = %v", err)
	}
}

func TestRewriteRefusesChunksPastTheEnd(t *testing.T) {
	full := filepath.Join(t.TempDir(), "full.mp4")
	if err := Rewrite(context.Background(), editFixture(t), full); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(full)
	trunc := filepath.Join(t.TempDir(), "trunc.mp4")
	if err := os.WriteFile(trunc, data[:len(data)-10], 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := Rewrite(context.Background(), trunc, dst); err == nil || !strings.Contains(err.Error(), "outside the file") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a refused rewrite left an output")
	}
}

func TestRewriteCancelLeavesNoOutput(t *testing.T) {
	src := editFixture(t)
	old := copyRunMax
	copyRunMax = 1
	defer func() { copyRunMax = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dst := filepath.Join(t.TempDir(), "out.mp4")
	err := Rewrite(ctx, src, dst, Options{Progress: func(done, total int64) { cancel() }})
	if err == nil {
		t.Fatal("a rewrite cancelled after its first read went through")
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Errorf("a cancelled rewrite left its output (err %v)", err)
	}
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
