package mp4

// hlsplan_mut_test.go targets the mutation-testing survivors found in
// hlsplan.go: branches and boundary constants that no existing test
// distinguished. Each test asserts the SPECIFIC value a mutated operator or
// constant would change (an exact count, offset, or boundary), not just "it
// runs" - see hlsplan_test.go, hls_laced_test.go and hlsplan_subcursor_test.go
// for the broader byte-parity and behavioural coverage this file complements.

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// --- PlanHLS: SegmentMs <= 0 selects the default (hlsplan.go:103) -----------

// A non-positive SegmentMs must fall back to defaultSegmentMs (6000): a
// CONDITIONALS_BOUNDARY mutant turning "<= 0" into "< 0" would leave SegmentMs
// at exactly 0, and the boundary loop ("cue.TimeMs >= last+segMs") then cuts a
// new segment at nearly every video keyframe instead of every ~6 s - a large,
// easily distinguished difference in segment count.
func TestPlanHLSSegmentMsZeroDefaultsTo6000(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 10; i++ { // one keyframe per second, 10 s total
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 1000, key: true,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
	}, gblocks)

	zero, err := PlanHLS(context.Background(), src, Options{SegmentMs: 0})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := PlanHLS(context.Background(), src, Options{SegmentMs: defaultSegmentMs})
	if err != nil {
		t.Fatal(err)
	}
	if zero.NumSegments() != explicit.NumSegments() {
		t.Errorf("SegmentMs:0 gave %d segments, want %d (same as the explicit default %d)",
			zero.NumSegments(), explicit.NumSegments(), defaultSegmentMs)
	}
	// A live regression bound, independent of the second plan: with the
	// keyframe cadence above, the correct default (6000 ms) cuts at most 2
	// boundaries for 10 s of content; a stuck SegmentMs of 0 would cut nearly
	// one per keyframe (10).
	if zero.NumSegments() > 3 {
		t.Errorf("SegmentMs:0 gave %d segments over 10 s - looks like SegmentMs stayed 0 instead of defaulting to %d",
			zero.NumSegments(), defaultSegmentMs)
	}
}

// --- peekTail: the -1 "unseen" sentinels and the laced-frame count ---------
// (hlsplan.go:472 lastPts/prevPts/lastBlockTC init, :490 lastFrames++)

// hlsplanPeekTailFixture writes a minimal Matroska file: one video keyframe
// (for the Cues a real plan needs elsewhere) and, in the same cluster, one
// laced audio SimpleBlock of 3 frames sharing a single stored (Block)
// timecode of 0 - the sentinel-distinguishing case, since a track whose
// tail only reaches Timecode 0 is the one input where "unseen" (-1) and
// "seen, value 0" actually differ in the greater-than comparisons peekTail
// runs against them.
func hlsplanPeekTailFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peektail.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	sr := 48000.0
	ch := uint8(2)
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteMetadata(c, tracks, 0); err != nil {
		t.Fatal(err)
	}
	var cl bytes.Buffer
	cl.Write(rawUintElem(mkv.IDTimestamp, 0, 2))
	writeRawBlock(&cl, 1, 0, true, []byte{0x00, 0x00, 0x00, 0x01, 0x65})
	writeLacedRawBlock(&cl, 2, 0, [][]byte{{0xAA, 1}, {0xAA, 2}, {0xAA, 3}})
	m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: 0, Track: 1, ClusterPos: m.RelPos()})
	if _, err := ebml.WriteElementHeader(m.W, mkv.IDCluster, int64(cl.Len())); err != nil {
		t.Fatal(err)
	}
	if _, err := m.W.Write(cl.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPeekTailSentinelsAndLacedFrameCount(t *testing.T) {
	src := hlsplanPeekTailFixture(t)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	ai := -1
	for i, pt := range plan.tracks {
		if pt.ft.outTrack.mkv.ID == 2 {
			ai = i
		}
	}
	if ai < 0 {
		t.Fatal("audio track not found in plan.tracks")
	}
	lastPts, prevPts, lastFrames, err := plan.peekTail(context.Background(), plan.offsets[len(plan.offsets)-1])
	if err != nil {
		t.Fatal(err)
	}
	// A -1 sentinel lets Timecode 0 register as "seen" (0 > -1): the audio
	// track's frames all sit at Timecode 0, so lastPts must become exactly
	// 0, not stay at an un-registered sentinel value.
	if lastPts[ai] != 0 {
		t.Errorf("lastPts = %d, want 0 (the -1 sentinel must let Timecode 0 register)", lastPts[ai])
	}
	// The second and third frames repeat the same Timecode as the first, so
	// prevPts catches up to 0 too (it tracks the second-largest PTS,
	// including repeats) - it must NOT stay stuck at an un-registered
	// sentinel value (1, what an INVERT_NEGATIVES/ARITHMETIC_BASE mutant on
	// the initial -1 would produce, since 0 is never > 1).
	if prevPts[ai] != 0 {
		t.Errorf("prevPts = %d, want 0 (repeated Timecode 0 must still register, not stay at a sentinel)", prevPts[ai])
	}
	// Three laced frames share the block's timecode: lastFrames must count up
	// to 3 (INCREMENT_DECREMENT on lastFrames[i]++ would drive it negative).
	if lastFrames[ai] != 3 {
		t.Errorf("lastFrames = %d, want 3 (three laced frames sharing one block timecode)", lastFrames[ai])
	}
}

// --- grid duration recovery for laced, no-DefaultDuration audio ------------
// (hlsplan.go:258 kLast, :264 the collapsed-lace correction, :268 durMediaTS)

// hlsplanIntegerMsLacedFixture is buildLacedFixtureOpt's shape (25 fps video,
// laced audio, no DefaultDuration declared) but with an INTEGER millisecond
// frame duration: real AC-3/E-AC-3 muxing (32 ms/frame), unlike AAC's
// fractional 21.333 ms. An integer stride makes the plan's closed-form
// last-frame math land EXACTLY where the full pass's incremental derivation
// does, so every resource - including the init segment's total duration,
// which is exactly where kLast/durMediaTS feed - must be byte-identical.
// Both tracks' first sample is delayed by one frame/lace: a zero-based first
// PTS would make the plan's "lastPts - firstPtsMs" grid subtraction
// indistinguishable from an addition (firstPtsMs would be 0), hiding an
// INVERT_NEGATIVES/ARITHMETIC_BASE mutant on that "-".
func hlsplanIntegerMsLacedFixture(t *testing.T, frameDurMs int64, framesPerLace int, durMs int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "laced_int.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	sr := 48000.0
	ch := uint8(2)
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteMetadata(c, tracks, durMs); err != nil {
		t.Fatal(err)
	}
	laceDurMs := frameDurMs * int64(framesPerLace)
	audioStartMs := laceDurMs
	audioBlocks := 0
	for ts := int64(0); ts < durMs; ts += 1000 {
		var cl bytes.Buffer
		cl.Write(rawUintElem(mkv.IDTimestamp, uint64(ts), 2))
		start := 0
		if ts == 0 {
			start = 1 // delay the video track's first sample too: a zero-based
			// firstPtsMs would hide the else branch's "- scale(firstPtsMs)".
		}
		for i := start; i < 25; i++ { // video: 40 ms frames, keyframe first
			data := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, byte(ts/1000), byte(i))
			writeRawBlock(&cl, 1, int16(int64(i)*40), i == start, data)
		}
		for { // audio: the laced blocks whose stored pts falls in this cluster
			pts := audioStartMs + int64(audioBlocks)*laceDurMs
			if pts >= ts+1000 || pts >= durMs {
				break
			}
			frames := make([][]byte, framesPerLace)
			for i := range frames {
				frames[i] = []byte{0xAF, byte(audioBlocks), byte(i), 0x01}
			}
			writeLacedRawBlock(&cl, 2, int16(pts-ts), frames)
			audioBlocks++
		}
		m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: ts, Track: 1, ClusterPos: m.RelPos()})
		if _, err := ebml.WriteElementHeader(m.W, mkv.IDCluster, int64(cl.Len())); err != nil {
			t.Fatal(err)
		}
		if _, err := m.W.Write(cl.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanHLSGridDurationIntegerMsMatchesFullPass(t *testing.T) {
	src := hlsplanIntegerMsLacedFixture(t, 32, 4, 5000) // 32 ms/frame, 4-frame lace, 5 s
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range plan.Resources() {
		switch name {
		case "master.m3u8", "manifest.mpd":
			continue // BANDWIDTH is estimated on the Matroska plan
		}
		got, _, err := plan.Resource(context.Background(), name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil {
			t.Errorf("full pass did not write %s", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes) - the recovered grid duration diverged",
				name, len(got), len(want))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no resources compared")
	}
}

// --- relTCMaxMs boundaries (hlsplan.go:1026) --------------------------------

// relTCMaxMs falls back to the raw 32767 relative-timecode range unless
// tcScale is strictly between 0 and math.MaxInt64/32767; a CONDITIONALS_
// BOUNDARY mutant on either comparison changes what happens exactly at 0 or
// at the division's own boundary.
func TestRelTCMaxMsBoundaries(t *testing.T) {
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(320), Height: u32(240)},
	}, []genBlock{{track: 1, pts: 0, key: true, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65}}})
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}

	limit := int64(math.MaxInt64 / 32767)
	cases := []struct {
		name    string
		tcScale int64
		want    int64
	}{
		// tcScale > 0 is false here: must fall back to 32767, not compute
		// 32767*0/1e6 = 0 (what a ">= 0" mutant would give).
		{"zero", 0, 32767},
		{"negative", -1000, 32767},
		// A normal scale computes the scaled range.
		{"typical-1_000_000", 1_000_000, 32767},
		{"half-scale", 500_000, 16383},
		// Exactly at the division boundary: tcScale < limit is false, so the
		// safe fallback applies; a "<=" mutant would instead compute
		// 32767*limit/1e6, a huge value, at this exact point.
		{"at-overflow-boundary", limit, 32767},
		{"just-under-boundary", limit - 1, 32767 * (limit - 1) / 1_000_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan.tcScale = c.tcScale
			if got := plan.relTCMaxMs(); got != c.want {
				t.Errorf("relTCMaxMs() with tcScale=%d = %d, want %d", c.tcScale, got, c.want)
			}
		})
	}
}

// --- subNeedsNextCue boundary (hlsplan.go:1238) -----------------------------

// subNeedsNextCue must inspect EVERY trailing cue sharing the last start
// time, down to index 0: a CONDITIONALS_BOUNDARY mutant turning "i >= 0"
// into "i > 0" would skip cues[0] whenever it is the earliest of that run.
func TestSubNeedsNextCueChecksIndexZero(t *testing.T) {
	cues := []subtitle.Cue{
		{StartMs: 5000, EndMs: 0, Text: "no end, shares the last start"}, // index 0: needs a successor
		{StartMs: 5000, EndMs: 5500, Text: "has an end, same start"},
		{StartMs: 5000, EndMs: 5800, Text: "has an end, same start"},
	}
	if !subNeedsNextCue(cues, 10_000) {
		t.Error("subNeedsNextCue = false, want true (cues[0] shares the last start and has no end)")
	}
	// Flip it: give cues[0] a real end too - now nothing needs a successor.
	cues[0].EndMs = 5200
	if subNeedsNextCue(cues, 10_000) {
		t.Error("subNeedsNextCue = true, want false (every cue sharing the last start now has an end)")
	}
}

// --- subtitleSegment: the n out-of-range boundary (hlsplan.go:1282) --------

// The last valid windowed subtitle segment index is segCount-1: a
// CONDITIONALS_BOUNDARY mutant turning "n >= p.segCount" into "n > p.segCount"
// would let n == segCount slip through and index p.bounds out of range.
func TestSubtitleSegmentOutOfRangeBoundary(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 3; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 2000, key: true,
			data: []byte("cue")})
	}
	sortGenBlocks(gblocks)
	src := buildPlanFixture(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, gblocks, nil)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	last := plan.NumSegments() - 1
	if _, err := plan.subtitleSegment(context.Background(), 0, last); err != nil {
		t.Errorf("subtitleSegment(last=%d): %v, want success", last, err)
	}
	if _, err := plan.subtitleSegment(context.Background(), 0, last+1); err == nil {
		t.Errorf("subtitleSegment(segCount=%d) succeeded, want an out-of-range error", last+1)
	}
}

// --- planHLSFromMP4: the iframe/DASH gating conjunction ---------------------
// (hlsplan.go:1440 "video != nil && o.Encrypt == nil && o.CENC == nil &&
// len(iframes) > 0", :1451 "o.Encrypt == nil")

// hlsplanMP4Source writes a synthetic h264+aac MKV, remuxes it to MP4, and
// returns the MP4 path - the source planHLSFromMP4 (the MP4-sniffed PlanHLS
// path) plans from.
func hlsplanMP4Source(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 300; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, gblocks)
	mp4Src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, mp4Src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	return mp4Src
}

// Under the plain (unencrypted, non-CENC) conjunction every clause is true,
// so iframe.m3u8 and manifest.mpd must both be offered - the baseline a
// CONDITIONALS_NEGATION on ANY one of the four "&&" clauses, or on
// Options.Encrypt's gate, would break.
func TestPlanHLSFromMP4IframeAndDASHGating(t *testing.T) {
	src := hlsplanMP4Source(t)

	t.Run("plain", func(t *testing.T) {
		plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		if !containsName(plan.Resources(), "iframe.m3u8") {
			t.Error("Resources() must list iframe.m3u8 for an unencrypted, non-CENC MP4 plan")
		}
		if !containsName(plan.Resources(), "manifest.mpd") {
			t.Error("Resources() must list manifest.mpd for an unencrypted MP4 plan")
		}
	})

	t.Run("encrypted", func(t *testing.T) {
		enc := &HLSEncryption{Key: bytes.Repeat([]byte{0x42}, 16), KeyURI: "https://example.test/key"}
		plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, Encrypt: enc})
		if err != nil {
			t.Fatal(err)
		}
		if containsName(plan.Resources(), "iframe.m3u8") {
			t.Error("Resources() must NOT list iframe.m3u8 for an AES-128-encrypted MP4 plan")
		}
		if containsName(plan.Resources(), "manifest.mpd") {
			t.Error("Resources() must NOT list manifest.mpd for an AES-128-encrypted MP4 plan")
		}
	})

	t.Run("cenc", func(t *testing.T) {
		cenc := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc")}
		plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, CENC: cenc})
		if err != nil {
			t.Fatal(err)
		}
		if containsName(plan.Resources(), "iframe.m3u8") {
			t.Error("Resources() must NOT list iframe.m3u8 for a CENC MP4 plan")
		}
		// CENC still gets a DASH manifest (unlike Options.Encrypt) - only the
		// iframe gate excludes it.
		if !containsName(plan.Resources(), "manifest.mpd") {
			t.Error("Resources() must still list manifest.mpd for a CENC MP4 plan")
		}
	})
}

// --- planHLSFromMP4: movie metadata rides only the primary rendition -------
// (hlsplan.go:1457 "i == primaryIndex(fts)")

// The container title must land in the primary (video) rendition's init
// segment only - a CONDITIONALS_NEGATION mutant ("i != primaryIndex(fts)")
// would swap which rendition gets it: the audio init would carry the title
// and the video init would not.
func TestPlanHLSFromMP4TitleOnPrimaryRenditionOnly(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 300; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	mkvSrc := buildMKVTitled(t, "Mon Titre MP4 Plan", []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, gblocks)
	mp4Src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, mp4Src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanHLS(context.Background(), mp4Src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	videoInit, _, err := plan.Resource(context.Background(), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	audioInit, _, err := plan.Resource(context.Background(), "init_a1.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(videoInit, []byte("Mon Titre MP4 Plan")) {
		t.Error("init.mp4 (primary/video rendition) must carry the container title")
	}
	if bytes.Contains(audioInit, []byte("Mon Titre MP4 Plan")) {
		t.Error("init_a1.mp4 (non-primary rendition) must NOT carry the container title")
	}
}

// --- mp4SegmentTrack: an empty window must not index out of range ----------
// (hlsplan.go:1497/:1507 "end > start")

// A rendition whose samples run out before the presentation's end (a shorter
// audio track) must serve a valid, empty-sample trailing segment instead of
// panicking on ft.samples[start] - a CONDITIONALS_BOUNDARY mutant turning
// "end > start" into "end >= start" would do exactly that (start == end ==
// len(samples) still satisfies ">=", indexing ft.samples[start] out of range).
func TestMP4SegmentTrackEmptyTrailingWindow(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 150; i++ { // video: 6 s (40 ms frames), keyframe every 1 s
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 100; i++ { // audio: only 2 s (20 ms frames) - ends well before the video
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, gblocks)
	mp4Src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, mp4Src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), mp4Src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), mp4Src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() < 3 {
		t.Fatalf("plan has %d segments, want at least 3 (audio must run dry before the last one)", plan.NumSegments())
	}
	// The trailing audio segments (audio ends at 2 s, video runs to 6 s) must
	// have zero samples but still be servable, byte-identical to the full pass.
	for n := plan.NumSegments() - 1; n >= plan.NumSegments()-2; n-- {
		name := hlsplanRenditionSegmentName(t, plan, n)
		got, err := plan.mp4SegmentTrack(context.Background(), hlsplanAudioTrackIndex(t, plan), n)
		if err != nil {
			t.Fatalf("mp4SegmentTrack(audio, %d): %v", n, err)
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil {
			t.Fatalf("full pass did not write %s", name)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
}

// hlsplanAudioTrackIndex returns the plan's audio rendition index (the non-primary
// track), for direct mp4SegmentTrack calls.
func hlsplanAudioTrackIndex(t *testing.T, plan *HLSPlan) int {
	t.Helper()
	fts := plan.fts()
	for i, ft := range fts {
		if !ft.outTrack.spec.video {
			return i
		}
	}
	t.Fatal("no audio track in plan")
	return -1
}

// hlsplanRenditionSegmentName returns the audio rendition's n-th segment file name.
func hlsplanRenditionSegmentName(t *testing.T, plan *HLSPlan, n int) string {
	t.Helper()
	fts := plan.fts()
	return renditionSegment(fts, hlsplanAudioTrackIndex(t, plan), n)
}
