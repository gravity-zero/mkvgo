package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// diagnose.go - Diagnose is the one-call triage a media library scan needs:
// instead of stacking separate probes (an index check, an external
// audio-delay probe, a damage dry-run) per file, one call classifies the
// file and names the remedy for each finding. Head-mostly: the track list,
// index and declared size are read from the head, the audio delays from the
// first cluster(s), the track ends from a tail walk bounded by the index; the
// full tolerant walk (MapDamage) runs only when the cheap checks find the
// declared size and the real size disagree - the head-visible signature of
// truncation or trailing junk. A file with no Cues is the exception: its
// tail walk starts at the first cluster and reads it whole.

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

	// One head-only read serves the index verdict, the track ends and the hole
	// probe alike: Tracks, Cues (with their positions) and Tags.
	meta, err := reader.OpenMetaWithFS(ctx, path, fs, reader.WithCues(), reader.WithTags())
	if err != nil {
		// A .mkv that is really an ISO base media file is a CLASSIFICATION,
		// not an error: an error would put the file back in the failed pile
		// of every scan pass, while this finding settles it once - the file
		// needs a rename/remux, no Matroska repair will ever apply. (The root
		// mkvgo.Diagnose and the CLI sniff content and route such a file to
		// the MP4 triage on their own; this finding is for callers addressing
		// the Matroska engine directly.)
		if errors.Is(err, reader.ErrNotMatroska) {
			d.Findings = append(d.Findings, Finding{
				Kind:   "wrong-container",
				Detail: "the content is ISO base media (MP4/MOV), not Matroska/WebM",
				Remedy: "rename or remux the file; mkvgo.Diagnose and the CLI route by content",
			})
			// Empty, not nil: every successful Diagnosis carries the map, and
			// a consumer indexing it must not care which finding path ran.
			d.AudioDelaysNs = map[uint64]int64{}
			return d, nil
		}
		return nil, fmt.Errorf("diagnose: %w", err)
	}

	d.TimecodeScale = meta.Info.TimecodeScale

	// Index health (head-only), then where each track's content really ends
	// (statistics tags, else a tail walk: bounded, from the index, when there
	// is one; from the first cluster when there is none - a file with no Cues
	// cannot be judged from its head, and the reindex it needs reads it whole
	// anyway, so the damage the walk may pass over is found here rather than
	// by that reindex's refusal). The walked picture end is handed back to the
	// index verdict: a file that states no statistics had its tail measured
	// against the declared duration - an audio track's end, on real files -
	// and this is where that last guess is replaced by the picture's real end.
	// Declared-size coherence (head-only): the Segment's declared end vs the
	// real file size is the cheap signature of truncation or trailing junk,
	// and decides whether the tolerant walk over the whole file runs. So does
	// Options.DeepVerify: damage inside a cluster far from the tail, in a file
	// whose head is sound, is out of reach of everything else here, and a
	// caller that wants it found pays the full read deliberately.
	declaredEnd, known, err := segmentDeclaredEndOf(path, fs)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	stat, err := fs.DoStat(path)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	size := stat.Size()
	d.FileSize = size
	if known {
		d.DeclaredSize = declaredEnd
		if declaredEnd > size {
			d.MissingTailBytes = declaredEnd - size
		}
	}
	needWalk := mkv.DeepVerifyFrom(opts)
	var sizeFinding *Finding
	switch {
	case !known:
		sizeFinding = &Finding{
			Kind:   "streamed-size",
			Detail: "the Segment declares no size (a streamed or interrupted write); readers cannot bound it",
			Remedy: "mkvgo reindex (the rewrite seals the size)",
		}
	case declaredEnd > size:
		needWalk = true // truncated: measure what survives
	case declaredEnd < size:
		needWalk = true // trailing bytes: junk, or a crashed in-place journal
	}

	d.Findings = append(d.Findings, undeclaredCompressionFindings(ctx, path, fs, meta)...)

	ch := cueHealthFrom(meta, 0)
	// A file with no Cues is walked from its first cluster - unless the
	// tolerant walk is about to read it whole anyway, which finds the same
	// damage: one full read, not two.
	if ch.TotalCues > 0 || !needWalk {
		ends, err := trackEndsFrom(ctx, path, fs, meta)
		if err != nil {
			return nil, fmt.Errorf("diagnose: %w", err)
		}
		d.TrackEnds = ends
	}
	if ends := d.TrackEnds; ends != nil && ends.SkippedBytes > 0 {
		d.Findings = append(d.Findings, Finding{
			Kind: "damaged",
			Detail: fmt.Sprintf("the tail walk passed over %d byte(s) of damage inside the file: an element that cannot be read, with media continuing behind it",
				ends.SkippedBytes),
			Remedy: "mkvgo reindex --resync",
		})
	}
	if ends := d.TrackEnds; ends != nil && !ch.VideoEndExact && ends.VideoEndMs > 0 {
		ch = cueHealthFrom(meta, ends.VideoEndMs)
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
		// misskeyed: the index is on the right track, just too coarse - unless
		// the picture itself is missing where the hole is, in which case no
		// index can close it and the remedy is the source, not a reindex.
		detail, remedy := ch.Reason, "mkvgo reindex"
		if pictureMissing(ch) {
			remedy = "re-acquire the source (the picture is missing from the stream there; a reindex cannot restore it)"
		}
		// Then look inside the holes: one bounded, header-only walk per hole
		// (ProbeCueHoles) says whether a reindex has anything to cue there. The
		// head-only verdict stands wherever the probe could not conclude, and
		// a probe that fails to run is said so rather than swallowed.
		if err := probeCueHolesFrom(ctx, path, fs, meta, ch); err != nil {
			detail += fmt.Sprintf(" (hole probe failed: %v)", err)
		} else {
			detail, remedy = probedSparseVerdict(ch, detail, remedy)
		}
		d.Findings = append(d.Findings, Finding{
			Kind: "index-sparse", Detail: detail, Remedy: remedy,
		})
		// Picture missing from the stream is a PLAYBACK defect in its own
		// right - the picture freezes there whatever the index does - and gets
		// its own finding, from what the probe saw (exact, located), with the
		// track's own statistics as corroboration when they speak.
		if f, ok := pictureMissingFinding(ch, meta); ok {
			d.Findings = append(d.Findings, f)
		}
	}

	// An audio track that dies before the picture leaves a structurally
	// healthy file whose playlists promise audio that cannot exist.
	if ends := d.TrackEnds; ends != nil {
		if ends.AudioShortfallMs >= audioShortFindingMs {
			bound := ""
			for _, e := range ends.Ends {
				if e.Track == ends.ShortAudioTrack && e.Source == "walk-bound" {
					bound = " at least"
				}
			}
			d.Findings = append(d.Findings, Finding{
				Kind: "audio-short",
				Detail: fmt.Sprintf("audio track %d ends%s %.0fs before the picture (at %s, the picture ends at %s)",
					ends.ShortAudioTrack, bound, secs(ends.AudioShortfallMs), clockMs(ends.VideoEndMs-ends.AudioShortfallMs), clockMs(ends.VideoEndMs)),
				Remedy: "re-acquire the source (the audio is missing from the file; playback pads it with silence)",
				Track:  ends.ShortAudioTrack,
			})
		}
	}

	// Audio start delays (first clusters).
	delays, err := AudioStartDelays(ctx, path, opts...)
	if err != nil {
		return nil, fmt.Errorf("diagnose: %w", err)
	}
	d.AudioDelaysNs = delays
	// In track order: a map walk would report the same file's findings in a
	// different order from one call to the next.
	for _, track := range sortedTracks(delays) {
		ns := delays[track]
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

	if sizeFinding != nil {
		d.Findings = append(d.Findings, *sizeFinding)
	}

	// The tolerant walk, when the size check warranted it or the caller asked.
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
			// midDamage counts the ranges lost INSIDE the file, the cut tail
			// aside: a download can be both cut short and missing pieces in
			// its middle, and the first verdict must not hide the second.
			midDamage, midBytes := 0, int64(0)
			for _, r := range report.DamagedRanges {
				if r.StartOffset < declaredEnd {
					bodyDamage++
				}
				if r.StartOffset < declaredEnd && r.EndOffset < size {
					midDamage++
					midBytes += r.EndOffset - r.StartOffset
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
				if midDamage > 0 {
					d.Findings = append(d.Findings, Finding{
						Kind: "damaged",
						Detail: fmt.Sprintf("%d damaged range(s) inside the file as well, %d bytes unrecoverable (missing or overwritten regions before the cut)",
							midDamage, midBytes),
						Remedy: "re-download the source (mkvgo reindex --resync keeps what the file still holds meanwhile)",
					})
				}
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

	if meta.ResyncedBytes > 0 {
		// Junk in the head - ahead of the metadata, or between it and the first
		// Cluster: the head read resynced past it, a strict rewrite will not.
		d.Findings = append(d.Findings, Finding{
			Kind: "damaged",
			Detail: fmt.Sprintf("%d undecodable byte(s) ahead of the first Cluster were skipped to read the head of the file; a reader that does not resynchronize stops there, and a strict rewrite refuses the file",
				meta.ResyncedBytes),
			Remedy: "mkvgo reindex --resync",
		})
	}

	// Whatever showed the file damaged - junk in its head, a region the tail
	// walk passed over, ranges the tolerant walk mapped inside the Segment -
	// the strict reindex is refused on it. A remedy naming it (for a missing
	// or stale index, an unsealed size...) would send the operator to a
	// refusal: name the command that works.
	damaged := meta.ResyncedBytes > 0 || (d.TrackEnds != nil && d.TrackEnds.SkippedBytes > 0)
	if d.Damage != nil {
		for _, r := range d.Damage.DamagedRanges {
			damaged = damaged || !known || r.StartOffset < declaredEnd
		}
		damaged = damaged || len(d.Damage.RepairedRanges) > 0
	}
	if damaged {
		for i := range d.Findings {
			d.Findings[i].Detail = resyncAdvice(d.Findings[i].Detail)
			d.Findings[i].Remedy = resyncAdvice(d.Findings[i].Remedy)
		}
	}

	d.Healthy = len(d.Findings) == 0
	return d, nil
}

// resyncAdvice rewrites advice naming the strict reindex into the tolerant
// one. Advice must never recommend what will be refused.
func resyncAdvice(advice string) string {
	advice = strings.ReplaceAll(advice, "mkvgo reindex", "mkvgo reindex --resync")
	return strings.ReplaceAll(advice, "--resync --resync", "--resync")
}

// adviseResync rewrites advice that names the strict reindex for a file the
// reader had to resync through (Container.ResyncedBytes): the strict rewrite
// refuses undecodable bytes, so the command that works there is the tolerant
// one. Advice must never recommend what will be refused.
func adviseResync(c *mkv.Container, advice string) string {
	if c.ResyncedBytes == 0 {
		return advice
	}
	return resyncAdvice(advice)
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
	if err := discardN(r, h1.Size); err != nil {
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

// audioShortFindingMs is how far an audio track may end before the picture
// without a finding: encoder priming and padding leave a few hundred
// milliseconds either way on any real mux, a lost stretch is seconds to
// minutes.
const audioShortFindingMs = 5_000

// pictureMissingFinding names every stretch the hole probe found without any
// video block: where the picture is missing from the stream, and for how long.
func pictureMissingFinding(ch *CueHealthReport, meta *mkv.Container) (Finding, bool) {
	var parts []string
	for _, h := range ch.Holes {
		if h.Content == "picture-missing" {
			parts = append(parts, fmt.Sprintf("%.0fs at %s", secs(h.VideoAbsentMs), clockMs(h.AtMs)))
		}
	}
	if len(parts) == 0 {
		return Finding{}, false
	}
	detail := "the picture is missing from the stream: " + strings.Join(parts, ", ")
	if ch.VideoShortfallMs > 0 {
		detail += fmt.Sprintf(" (the video track states %.0fs less picture than its duration at its frame rate holds)", secs(ch.VideoShortfallMs))
	}
	var track uint64
	for _, t := range meta.Tracks {
		if t.Type == mkv.VideoTrack {
			track = t.ID
			break
		}
	}
	return Finding{
		Kind:   "picture-missing",
		Detail: detail,
		Remedy: "re-acquire the source (playback freezes there; no repair restores frames the file does not hold)",
		Track:  track,
	}, true
}

// discardN skips n bytes of r in int-sized steps, so a declared size past a
// 32-bit int is still consumed to its end (or fails at EOF) instead of being
// truncated into a wrong, possibly negative, count.
func discardN(r *bufio.Reader, n int64) error {
	for n > 0 {
		step := n
		if step > math.MaxInt32 {
			step = math.MaxInt32
		}
		if _, err := r.Discard(int(step)); err != nil {
			return err
		}
		n -= step
	}
	return nil
}

// undeclaredCompressionFindings probes the first cued block of each subtitle
// track that declares no compression: a zlib stream there means the muxer
// compressed the track and lost its ContentEncodings. One cluster read per track.
func undeclaredCompressionFindings(ctx context.Context, path string, fs *mkv.FS, meta *mkv.Container) []Finding {
	var out []Finding
	for i := range meta.Tracks {
		t := &meta.Tracks[i]
		if t.Type != mkv.SubtitleTrack || t.Compression != mkv.CompressionNone {
			continue
		}
		var cue *mkv.CuePoint
		for k := range meta.Cues {
			if meta.Cues[k].Track == t.ID {
				cue = &meta.Cues[k]
				break
			}
		}
		if cue == nil {
			continue // no cue to seat on: a walk would be the only way, and diagnose stays head-only here
		}
		recovered, err := probeUndeclaredZlib(ctx, path, fs, meta, t, cue)
		if err != nil || !recovered {
			continue
		}
		out = append(out, Finding{
			Kind:   "undeclared-compression",
			Detail: fmt.Sprintf("subtitle track %d (%s) stores zlib-compressed blocks but declares no ContentCompression; mkvgo inflates them on read", t.ID, t.Codec),
			Remedy: "re-mux the track so the file declares the compression, or decompress it",
		})
	}
	return out
}

// probeUndeclaredZlib reads the cued block of t and reports whether it inflated as an undeclared zlib stream.
func probeUndeclaredZlib(ctx context.Context, path string, fs *mkv.FS, meta *mkv.Container, t *mkv.Track, cue *mkv.CuePoint) (bool, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	br, err := reader.NewBlockReaderAt(f, meta.Info.TimecodeScale, meta.SegmentStart+cue.ClusterPos)
	if err != nil {
		return false, err
	}
	br.KeepTracks(t.ID)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		blk, err := br.Next()
		if err != nil {
			return false, err
		}
		if blk.TrackNumber != t.ID {
			continue
		}
		_, recovered, err := t.DecodePayloadRecovered(blk.Data)
		return recovered, err
	}
}
