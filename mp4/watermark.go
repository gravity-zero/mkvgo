package mp4

// watermark.go - forensic A/B session watermarking, no re-encode. Given two
// pre-encoded, GOP-aligned variants of the same title (A and B, encoded so
// their segment boundaries, timeline and decoder config match but their sample
// data differs imperceptibly), a WatermarkPlan serves ONE ordinary HLS
// presentation whose per-segment bytes are drawn from A or B according to a
// per-viewer bit pattern. A leaked copy - even re-recorded off a screen and
// re-compressed - then carries a binary signature identifying the session that
// played it.
//
// mkvgo supplies the MECHANISM: it verifies the two variants are spliceable
// (same init, same segment count and durations) and routes each segment to A or
// B per a caller-supplied bit. The CODE ASSIGNMENT - which session gets which
// pattern, and collusion-resistant codes (e.g. Tardos) so several colluding
// leakers cannot erase or frame - is policy that lives above this package: the
// caller owns it and passes the resulting bits in at serve time.
//
// The manifest is identical for every viewer (A and B are aligned, so the
// playlist's segment names and durations are the same); the watermark lives
// entirely in which variant's bytes each segment carries, chosen when the
// segment is served. So one plan serves every session; only the per-segment
// A/B bit differs.

import (
	"bytes"
	"context"
)

// WatermarkPlan serves an A/B session-watermarked HLS presentation on demand.
// It is immutable after PlanWatermark returns; Segment calls are safe to run
// concurrently (each underlying variant opens its own reader per segment).
type WatermarkPlan struct {
	a, b *HLSPlan
}

// PlanWatermark plans two GOP-aligned variants of one title for session
// watermarking. srcA and srcB must be encoded so they are spliceable: identical
// init segment (same tracks and decoder configuration), the same number of
// media segments, and matching per-segment durations - so a player decoding any
// mix of A and B segments plays seamlessly. Anything else is rejected, because
// splicing mismatched encodes would break playback. Options apply to both.
func PlanWatermark(ctx context.Context, srcA, srcB string, opts ...Options) (*WatermarkPlan, error) {
	o := optionsFrom(opts)
	if o.Encrypt != nil || o.CENC != nil {
		return nil, errf("watermark planning does not support encryption in this version (encrypt the served bytes at your edge, or splice before encrypting)")
	}
	a, err := PlanHLS(ctx, srcA, o)
	if err != nil {
		return nil, errf("watermark variant A (%s): %w", srcA, err)
	}
	b, err := PlanHLS(ctx, srcB, o)
	if err != nil {
		return nil, errf("watermark variant B (%s): %w", srcB, err)
	}
	if err := assertSpliceable(a, b); err != nil {
		return nil, err
	}
	return &WatermarkPlan{a: a, b: b}, nil
}

// assertSpliceable verifies A and B segments can be interleaved seamlessly.
func assertSpliceable(a, b *HLSPlan) error {
	if a.NumSegments() != b.NumSegments() {
		return errf("watermark variants are not aligned: A has %d segments, B has %d (they must share GOP boundaries)", a.NumSegments(), b.NumSegments())
	}
	if len(a.durs) != len(b.durs) {
		return errf("watermark variants have different segment counts (%d vs %d)", len(a.durs), len(b.durs))
	}
	for i := range a.durs {
		if a.durs[i] != b.durs[i] {
			return errf("watermark variants differ in segment %d duration (%.3f vs %.3f): they must be GOP-aligned", i, a.durs[i], b.durs[i])
		}
	}
	if !bytes.Equal(a.InitSegment(), b.InitSegment()) {
		return errf("watermark variants have different init segments (different decoder configuration): the two encodes must share the same codec/SPS/PPS so their segments are interchangeable")
	}
	return nil
}

// NumSegments returns the (shared) media segment count.
func (w *WatermarkPlan) NumSegments() int { return w.a.NumSegments() }

// MasterPlaylist returns the master playlist (shared - the presentation is one
// rendition regardless of the per-session A/B routing).
func (w *WatermarkPlan) MasterPlaylist() []byte { return w.a.MasterPlaylist() }

// MediaPlaylist returns the media playlist (shared across all sessions - A and
// B are aligned, so segment names and durations are identical for everyone).
func (w *WatermarkPlan) MediaPlaylist() []byte { return w.a.MediaPlaylist() }

// InitSegment returns the (shared) init segment.
func (w *WatermarkPlan) InitSegment() []byte { return w.a.InitSegment() }

// SegmentName returns the URI name of the n-th (0-based) segment.
func (w *WatermarkPlan) SegmentName(n int) string { return w.a.SegmentName(n) }

// Segment builds the n-th (0-based) media segment for one session, drawn from
// variant B when fromB is true, otherwise variant A. The caller derives fromB
// from the session's watermark bit for segment n (bit i of the session's code).
func (w *WatermarkPlan) Segment(ctx context.Context, n int, fromB bool) ([]byte, error) {
	if fromB {
		return w.b.Segment(ctx, n)
	}
	return w.a.Segment(ctx, n)
}

// SegmentForPattern is a convenience over Segment: pattern is the session's bit
// code, one bit per segment (LSB-first within each byte); bit n selects B when
// set, A when clear. Bits past the pattern length default to A (0). This is the
// serve-time entry point a handler calls once it has mapped the session to its
// code.
func (w *WatermarkPlan) SegmentForPattern(ctx context.Context, n int, pattern []byte) ([]byte, error) {
	return w.Segment(ctx, n, patternBit(pattern, n))
}

// patternBit reports bit n of pattern (LSB-first within each byte), or false if
// n is past the end.
func patternBit(pattern []byte, n int) bool {
	byteIdx := n / 8
	if byteIdx >= len(pattern) {
		return false
	}
	return pattern[byteIdx]&(1<<(uint(n)%8)) != 0
}
