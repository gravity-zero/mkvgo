package ops

import (
	"bufio"
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// diagnose.go - Diagnose is the one-call triage a media library scan needs:
// instead of stacking separate probes (an index check, an external
// audio-delay probe, a damage dry-run) per file, one call classifies the
// file and names the remedy for each finding. Head-mostly: the track list,
// index and declared size are read from the head, the audio delays from the
// first cluster(s); the full tolerant walk (MapDamage) runs only when the
// cheap checks find the declared size and the real size disagree - the
// head-visible signature of truncation or trailing junk.

// audioDelayFindingThresholdNs is the delay above which an audio track's
// late start becomes a finding (the raw per-track values are always in the
// report for callers with their own threshold).
const audioDelayFindingThresholdNs = 100_000_000 // 100ms

// Finding is one diagnosed defect with its remedy. The definition lives in
// the mkv package (see mkv/reports.go), shared with the mp4 triage.
type Finding = mkv.Finding

// Diagnosis is the full triage verdict for one file - the same shape
// mp4.Diagnose returns, so one scan loop covers a mixed library. The
// definition lives in the mkv package (see mkv/reports.go).
type Diagnosis = mkv.Diagnosis

// Diagnose classifies path in one call: seek-index health (head-only), audio
// start delays (first clusters), declared-size coherence (head-only), and -
// only when the size check suggests damage - the full tolerant walk. Every
// finding names its remedy, so a caller can route straight to the right
// repair (reindex / retime / resync / re-download). Matroska/WebM only: an
// MP4's sample table is its index by construction, and its triage needs none
// of this.
func Diagnose(ctx context.Context, path string, opts ...mkv.Options) (*Diagnosis, error) {
	fs := mkv.FSFrom(opts)
	d := &Diagnosis{}

	// Index health (head-only).
	ch, err := CueHealth(ctx, path, opts...)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	d.CueHealth = ch
	switch {
	case ch.Healthy:
	case ch.TotalCues == 0:
		d.Findings = append(d.Findings, Finding{
			Kind: "no-index", Detail: ch.Reason,
			Remedy: "mkvgo reindex (or serve with SynthesizeIndex)",
		})
	case ch.UnknownTrackCues > 0:
		d.Findings = append(d.Findings, Finding{
			Kind: "index-stale-tracks", Detail: ch.Reason, Remedy: "mkvgo reindex",
		})
	case ch.VideoCues == 0:
		d.Findings = append(d.Findings, Finding{
			Kind: "index-misskeyed", Detail: ch.Reason, Remedy: "mkvgo reindex",
		})
	default:
		// Video cues exist but leave a hole too wide to seek into. Distinct from
		// misskeyed: the index is on the right track, just too coarse.
		d.Findings = append(d.Findings, Finding{
			Kind: "index-sparse", Detail: ch.Reason, Remedy: "mkvgo reindex",
		})
	}

	// Audio start delays (first clusters).
	delays, err := AudioStartDelays(ctx, path, opts...)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	d.AudioDelaysNs = delays
	for track, ns := range delays {
		if ns >= audioDelayFindingThresholdNs {
			d.Findings = append(d.Findings, Finding{
				Kind:    "audio-delay",
				Detail:  fmt.Sprintf("audio track %d starts %dms after the video", track, ns/1_000_000),
				Remedy:  fmt.Sprintf("mkvgo retime --shift %d=-%d", track, ns/1_000_000),
				Track:   track,
				DelayNs: ns,
			})
		}
	}

	// Declared-size coherence (head-only): the Segment's declared end vs the
	// real file size is the cheap signature of truncation or trailing junk.
	declaredEnd, known, err := segmentDeclaredEndOf(path, fs)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	stat, err := fs.DoStat(path)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	size := stat.Size()
	needWalk := false
	switch {
	case !known:
		d.Findings = append(d.Findings, Finding{
			Kind:   "streamed-size",
			Detail: "the Segment declares no size (a streamed or interrupted write); readers cannot bound it",
			Remedy: "mkvgo reindex (the rewrite seals the size)",
		})
	case declaredEnd > size:
		needWalk = true // truncated: measure what survives
	case declaredEnd < size:
		needWalk = true // trailing bytes: junk, or a crashed in-place journal
	}

	// The tolerant walk, only when the size check warranted it.
	if needWalk {
		report, derr := MapDamage(ctx, path, opts...)
		if derr != nil {
			d.Findings = append(d.Findings, Finding{
				Kind:   "damaged",
				Detail: fmt.Sprintf("the tolerant walk itself failed: %v", derr),
				Remedy: "re-download the source",
			})
		} else {
			d.Damage = report
			// Damage entirely beyond the declared Segment end is not
			// corruption: it is the trailing bytes themselves (junk, or a
			// crashed in-place journal), which the walker cannot parse by
			// definition.
			bodyDamage := 0
			for _, r := range report.DamagedRanges {
				if r.StartOffset < declaredEnd {
					bodyDamage++
				}
			}
			switch {
			case declaredEnd > size && report.TruncatedTail:
				d.Findings = append(d.Findings, Finding{
					Kind: "truncated",
					Detail: fmt.Sprintf("the source ends early: %d of %d declared bytes present, a repair recovers the playable prefix only",
						size, declaredEnd),
					Remedy: "re-download the source (mkvgo salvage keeps the playable prefix meanwhile)",
				})
			case bodyDamage > 0 || len(report.RepairedRanges) > 0:
				d.Findings = append(d.Findings, Finding{
					Kind: "damaged",
					Detail: fmt.Sprintf("%d damaged range(s), %d bytes unrecoverable, %d repairable region(s)",
						len(report.DamagedRanges), report.BytesSkipped, len(report.RepairedRanges)),
					Remedy: "mkvgo reindex --resync",
				})
			}
			if declaredEnd < size {
				d.Findings = append(d.Findings, Finding{
					Kind:   "trailing-junk",
					Detail: fmt.Sprintf("%d byte(s) beyond the declared Segment end", size-declaredEnd),
					Remedy: "mkvgo reindex (the rewrite drops them; run RecoverInPlace first if an in-place repair crashed here)",
				})
			}
		}
	}

	d.Healthy = len(d.Findings) == 0
	return d, nil
}

// segmentDeclaredEndOf reads the EBML and Segment headers and returns the
// declared absolute end of the Segment, or known=false for an unknown-size
// (streamed) Segment.
func segmentDeclaredEndOf(path string, fs *mkv.FS) (end int64, known bool, err error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4096)
	h1, n1, err := ebml.ReadElementHeader(r)
	if err != nil || h1.ID != ebml.IDEBMLHeader || h1.Size < 0 {
		return 0, false, fmt.Errorf("not a Matroska file")
	}
	if _, err := r.Discard(int(h1.Size)); err != nil {
		return 0, false, err
	}
	h2, n2, err := ebml.ReadElementHeader(r)
	if err != nil || h2.ID != mkv.IDSegment {
		return 0, false, fmt.Errorf("expected Segment")
	}
	if h2.Size < 0 {
		return 0, false, nil
	}
	return int64(n1) + h1.Size + int64(n2) + h2.Size, true, nil
}
