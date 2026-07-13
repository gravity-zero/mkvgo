package ops

// cuehealth_noseekhead_test.go pins the head-only verdict on the layout most
// real muxers produce: NO SeekHead, Cues written at the very end. The Cues are
// intact and video-keyed, so every head-only consumer (CueHealth, Diagnose,
// Ingest) must call the index present. Reading them only through a
// SeekHead-referenced offset made all three report a healthy index as missing -
// a false "no-index" on the majority of a real library.

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// voidFileSeekHead overwrites path's SeekHead with a Void element spanning
// exactly the same bytes: nothing else about the file moves, and its Cues stay
// where they are - at the tail, now indexed by nothing.
func voidFileSeekHead(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(data, []byte{0x11, 0x4D, 0x9B, 0x74}) // IDSeekHead
	if at < 0 {
		t.Fatal("fixture has no SeekHead to void")
	}
	eh, hlen, err := ebml.ReadElementHeader(bytes.NewReader(data[at:]))
	if err != nil || eh.ID != mkv.IDSeekHead || eh.Size < 0 {
		t.Fatalf("SeekHead header at %d: id=%#x size=%d err=%v", at, eh.ID, eh.Size, err)
	}
	span := int64(hlen) + eh.Size
	if span < 9 {
		t.Fatalf("SeekHead spans %d bytes, too small to void", span)
	}
	data[at] = 0xEC // Void ID (1 byte) + an 8-byte size VINT absorbs the rest exactly
	data[at+1] = 0x01
	body := uint64(span - 9)
	for i := 0; i < 7; i++ {
		data[at+2+i] = byte(body >> (8 * (6 - i)))
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCueHealthNoSeekHeadTailCues(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	cues := []mkv.CuePoint{
		{TimeMs: 0, Track: 1, ClusterPos: 100},
		{TimeMs: 1000, Track: 1, ClusterPos: 200},
		{TimeMs: 2000, Track: 1, ClusterPos: 300},
	}
	path := buildMKVWithCues(t, dir, "noseekhead.mkv", tracks, cueHealthFixtureSets(3), cues)
	voidFileSeekHead(t, path)

	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatalf("CueHealth: %v", err)
	}
	if r.TotalCues != len(cues) {
		t.Errorf("TotalCues = %d, want %d (the index is intact at the tail, only the SeekHead is gone)", r.TotalCues, len(cues))
	}
	if !r.Healthy {
		t.Errorf("Healthy = false (%s), want true", r.Reason)
	}

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	for _, f := range d.Findings {
		if f.Kind == "no-index" {
			t.Errorf("Diagnose: %q finding on a file whose Cues are intact: %s", f.Kind, f.Detail)
		}
	}
}

// TestIngestNoSeekHeadSeesSeekIndex is the serving-side face of the same bug:
// a SeekHead-less source with a real index must not be scheduled for a reindex.
func TestIngestNoSeekHeadSeesSeekIndex(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	cues := []mkv.CuePoint{{TimeMs: 0, Track: 1, ClusterPos: 100}, {TimeMs: 1000, Track: 1, ClusterPos: 200}}
	path := buildMKVWithCues(t, dir, "ingest-noseekhead.mkv", tracks, cueHealthFixtureSets(2), cues)
	voidFileSeekHead(t, path)

	// safari verdicts remux for an mkv source, which is the branch that checks
	// the seek index (a direct-play verdict never looks).
	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "safari"})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if plan.Strategy != StrategyRemuxHLS {
		t.Fatalf("strategy = %q, want remux-hls (reasons=%v)", plan.Strategy, plan.Reasons)
	}
	if !plan.HasSeekIndex {
		t.Error("HasSeekIndex = false, want true (tail Cues, no SeekHead)")
	}
	if plan.NeedsReindex {
		t.Error("NeedsReindex = true: a file with a healthy tail index must not be scheduled for a reindex")
	}
}

// TestIngestAudioKeyedIndexNeedsReindex closes the gap between what Ingest
// PROMISES and what serving actually needs. "Has a seek index" used to mean
// len(Cues) > 0 - any cue, any track - while PlanHLS cuts its segments on the
// VIDEO track's cues and refuses a source that indexes no video keyframe. An
// audio-keyed index was therefore waved through as "ready for on-demand HLS",
// and blew up at serving time on the plan Ingest had just blessed.
func TestIngestAudioKeyedIndexNeedsReindex(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	audioCues := []mkv.CuePoint{{TimeMs: 0, Track: 2, ClusterPos: 100}, {TimeMs: 1000, Track: 2, ClusterPos: 200}}
	path := buildMKVWithCues(t, dir, "audio-keyed.mkv", tracks, cueHealthFixtureSets(2), audioCues)

	plan, err := Ingest(context.Background(), path, IngestOptions{Target: "safari"})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if plan.Strategy != StrategyRemuxHLS {
		t.Fatalf("strategy = %q, want remux-hls (reasons=%v)", plan.Strategy, plan.Reasons)
	}
	if plan.HasSeekIndex {
		t.Error("HasSeekIndex = true on an index that cues only the audio: PlanHLS would refuse this source")
	}
	if !plan.NeedsReindex {
		t.Error("NeedsReindex = false: the source needs a video-keyed index before it can be packaged")
	}
}
