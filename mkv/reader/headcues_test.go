package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// chaptersElem/attachmentsElem: minimal head elements a real muxer writes between
// Tracks and the first Cluster, and that no SeekHead is required to index.
func chaptersElem() []byte {
	return masterElem(mkv.IDChapters,
		masterElem(mkv.IDEditionEntry,
			masterElem(mkv.IDChapterAtom,
				uintElem(mkv.IDChapterUID, 42, 1),
				uintElem(mkv.IDChapterTimeStart, 0, 1),
			),
		),
	)
}

func attachmentsElem() []byte {
	var data bytes.Buffer
	ebml.WriteElementHeader(&data, mkv.IDFileData, 4)
	data.Write([]byte{0xFF, 0xD8, 0xFF, 0xD9})
	return masterElem(mkv.IDAttachments,
		masterElem(mkv.IDAttachedFile,
			strElem(mkv.IDFileName, "cover.jpg"),
			strElem(mkv.IDFileMimeType, "image/jpeg"),
			data.Bytes(),
		),
	)
}

// TestHeadMetaNoSeekHeadTailCuesScan is the regression for the head-only false
// "no-index". A file whose Cues sit intact at the tail but which carries NO
// SeekHead (the majority of real muxers) was read as having ZERO cues by the
// metadata path - the path CueHealth/Diagnose/Ingest/PlanHLS use through
// WithCues() - because finalizeHeadMeta only ever followed a SeekHead-referenced
// offset. The full reader has always recovered those Cues with one bounded read
// back from EOF; the metadata path must reach the same verdict, or it reports a
// perfectly healthy index as missing.
func TestHeadMetaNoSeekHeadTailCuesScan(t *testing.T) {
	const clusterSize = 128 << 10

	small := &callCountingReadSeeker{rs: bytes.NewReader(buildTailMKV(t, false, 20, clusterSize, false))}
	cSmall, err := ReadMeta(context.Background(), small, "small.mkv", WithCues())
	if err != nil {
		t.Fatalf("ReadMeta(20 clusters, no seekhead): %v", err)
	}
	large := &callCountingReadSeeker{rs: bytes.NewReader(buildTailMKV(t, false, 200, clusterSize, false))}
	cLarge, err := ReadMeta(context.Background(), large, "large.mkv", WithCues())
	if err != nil {
		t.Fatalf("ReadMeta(200 clusters, no seekhead): %v", err)
	}

	for _, c := range []struct {
		name string
		got  int
	}{{"20 clusters", len(cSmall.Cues)}, {"200 clusters", len(cLarge.Cues)}} {
		if c.got != tailCueCount {
			t.Errorf("%s: Cues = %d, want %d", c.name, c.got, tailCueCount)
		}
	}
	if len(cSmall.Keyframes) != tailCueCount {
		t.Errorf("Keyframes = %d, want %d (derived from the recovered Cues)", len(cSmall.Keyframes), tailCueCount)
	}
	// The recovery is ONE bounded read at EOF, not a cluster walk: 10x the
	// clusters must not cost more reads.
	if small.calls != large.calls {
		t.Errorf("read calls grew with cluster count: 20=%d, 200=%d (the meta path must not walk the clusters)", small.calls, large.calls)
	}
}

// TestHeadMetaStaleSeekHeadCuesFallback covers the other false-negative shape: a
// SeekHead IS present but its Cues pointer is stale (lands mid-cluster - a Cues
// index the muxer never wrote, or a position left behind by an edit). The offset
// is rejected, and the real tail Cues must still be found.
func TestHeadMetaStaleSeekHeadCuesFallback(t *testing.T) {
	data := buildTailMKV(t, true, 30, 64<<10, true)
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "stale.mkv", WithCues())
	if err != nil {
		t.Fatalf("ReadMeta with stale SeekHead: %v", err)
	}
	if len(c.Cues) != tailCueCount {
		t.Errorf("Cues = %d, want %d", len(c.Cues), tailCueCount)
	}
}

// TestHeadMetaNoCuesStaysEmpty guards the negative: a file with no SeekHead AND
// no Cues at all must still report none - the tail scan misses and invents
// nothing - without erroring.
func TestHeadMetaNoCuesStaysEmpty(t *testing.T) {
	data := buildNoCuesMKV(t, 8, 64<<10)
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "nocues.mkv", WithCues())
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Cues) != 0 {
		t.Errorf("Cues = %d, want 0 (the file has no index)", len(c.Cues))
	}
	if len(c.Tracks) != 2 {
		t.Errorf("Tracks = %d, want 2 (head metadata must survive the miss)", len(c.Tracks))
	}
}

// TestHeadMetaNoSeekHeadTailTags is the same hole, one element over: the Tags
// live in that same trailing region, and WithBitrate/WithTags followed only a
// SeekHead pointer too - so a file with no SeekHead reported no per-track BPS
// bitrate while carrying it. The tail scan hands over whatever the caller asked
// for, not just the Cues.
func TestHeadMetaNoSeekHeadTailTags(t *testing.T) {
	data := buildTailMKV(t, false, 20, 64<<10, false)

	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithTags())
	if err != nil {
		t.Fatalf("ReadMeta(WithTags): %v", err)
	}
	if len(c.Tags) != 1 {
		t.Errorf("Tags = %d, want 1 (the tail Tags element, indexed by nothing)", len(c.Tags))
	}
	// Asking for the Tags must not smuggle the Cues in: the metadata contract
	// still hides what was not requested.
	if c.Cues != nil {
		t.Errorf("Cues = %d, want nil (not requested)", len(c.Cues))
	}
}

// TestHeadMetaHeadElementsNoSeekHead covers the third face of the same hole, and
// the one the tail scan CANNOT close: Chapters, Attachments and Tags written in
// the head - between Tracks and the first Cluster - with no SeekHead indexing
// them. The metadata path skipped them outright (no case for Chapters or
// Attachments; Tags kept only for WithBitrate) and then stopped at Info+Tracks,
// so the elements were neither read on the way past nor reachable afterwards:
// the SeekHead does not point at them and they are nowhere near EOF. The full
// reader parses them wherever they sit.
func TestHeadMetaHeadElementsNoSeekHead(t *testing.T) {
	data := segmentMKV(infoElem(), tracksElem(),
		chaptersElem(), attachmentsElem(), tagsElem("ENCODER", "mkvgo-test"),
		clusterElem())

	full, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(full.Chapters) == 0 || len(full.Attachments) == 0 || len(full.Tags) == 0 {
		t.Fatalf("fixture is wrong: full Read got chapters=%d attachments=%d tags=%d",
			len(full.Chapters), len(full.Attachments), len(full.Tags))
	}

	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv",
		WithChapters(), WithAttachments(), WithTags())
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Chapters) != len(full.Chapters) {
		t.Errorf("Chapters = %d, want %d", len(c.Chapters), len(full.Chapters))
	}
	if len(c.Attachments) != len(full.Attachments) {
		t.Errorf("Attachments = %d, want %d", len(c.Attachments), len(full.Attachments))
	}
	if len(c.Tags) != len(full.Tags) {
		t.Errorf("Tags = %d, want %d (WithTags alone, no WithBitrate)", len(c.Tags), len(full.Tags))
	}
	if len(c.Tracks) != len(full.Tracks) {
		t.Errorf("Tracks = %d, want %d (the head scan must not duplicate or lose tracks)", len(c.Tracks), len(full.Tracks))
	}
}

// TestHeadMetaWithoutWithCuesSkipsTailScan pins the cost contract: a caller that
// does not ask for the Cues must not pay the tail read at all, and still gets the
// metadata contract (Cues nil). Only WithCues() opts into the recovery.
func TestHeadMetaWithoutWithCuesSkipsTailScan(t *testing.T) {
	const clusterSize = 128 << 10
	data := buildTailMKV(t, false, 20, clusterSize, false)

	plain := &callCountingReadSeeker{rs: bytes.NewReader(data)}
	c, err := ReadMeta(context.Background(), plain, "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if c.Cues != nil {
		t.Errorf("Cues = %d, want nil (not requested)", len(c.Cues))
	}

	withCues := &callCountingReadSeeker{rs: bytes.NewReader(data)}
	if _, err := ReadMeta(context.Background(), withCues, "x.mkv", WithCues()); err != nil {
		t.Fatalf("ReadMeta(WithCues): %v", err)
	}
	if plain.calls >= withCues.calls {
		t.Errorf("plain ReadMeta made %d read calls vs %d with WithCues: the tail scan must only run when the Cues are requested", plain.calls, withCues.calls)
	}
}
