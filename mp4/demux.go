package mp4

import (
	"context"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// demux.go — RemuxFromMP4: parse an MP4's movie header and rewrite its samples
// as a seekable Matroska file. Like RemuxToMP4 it never transcodes; sample bytes
// are copied verbatim. Only the sample index is held in memory — sample data is
// read from the source on demand, one cluster at a time.

// clusterWindowMs bounds a cluster's span (by decode time). Keeping clusters
// around a second keeps SimpleBlock relative timecodes well inside int16 range
// and gives the Cues index a useful granularity.
const clusterWindowMs = 1000

// RemuxFromMP4 reads the MP4 file at srcPath and writes an equivalent Matroska
// file to dstPath. Supported sample entries are avc1/avc3 (H.264), hvc1/hev1
// (HEVC), av01 (AV1), mp4a (AAC, MP3 or DTS, by esds object type), Opus, ac-3
// (AC-3), ec-3 (E-AC-3) and fLaC (FLAC); tracks with any other sample entry,
// and non-audio/video tracks, are dropped. It errors if no convertible track
// remains.
//
// Composition offsets (ctts) are folded back into each block's presentation
// timestamp, so B-frame ordering is preserved. The output is a normal seekable
// MKV with a SeekHead and Cues.
func RemuxFromMP4(ctx context.Context, srcPath, dstPath string, opts ...Options) (err error) {
	o := optionsFrom(opts)
	fs := o.FS
	progress := o.Progress

	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	fi, err := fs.DoStat(srcPath)
	if err != nil {
		return err
	}
	size := fi.Size()

	mv, err := parseMP4(src, size, true)
	if err != nil {
		return err
	}
	for _, d := range mv.dropped {
		o.report(d) // cover art / non-media tracks the MKV output does not carry
	}
	tracks := buildMKVTracks(mv)

	dst, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = errf("close output: %w", cerr)
		}
	}()

	return writeMKV(ctx, dst, src, mv, tracks, size, progress)
}

// buildMKVTracks maps the parsed MP4 tracks to Matroska tracks (1-based IDs).
func buildMKVTracks(mv *movie) []mkv.Track {
	tracks := make([]mkv.Track, len(mv.tracks))
	for i := range mv.tracks {
		t := &mv.tracks[i]
		id := uint64(i + 1)
		mt := mkv.Track{
			ID:           id,
			UID:          id,
			Type:         t.trackType,
			Codec:        t.codec,
			CodecPrivate: t.codecPrivate,
		}
		// Language and the "default" selection flag, so a probe carries the same
		// track-selection metadata ffprobe reports (not gated by codec support).
		if t.languageKnown {
			mt.Language = t.language
			mt.LanguageBCP47 = t.languageBCP47
			mt.LanguagePresent = true
		}
		if t.flagsKnown {
			mt.IsDefault = t.enabled
			mt.DefaultPresent = true
		}
		if t.forcedKnown {
			mt.IsForced = t.forced
			mt.ForcedPresent = true
		}
		switch t.trackType {
		case mkv.VideoTrack:
			w, h := t.width, t.height
			mt.Width, mt.Height = &w, &h
			if t.displayWidth > 0 && t.displayHeight > 0 {
				dw, dh := t.displayWidth, t.displayHeight
				mt.DisplayWidth, mt.DisplayHeight = &dw, &dh
			}
			mt.ColorPrimaries = t.colorPrimaries
			mt.ColorTransfer = t.colorTransfer
			mt.ColorSpace = t.colorMatrix
			mt.ColorRange = t.colorRange
			// Fall back to the codec bitstream (e.g. H.264 SPS VUI) for any colour
			// field the colr box did not supply — matches ffprobe's color_space on
			// SDR streams that carry colour only in the SPS.
			reader.FillColourFromCodecPrivate(&mt)
			mt.DolbyVision = t.dolbyVision
			// Nominal frame rate from the stts header (head-only), falling back to
			// the sample-timing average when the table was built (Keyframes opt-in).
			fps := t.frameRate
			if fps == 0 {
				fps = videoFrameRate(t.samples)
			}
			if fps > 0 {
				mt.FrameRate = &fps
			}
			mt.Rotation = t.rotation
			mt.FrameCount = t.frameCount
		case mkv.AudioTrack:
			if t.channels > 0 {
				ch := t.channels
				mt.Channels = &ch
			}
			if t.sampleRate > 0 {
				sr := t.sampleRate
				mt.SampleRate = &sr
			}
			if t.outputSampleRate > 0 {
				osr := t.outputSampleRate
				mt.OutputSampleRate = &osr
			}
		}
		if t.bitrate > 0 {
			br := t.bitrate
			mt.Bitrate = &br
		}
		tracks[i] = mt
	}
	return tracks
}

// videoFrameRate returns the average frame rate from a track's sample durations,
// or 0 when there are too few samples (e.g. the metadata-only probe built none).
func videoFrameRate(samples []inSample) float64 {
	if len(samples) < 2 {
		return 0
	}
	var total int64
	for i := range samples {
		total += samples[i].durMs
	}
	if total <= 0 {
		return 0
	}
	avg := float64(total) / float64(len(samples))
	if avg <= 0 {
		return 0
	}
	return 1000 / avg
}

// sampleRef points at one sample within a parsed track.
type sampleRef struct {
	track int
	idx   int
}

func writeMKV(ctx context.Context, dst mkv.WriteSeekCloser, src io.ReadSeeker, mv *movie, tracks []mkv.Track, size int64, progress mkv.ProgressFunc) error {
	m := writer.NewMKVWriter(dst)
	if err := m.WriteStart(); err != nil {
		return errf("write start: %w", err)
	}
	const scale = 1_000_000
	c := containerFromMovie(mv)
	if err := m.WriteMetadata(c, tracks, c.DurationMs); err != nil {
		return errf("write metadata: %w", err)
	}

	// Merge all tracks' samples into one decode-ordered stream.
	merged := mergeByDTS(mv)

	var (
		group        []mkv.Block
		groupStart   int64
		groupHasData bool
		processed    int64
	)
	flush := func() error {
		if len(group) == 0 {
			return nil
		}
		clusterTS := minTimecode(group)
		if err := m.WriteClusterWithCues(clusterTS, scale, group); err != nil {
			return errf("write cluster: %w", err)
		}
		group = group[:0]
		groupHasData = false
		return nil
	}

	for _, ref := range merged {
		if err := ctx.Err(); err != nil {
			return err
		}
		tk := &mv.tracks[ref.track]
		s := &tk.samples[ref.idx]
		data, err := readSample(src, s.offset, s.size)
		if err != nil {
			return errf("read sample: %w", err)
		}
		processed += int64(s.size)
		if progress != nil {
			progress(processed, size)
		}

		block := mkv.Block{TrackNumber: uint64(ref.track + 1), Timecode: s.ctsMs, Keyframe: s.sync, Data: data}
		if tk.trackType == mkv.SubtitleTrack {
			text, ok := decodeSubtitleSample(tk.codec, data)
			if !ok {
				continue // empty/gap cue → no Matroska block
			}
			block.Data = text
			block.Keyframe = true
			block.Duration = s.durMs
		}

		if groupHasData && s.dtsMs-groupStart >= clusterWindowMs {
			if err := flush(); err != nil {
				return err
			}
		}
		if !groupHasData {
			groupStart = s.dtsMs
			groupHasData = true
		}
		group = append(group, block)
	}
	if err := flush(); err != nil {
		return err
	}
	if err := m.Finalize(); err != nil {
		return errf("finalize: %w", err)
	}
	return nil
}

// mergeByDTS returns references to every sample across all tracks, ordered by
// decode time (stable, so equal-DTS samples keep per-track order). Each track's
// samples are already in decode order, so this interleaves them.
func mergeByDTS(mv *movie) []sampleRef {
	var total int
	for i := range mv.tracks {
		total += len(mv.tracks[i].samples)
	}
	refs := make([]sampleRef, 0, total)
	for ti := range mv.tracks {
		for si := range mv.tracks[ti].samples {
			refs = append(refs, sampleRef{track: ti, idx: si})
		}
	}
	sort.SliceStable(refs, func(a, b int) bool {
		return mv.tracks[refs[a].track].samples[refs[a].idx].dtsMs <
			mv.tracks[refs[b].track].samples[refs[b].idx].dtsMs
	})
	return refs
}

func minTimecode(blocks []mkv.Block) int64 {
	min := blocks[0].Timecode
	for _, b := range blocks[1:] {
		if b.Timecode < min {
			min = b.Timecode
		}
	}
	return min
}

func movieDurationMs(mv *movie) int64 {
	var max int64
	for i := range mv.tracks {
		s := mv.tracks[i].samples
		if len(s) == 0 {
			continue
		}
		last := s[len(s)-1]
		end := last.ctsMs
		if end > max {
			max = end
		}
	}
	return max
}

// decodeTx3g extracts the UTF-8 text from a tx3g sample (uint16 length + text).
// It returns ok=false for empty cues (the gap-fillers) and malformed samples.
func decodeTx3g(data []byte) ([]byte, bool) {
	if len(data) < 2 {
		return nil, false
	}
	n := int(data[0])<<8 | int(data[1])
	if n == 0 || 2+n > len(data) {
		return nil, false
	}
	return append([]byte(nil), data[2:2+n]...), true
}

func readSample(r io.ReadSeeker, off int64, size uint32) ([]byte, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
