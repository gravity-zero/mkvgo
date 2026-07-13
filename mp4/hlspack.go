package mp4

// hlspack.go - MP4/MOV sources for the CMAF packager. The packaging pipeline
// (RemuxToHLS / RemuxToABR / PlanHLS) is source-agnostic past its collect
// phase; this file supplies the MP4 side: the source is sniffed from its
// first bytes, its moov gives the complete sample table (head-only - offsets,
// sizes, sync flags, timing), and samples are read straight from the mdat.
// An MP4 source therefore needs no Cues equivalent: the sample table IS the
// index, which makes the on-demand plan exact by construction.

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// packagingSource is an opened packaging input: Matroska (c only) or MP4
// (c + mv + the open reader for sample reads).
type packagingSource struct {
	c    *mkv.Container
	mv   *movie
	src  mkv.ReadSeekCloser // open MP4 reader; nil for Matroska
	size int64
}

func (ps *packagingSource) Close() {
	if ps.src != nil {
		ps.src.Close()
	}
}

// openPackagingSource sniffs and opens srcPath for packaging.
func openPackagingSource(ctx context.Context, srcPath string, fs *mkv.FS) (*packagingSource, error) {
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		f.Close()
		return nil, errf("read head: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		f.Close() // Matroska: the existing reader owns its own handle
		c, err := reader.OpenWithFS(ctx, srcPath, fs)
		if err != nil {
			return nil, err
		}
		return &packagingSource{c: c}, nil
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		f.Close()
		return nil, err
	}
	mv, err := parseMP4(f, st.Size(), sampleFull)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &packagingSource{c: containerFromMovie(mv), mv: mv, src: f, size: st.Size()}, nil
}

// collectFromMP4 is the MP4 counterpart of collectFragSamples: every media
// sample's bytes go to its track's temp file (read in rough file order via
// the DTS merge) and subtitle samples become WebVTT cues.
func collectFromMP4(ctx context.Context, ps *packagingSource,
	routing map[uint64]*fragTrack, subs []hlsSubTrack, progress mkv.ProgressFunc) error {
	subRouting := make(map[uint64]*hlsSubTrack, len(subs))
	for i := range subs {
		subRouting[subs[i].track.ID] = &subs[i]
	}
	var processed int64
	for _, ref := range mergeByDTS(ps.mv) {
		if err := ctx.Err(); err != nil {
			return err
		}
		tk := &ps.mv.tracks[ref.track]
		s := &tk.samples[ref.idx]
		id := uint64(ref.track + 1)

		if st, ok := subRouting[id]; ok {
			data, err := readSample(ps.src, s.offset, s.size)
			if err != nil {
				return errf("read subtitle sample: %w", err)
			}
			if text, ok := decodeSubtitleSample(tk.codec, data); ok {
				cue := subtitle.Cue{StartMs: s.ctsMs, Text: string(text)}
				if s.durMs > 0 {
					cue.EndMs = s.ctsMs + s.durMs
				}
				st.cues = append(st.cues, cue)
			}
			continue
		}
		ft, ok := routing[id]
		if !ok {
			continue
		}
		data, err := readSample(ps.src, s.offset, s.size)
		if err != nil {
			return errf("read sample: %w", err)
		}
		if ft.outTrack.sampleEntry == nil {
			entry, err := ft.outTrack.spec.sampleEntry(&ft.outTrack.mkv, data)
			if err != nil {
				return err
			}
			ft.outTrack.sampleEntry = entry
		}
		if _, err := ft.tmp.Write(data); err != nil {
			return errf("buffer sample: %w", err)
		}
		ft.samples = append(ft.samples, fragSample{size: s.size, ptsMs: s.ctsMs, blockPtsMs: s.ctsMs, sync: s.sync})
		processed += int64(s.size)
		if progress != nil {
			progress(processed, ps.size)
		}
	}
	return nil
}

// mp4PlanTracks builds the on-demand plan state for an MP4 source: each
// track's fragment samples fully timed from the sample table (head-only) plus
// the parallel file offsets Segment reads through. Returns the fragTracks in
// planTracks order and the per-track sample offsets.
func mp4PlanSamples(ps *packagingSource, media []*outTrack) (fts []*fragTrack, offsets [][]int64, err error) {
	for _, t := range media {
		ti := int(t.mkv.ID) - 1
		if ti < 0 || ti >= len(ps.mv.tracks) {
			return nil, nil, errf("track %d out of range", t.mkv.ID)
		}
		samples := ps.mv.tracks[ti].samples
		ft := &fragTrack{outTrack: t, timescale: mediaTimescale(t)}
		offs := make([]int64, len(samples))
		ft.samples = make([]fragSample, len(samples))
		for i := range samples {
			ft.samples[i] = fragSample{size: samples[i].size, ptsMs: samples[i].ctsMs,
				blockPtsMs: samples[i].ctsMs, sync: samples[i].sync}
			offs[i] = samples[i].offset
		}
		if len(ft.samples) == 0 {
			return nil, nil, errf("track %d produced no samples", t.mp4ID)
		}
		// The sample entry: from CodecPrivate, or lazily from the first frame.
		if t.sampleEntry == nil {
			data, rerr := readSample(ps.src, samples[0].offset, samples[0].size)
			if rerr != nil {
				return nil, nil, rerr
			}
			entry, eerr := t.spec.sampleEntry(&t.mkv, data)
			if eerr != nil {
				return nil, nil, eerr
			}
			t.sampleEntry = entry
		}
		off, hasCTS, totalTS, ctsShift := fillFragTiming(ft.samples, t.frameDurMs, ft.timescale,
			audioGridTS(t, ft.timescale))
		ft.offsetMs, ft.hasCTS, ft.durMediaTS = off, hasCTS, totalTS
		ft.ctsShiftTS = ctsShift
		ft.durMovieMs = totalTS
		if ft.timescale != movieTimescale {
			ft.durMovieMs = totalTS * int64(movieTimescale) / int64(ft.timescale)
		}
		ft.presentMs = off + ft.durMovieMs
		fts = append(fts, ft)
		offsets = append(offsets, offs)
	}
	return fts, offsets, nil
}

// mp4SubCues decodes every subtitle sample of the plan's text tracks (small
// text, held in RAM - the MP4 plan has no lazy pass to defer to).
func mp4SubCues(ps *packagingSource, subs []hlsSubTrack) error {
	for i := range subs {
		ti := int(subs[i].track.ID) - 1
		if ti < 0 || ti >= len(ps.mv.tracks) {
			continue
		}
		tk := &ps.mv.tracks[ti]
		for si := range tk.samples {
			s := &tk.samples[si]
			data, err := readSample(ps.src, s.offset, s.size)
			if err != nil {
				return errf("read subtitle sample: %w", err)
			}
			if text, ok := decodeSubtitleSample(tk.codec, data); ok {
				cue := subtitle.Cue{StartMs: s.ctsMs, Text: string(text)}
				if s.durMs > 0 {
					cue.EndMs = s.ctsMs + s.durMs
				}
				subs[i].cues = append(subs[i].cues, cue)
			}
		}
		subtitle.ResolveCueEnds(subs[i].cues, 2000)
	}
	return nil
}
