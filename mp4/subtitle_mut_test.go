package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// subtitle_mut_test.go targets gremlins survivors in subtitle.go: the tx3g
// sample entry's fixed fields, the encodeCue truncation guard, stripMarkup's
// depth counter, truncateRunes' loop guard, and the one-cue-lookahead gap/
// clamp arithmetic in flushPendingCue. Each test asserts the exact value a
// branch controls, not just "runs without error".

// TestSubtitleMutSRTEntryVerticalJustification proves subtitle.go:37 (i8(-1)
// invert-negatives/arithmetic-base): the tx3g vertical-justification byte must
// be the signed value -1 (bottom), encoded as 0xFF - not 1 or 0.
func TestSubtitleMutSRTEntryVerticalJustification(t *testing.T) {
	entry, err := srtEntry(&mkv.Track{ID: 1, Codec: "srt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 8-byte box header + 13-byte body offset (reserved(6)+dataRefIdx(2)+
	// displayFlags(4)+horizontal-justification(1) = 13, then i8(-1)).
	const off = 8 + 13
	if got := entry[off]; got != 0xFF {
		t.Fatalf("vertical-justification byte = 0x%02x, want 0xFF (i8(-1))", got)
	}
}

// TestSubtitleMutEncodeCueLengthPrefixBoundary proves subtitle.go:62's
// CONDITIONALS_NEGATION (len(b) > 0xFFFF -> <= 0xFFFF): text one byte over the
// 65535-byte limit must be truncated to exactly 0xFFFF, not left untruncated
// (which would make the two-byte length prefix wrap to 0 while the payload
// still carries 65536 bytes).
func TestSubtitleMutEncodeCueLengthPrefixBoundary(t *testing.T) {
	text := bytes.Repeat([]byte("a"), 0xFFFF+1)
	got := encodeCue(text)
	n := binary.BigEndian.Uint16(got[:2])
	if n != 0xFFFF {
		t.Fatalf("length prefix = %d, want %d (text over the limit must be truncated)", n, 0xFFFF)
	}
	if len(got) != 2+0xFFFF {
		t.Fatalf("encoded sample = %d bytes, want %d (prefix + truncated payload)", len(got), 2+0xFFFF)
	}
}

// TestSubtitleMutStripMarkupDepthGuard proves subtitle.go:85 (depth > 0
// boundary): an unmatched '>' at depth 0 must never decrement below 0. With
// "<a>>b" the first '>' closes the tag (depth 1 -> 0) and the second '>' is a
// no-op (depth stays 0), so 'b' is emitted at depth 0. Letting depth go
// negative would keep 'b' hidden.
func TestSubtitleMutStripMarkupDepthGuard(t *testing.T) {
	if got := stripMarkup("<a>>b"); got != "b" {
		t.Fatalf("stripMarkup(%q) = %q, want %q", "<a>>b", got, "b")
	}
}

// TestSubtitleMutTruncateRunesLoopBoundary proves subtitle.go:102 (max > 0
// boundary): the mid-rune backtrack must stop as soon as max reaches 0, never
// indexing (or looping) below it. With every byte a UTF-8 continuation byte,
// backtracking from max=2 over b[2],b[1],b[0] must land exactly on 0.
func TestSubtitleMutTruncateRunesLoopBoundary(t *testing.T) {
	b := []byte{0x80, 0x80, 0x80} // all mid-rune continuation bytes
	if got := truncateRunes(b, 2); got != 0 {
		t.Fatalf("truncateRunes = %d, want 0 (stop at 0, not decrement past it)", got)
	}
}

// TestSubtitleMutFlushPendingCueGapClamp proves the CONDITIONALS_NEGATION/
// BOUNDARY survivors on subtitle.go:134-135's gap-clamp: nextStart >= 0 (col
// 15), nextStart > pendCuePTS (col 33), and dur <= 0 (col 43). Each sub-test
// isolates one comparison by choosing values where only that comparison's
// truth value determines the emitted duration.
func TestSubtitleMutFlushPendingCueGapClamp(t *testing.T) {
	newTrack := func() (*outTrack, *countWriter, *bytes.Buffer) {
		var buf bytes.Buffer
		tr := &outTrack{spec: codecTable["srt"]}
		return tr, &countWriter{w: &buf}, &buf
	}

	t.Run("nextStart>=0", func(t *testing.T) {
		// pendCuePTS negative (never occurs on a real timeline, but exercises the
		// comparison in isolation) and nextStart exactly 0: nextStart>=0 must
		// hold, opening the gap-clamp branch and clamping the non-positive
		// pendCueDur to the gap (100), not to the unrelated default (2000).
		tr, cw, _ := newTrack()
		tr.pendCuePTS = -100
		tr.pendCueDur = -1
		if err := tr.flushPendingCue(cw, 0); err != nil {
			t.Fatal(err)
		}
		if got := tr.samples.samples[0].dur; got != 100 {
			t.Fatalf("dur = %d, want 100 (gap = nextStart(0) - pendCuePTS(-100))", got)
		}
	})

	t.Run("nextStart>pendCuePTS", func(t *testing.T) {
		// pendCuePTS is 0 (so there is no lead-in sample ahead of the cue we are
		// inspecting) and nextStart is also exactly 0: the branch must NOT open
		// (no overlap possible), so a positive pendCueDur (500) passes through
		// unclamped - not zeroed by a gap of 0 and then defaulted to 2000.
		tr, cw, _ := newTrack()
		tr.pendCuePTS = 0
		tr.pendCueDur = 500
		if err := tr.flushPendingCue(cw, 0); err != nil {
			t.Fatal(err)
		}
		if got := tr.samples.samples[0].dur; got != 500 {
			t.Fatalf("dur = %d, want 500 (nextStart == pendCuePTS must not open the gap clamp)", got)
		}
	})

	t.Run("dur<=0", func(t *testing.T) {
		// pendCueDur exactly 0 (missing BlockDuration) with a 300ms gap: the
		// clamp must fire and use the gap, not fall through to the unrelated
		// 2000ms default. pendCuePTS is 0 so no lead-in sample precedes the cue.
		tr, cw, _ := newTrack()
		tr.pendCuePTS = 0
		tr.pendCueDur = 0
		if err := tr.flushPendingCue(cw, 300); err != nil {
			t.Fatal(err)
		}
		if got := tr.samples.samples[0].dur; got != 300 {
			t.Fatalf("dur = %d, want 300 (gap = nextStart(300) - pendCuePTS(0))", got)
		}
	})
}

// errSubtitleMutForcedWrite is returned unconditionally by failWriter to force
// flushChunk's write error to propagate.
var errSubtitleMutForcedWrite = errors.New("subtitle_mut_test: forced write failure")

// failWriter always fails, simulating a downstream I/O error at chunk flush.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errSubtitleMutForcedWrite }

// TestSubtitleMutFlushPendingCueBridgeWriteError proves subtitle.go:148
// (CONDITIONALS_NEGATION on the bridging emitSample's err != nil check): a
// write failure at the bridging empty-sample flush must propagate, not be
// swallowed. pendingCnt is primed one below the chunk-sample threshold so the
// main cue's emitSample succeeds (no flush yet) and only the bridging one
// trips flushChunk against a writer that always fails.
func TestSubtitleMutFlushPendingCueBridgeWriteError(t *testing.T) {
	tr := &outTrack{spec: codecTable["srt"]}
	tr.pendingCnt = chunkSampleThreshold - 2
	tr.pendCuePTS = 0   // no lead-in sample (pendCuePTS must be > 0 for that)
	tr.pendCueDur = 100 // > 0 and < the 500ms gap: no clamp, main cue emits cleanly
	tr.pendCueText = []byte("x")
	cw := &countWriter{w: failWriter{}}

	err := tr.flushPendingCue(cw, 500)
	if !errors.Is(err, errSubtitleMutForcedWrite) {
		t.Fatalf("flushPendingCue error = %v, want the bridging sample's forced write error propagated", err)
	}
}
