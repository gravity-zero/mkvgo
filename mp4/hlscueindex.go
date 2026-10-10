package mp4

import (
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// cueSubtitleIndex is the subtitle index a plan derives from the source's own
// Cues when the caller gave none: one entry per cued subtitle block, from the
// Cues the plan already read, so it costs no I/O to build. Its positions are
// relative to the cluster's data until the first scan of the track resolves
// them (ClusterTS -1 marks an entry still to resolve). Only a track whose cue
// count matches its trusted NUMBER_OF_FRAMES statistic is handed out: a muxer
// that cued some of a track's blocks would otherwise lose the others silently,
// where the walk finds them all.
type cueSubtitleIndex struct {
	tracks   map[uint64][]reader.IndexedBlock
	verified map[uint64]bool
}

// cueSubtitleIndexFrom derives the index for the subtitle tracks in subs and reports how many are verified and how many are cued but unverified.
func cueSubtitleIndexFrom(c *mkv.Container, subs []hlsSubTrack) (ix *cueSubtitleIndex, verified, unverified int64) {
	if len(c.Cues) == 0 || len(subs) == 0 {
		return nil, 0, 0
	}
	ix = &cueSubtitleIndex{tracks: map[uint64][]reader.IndexedBlock{}, verified: map[uint64]bool{}}
	stats := mkv.TrustedTrackStatistics(c)
	for _, s := range subs {
		id := s.track.ID
		var entries []reader.IndexedBlock
		for k := range c.Cues {
			cue := &c.Cues[k]
			if cue.Track != id || cue.RelativePos <= 0 {
				continue
			}
			entries = append(entries, reader.IndexedBlock{
				Pos:    reader.BlockPos{Off: cue.RelativePos, ClusterStart: c.SegmentStart + cue.ClusterPos, ClusterEnd: -1, ClusterTS: -1},
				TimeMs: cue.TimeMs, Frames: 1,
			})
		}
		if len(entries) == 0 {
			continue
		}
		sort.SliceStable(entries, func(a, b int) bool {
			if entries[a].Pos.ClusterStart != entries[b].Pos.ClusterStart {
				return entries[a].Pos.ClusterStart < entries[b].Pos.ClusterStart
			}
			return entries[a].Pos.Off < entries[b].Pos.Off
		})
		ix.tracks[id] = entries
		st, ok := stats[id]
		if ix.verified[id] = ok && st.Frames == int64(len(entries)); ix.verified[id] {
			verified++
		} else {
			unverified++
		}
	}
	return ix, verified, unverified
}

// Matches always holds: the index was derived from this very source.
func (ix *cueSubtitleIndex) Matches(int64, []byte, int64) bool { return true }

// TrackBlocks returns the track's cued blocks when the Cues were verified complete for it, nil otherwise.
func (ix *cueSubtitleIndex) TrackBlocks(trackID uint64) []reader.IndexedBlock {
	if !ix.verified[trackID] {
		return nil
	}
	return ix.tracks[trackID]
}

// cueResolver settles cue-derived positions on first use, reading each cluster's header once.
type cueResolver struct{ base reader.BlockPos }

// resolve replaces e's cluster-relative position by its absolute one when it is still unresolved.
func (r *cueResolver) resolve(src io.ReadSeeker, e *reader.IndexedBlock) error {
	if e.Pos.ClusterTS >= 0 {
		return nil
	}
	if r.base.ClusterStart != e.Pos.ClusterStart || r.base.Off == 0 {
		base, err := reader.ResolveClusterBlock(src, e.Pos.ClusterStart, 0)
		if err != nil {
			return err
		}
		r.base = base
	}
	off := r.base.Off + e.Pos.Off
	if r.base.ClusterEnd >= 0 && off >= r.base.ClusterEnd {
		return errf("cue-relative position %d lies past the cluster at %d", e.Pos.Off, e.Pos.ClusterStart)
	}
	e.Pos = reader.BlockPos{Off: off, ClusterStart: r.base.ClusterStart, ClusterEnd: r.base.ClusterEnd, ClusterTS: r.base.ClusterTS}
	return nil
}
