// Package mkvgo is the container-agnostic surface of the toolkit: entry
// points that accept a media file whatever its container and route to the
// right engine, exactly like the CLI does. The per-container packages stay
// the primary API - matroska (MKV/WebM) and mp4 (MP4/MOV) - and everything
// here is a thin sniff-and-dispatch over them: the container is decided from
// the file's FIRST BYTES (EBML magic vs ISO-BMFF box), never from its name,
// so a mislabeled file routes correctly.
package mkvgo

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// RetimeTracks cancels a constant A/V desync (track number -> shift in
// nanoseconds, negative = earlier) on either container - the library
// counterpart of `mkvgo retime`, which routes the same way:
//
//   - Matroska/WebM -> matroska.RetimeTracks: block timecodes shifted, the
//     cheaper of the in-place patch and the verified rewrite picked
//     automatically;
//   - MP4/MOV -> mp4.RetimeTracks: the moov edit list re-based, samples
//     untouched, a few bytes whatever the file size.
//
// Only the options meaningful to BOTH engines ride through (FS, Progress).
// Matroska-engine-specific options (DeepVerify, StrictVerify, KeepBackup,
// RollbackSink) are refused on an MP4 source rather than silently dropped -
// call matroska.RetimeTracks directly when you need them and know the
// container. Sources that are neither container are refused with the sniffed
// reason.
func RetimeTracks(ctx context.Context, path string, shift map[uint64]int64, opts ...matroska.Options) error {
	o := matroska.Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	format, err := sniffContainer(path, &o)
	if err != nil {
		return err
	}
	if format == "mkv" {
		return matroska.RetimeTracks(ctx, path, shift, opts...)
	}
	if o.DeepVerify || o.StrictVerify || o.KeepBackup || o.RollbackSink != nil || o.RollbackRequired {
		return fmt.Errorf("mkvgo: retime on an MP4 edits the moov only; DeepVerify/StrictVerify/KeepBackup/RollbackSink apply to the Matroska engine alone (call matroska.RetimeTracks for those)")
	}
	return mp4.RetimeTracks(ctx, path, shift, mp4.Options{FS: o.FS, Progress: o.Progress})
}

// Diagnose classifies a file in one call and names the remedy for every
// finding, whatever the container - the library counterpart of
// `mkvgo diagnose`, which routes the same way:
//
//   - Matroska/WebM -> the ops triage: seek-index health, per-track audio
//     start delays, declared-size coherence, tolerant walk only when the
//     sizes disagree;
//   - MP4/MOV -> the head-only mp4 triage: box-layout truncation, missing
//     moov, trailing junk, per-track edit-list audio delays.
//
// Both routes return the same mkv.Diagnosis shape (MP4 reports carry no
// CueHealth/Damage sections - the sample table is the index by
// construction), so one scan loop covers a mixed library.
func Diagnose(ctx context.Context, path string, opts ...matroska.Options) (*mkv.Diagnosis, error) {
	o := matroska.Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	format, err := sniffContainer(path, &o)
	if err != nil {
		return nil, err
	}
	if format == "mkv" {
		return matroska.Diagnose(ctx, path, opts...)
	}
	return mp4.Diagnose(ctx, path, mp4.Options{FS: o.FS})
}

// sniffContainer classifies path from its first bytes: "mkv" for an EBML
// header (Matroska/WebM), "mp4" for an ISO-BMFF box structure.
func sniffContainer(path string, o *matroska.Options) (string, error) {
	f, err := o.FS.DoOpen(path)
	if err != nil {
		return "", fmt.Errorf("mkvgo: %w", err)
	}
	defer f.Close()
	head := make([]byte, 8)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", fmt.Errorf("mkvgo: %s: read head: %w", path, err)
	}
	if head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		return "mkv", nil
	}
	switch string(head[4:8]) {
	case "ftyp", "moov", "styp", "wide", "free", "skip", "mdat":
		return "mp4", nil
	}
	// MPEG-TS carries no header, only a 0x47 sync byte every 188 bytes. One
	// sync byte is a coincidence; a second one exactly a packet later is not,
	// so it is named rather than lumped into "unrecognised" - a transport
	// stream is a common mislabel and needs remuxing, not repair.
	if head[0] == 0x47 {
		var rest [181]byte // reach the sync byte at offset 188 (8 + 180)
		if _, err := io.ReadFull(f, rest[:]); err == nil && rest[180] == 0x47 {
			return "", fmt.Errorf("mkvgo: %s: this is an MPEG-TS transport stream, not a container mkvgo reads - remux it to MP4 or Matroska first", path)
		}
	}
	return "", fmt.Errorf("mkvgo: %s: unrecognised container (neither an EBML/Matroska header nor an ISO-BMFF box)", path)
}
