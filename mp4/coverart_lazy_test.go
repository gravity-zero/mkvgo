package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// The remux and the packagers carry only the cover art out of a source's
// attachments: a font set stays on disk, never read. Measured through a
// counting FS on a source whose font dwarfs its media.
func TestRemuxAndPackagingSkipFontPayloads(t *testing.T) {
	ctx := context.Background()
	font := make([]byte, 4<<20)
	for i := range font {
		font[i] = byte(i * 7)
	}
	cover := []byte("\xff\xd8\xff\xe0 tiny jpeg " + string(bytes.Repeat([]byte{0x42}, 2000)))
	src := buildMKVWithCover(t, []mkv.Attachment{
		{ID: 1, Name: "DejaVu.ttf", MIMEType: "font/ttf", Data: font},
		{ID: 2, Name: "cover.jpg", MIMEType: "image/jpeg", Data: cover},
	})
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() < int64(len(font)) {
		t.Fatalf("fixture must embed the font: %d bytes", st.Size())
	}
	budget := st.Size() - int64(len(font)) + (256 << 10) // everything but the font, plus read-ahead

	tally := &readTally{}
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(ctx, src, dst, Options{FS: countingFS(tally)}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, cover) {
		t.Error("the cover art must reach the MP4")
	}
	if tally.bytes > budget {
		t.Errorf("RemuxToMP4 read %d KB, the font (%d KB) was loaded", tally.bytes>>10, len(font)>>10)
	}

	*tally = readTally{}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plan.InitSegment(), cover) {
		t.Error("the cover art must ride on the plan's init segment")
	}
	if tally.bytes > budget {
		t.Errorf("PlanHLS read %d KB, the font (%d KB) was loaded", tally.bytes>>10, len(font)>>10)
	}

	*tally = readTally{}
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 1000, FS: countingFS(tally)}); err != nil {
		t.Fatal(err)
	}
	init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(init, cover) {
		t.Error("the cover art must ride on the packaged init segment")
	}
	if tally.bytes > budget {
		t.Errorf("RemuxToHLS read %d KB, the font (%d KB) was loaded", tally.bytes>>10, len(font)>>10)
	}
}

// pickCoverAttachment works on the attachment LIST alone: an unloaded payload
// (Size + DataPath) qualifies, an empty or absurdly large one does not.
func TestPickCoverAttachmentLazy(t *testing.T) {
	lazy := mkv.Attachment{Name: "cover.png", MIMEType: "image/png", Size: 1234, DataPath: "/x.mkv", DataOffset: 99}
	huge := mkv.Attachment{Name: "cover.png", MIMEType: "image/png", Size: maxCoverArtBytes + 1, DataPath: "/x.mkv"}
	empty := mkv.Attachment{Name: "cover.png", MIMEType: "image/png"}
	if got := pickCoverAttachment([]mkv.Attachment{huge, lazy}); got == nil || got.DataOffset != 99 {
		t.Errorf("the lazy cover must be picked over the oversized one: %+v", got)
	}
	if got := pickCoverAttachment([]mkv.Attachment{empty, huge}); got != nil {
		t.Errorf("empty and oversized images are not covers: %+v", got)
	}
}
