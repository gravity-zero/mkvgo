package mp4

import (
	"context"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// demux.go - RemuxFromMP4: parse an MP4's movie header and rewrite its samples
// as a seekable Matroska file. Like RemuxToMP4 it never transcodes; sample bytes
// are copied verbatim. Only the sample index is held in memory - sample data is
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

	mv, err := parseMP4Limit(src, size, sampleFull, o.moovLimit())
	if err != nil {
		return err
	}
	for _, d := range mv.dropped {
		o.report(d) // cover art / non-media tracks the MKV output does not carry
	}
	if err := carriableTracks(mv, &o); err != nil {
		return err
	}
	if sampleCount(mv) == 0 {
		return errf("the source holds no samples (an initialisation segment?): nothing to remux")
	}
	tracks := buildMKVTracks(mv, o.MP3ContainerDelay)

	if err := distinctOutput(fs, dstPath, srcPath); err != nil {
		return err
	}
	dst, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	// A remux that fails leaves no truncated file under the output's name
	// (runs after the close below; never touches a file it did not create).
	defer func() {
		if err != nil {
			_ = fs.DoRemove(dstPath)
		}
	}()
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = errf("close output: %w", cerr)
		}
	}()

	return writeMKV(ctx, dst, src, mv, tracks, size, progress)
}

// buildMKVTracks maps the parsed MP4 tracks to Matroska tracks (1-based IDs).
// mp3Delay carries an MP3 edit-list delay into CodecDelay (opt-in, see wantsEditList).
func buildMKVTracks(mv *movie, mp3Delay bool) []mkv.Track {
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
		// track-selection metadata probers report (not gated by codec support).
		if t.languageKnown {
			mt.Language = t.language
			mt.LanguageBCP47 = t.languageBCP47
			mt.LanguagePresent = true
		}
		mt.Name = t.name // QuickTime udta/name -> Matroska TrackEntry Name
		mt.HandlerName = t.handlerName
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
			mt.ColourDetermined = t.colourDetermined
			// Fall back to the codec bitstream (e.g. H.264 SPS VUI) for any colour
			// field the colr box did not supply - matches the conventional color_space on
			// SDR streams that carry colour only in the SPS.
			reader.FillFromCodecPrivate(&mt)
			// A Matroska V_VP9 CodecPrivate is the VP9 Codec Feature Metadata,
			// not a vpcC: convert once the vpcC's colour has been read into the
			// track (it goes to the Colour element, where Matroska keeps it).
			if mt.Codec == "vp9" {
				if fm, ok := reader.VP9FeatureMetadataFromVpcC(mt.CodecPrivate); ok {
					mt.CodecPrivate = fm
				}
			}
			mt.DolbyVision = t.dolbyVision
			mt.HDR = t.hdr
			mt.StereoMode = t.stereoMode
			mt.Projection = t.projection
			// Nominal frame rate from the stts header (head-only), falling back to
			// the sample-timing average when the table was built (Keyframes opt-in).
			fps := t.frameRate
			if fps == 0 {
				fps = videoFrameRate(t.samples)
			}
			if fps > 0 {
				mt.FrameRate = &fps
			}
			// A constant frame duration becomes the Matroska DefaultDuration:
			// without it an MKV written from this track leaves its frame rate
			// to be guessed from millisecond timecodes (23.81 fps for 24).
			mt.DefaultDurationNs = t.frameDurNs
			mt.Rotation, mt.HFlip = t.rotation, t.hflip
			mt.FrameCount = t.frameCount
			if t.videoBitDepth > 0 && mt.VideoBitDepth == nil {
				bd := t.videoBitDepth
				mt.VideoBitDepth = &bd
			}
		case mkv.AudioTrack:
			if t.channels > 0 {
				ch := t.channels
				mt.Channels = &ch
			}
			if t.bitDepth > 0 {
				bd := t.bitDepth
				mt.BitDepth = &bd
			}
			if t.sampleRate > 0 {
				sr := t.sampleRate
				mt.SampleRate = &sr
			}
			if t.outputSampleRate > 0 {
				osr := t.outputSampleRate
				mt.OutputSampleRate = &osr
			}
			// Constant frame duration (single-entry stts) → DefaultDuration, so
			// MKV outputs declare it and packaging grid-times the audio.
			mt.DefaultDurationNs = t.frameDurNs
			// The AAC profile lives only in the AudioSpecificConfig.
			reader.FillFromCodecPrivate(&mt)
		}
		if t.bitrate > 0 {
			br := t.bitrate
			mt.Bitrate = &br
		}
		mt.Encryption = t.encryption
		mt.DurationMs = t.durationMs
		if t.presentMs > 0 { // the edit list ends the presentation, not the media
			mt.DurationMs = t.presentMs
		}
		if wantsEditList(t.codec, mp3Delay) {
			mt.CodecDelay = t.codecDelayNs
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
	sr := newTrackWindows(src, size, len(mv.tracks))

	var (
		group        []mkv.Block
		groupStart   int64
		groupLo      int64 // the group's lowest and highest block timecode: a SimpleBlock's offset is an int16
		groupHi      int64
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
		data, err := sr.read(ref.track, tk.samples, ref.idx)
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

		if groupHasData && (s.dtsMs-groupStart >= clusterWindowMs ||
			max(groupHi, block.Timecode)-min(groupLo, block.Timecode) > clusterSpanMax) {
			if err := flush(); err != nil {
				return err
			}
		}
		if !groupHasData {
			groupStart = s.dtsMs
			groupLo, groupHi = block.Timecode, block.Timecode
			groupHasData = true
		}
		groupLo, groupHi = min(groupLo, block.Timecode), max(groupHi, block.Timecode)
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

// trackWindowBytes is what trackWindows reads at once: a track's samples are
// stored in runs (its chunks), so one read serves a run where a read a sample
// cost a call each.
const trackWindowBytes = 1 << 20

// trackWindows reads a movie's samples through as many windows as tracks; a
// sample any window holds is served from it, so an interleaved file is read
// once, in order, the windows a ring over its last megabytes.
type trackWindows struct {
	src  io.ReadSeeker
	at   io.ReaderAt
	size int64
	wins []trackWindow
}

type trackWindow struct {
	off int64
	buf []byte // the source's bytes from off
}

func newTrackWindows(src io.ReadSeeker, size int64, tracks int) *trackWindows {
	sr := &trackWindows{src: src, size: size, wins: make([]trackWindow, tracks)}
	sr.at, _ = src.(io.ReaderAt)
	return sr
}

// read returns a copy of track ti's sample idx: from the windows holding its
// bytes, the oldest window refilled from the first byte none holds.
func (sr *trackWindows) read(ti int, samples []inSample, idx int) ([]byte, error) {
	off, end := samples[idx].offset, samples[idx].offset+int64(samples[idx].size)
	if off < 0 || end > sr.size {
		return nil, io.ErrUnexpectedEOF // a size the table declares past the source is not allocated
	}
	out := make([]byte, 0, end-off)
	for pos := off; pos < end; {
		if w := sr.holding(pos); w != nil {
			k := min(end, w.off+int64(len(w.buf)))
			out = append(out, w.buf[pos-w.off:k-w.off]...)
			pos = k
			continue
		}
		n := min(int64(trackWindowBytes), sr.size-pos)
		for i := range sr.wins {
			if w := &sr.wins[i]; w.off > pos && w.off < pos+n {
				n = w.off - pos // what that window holds is not read again
			}
		}
		if n < end-pos && n == sr.size-pos || n <= 0 {
			return nil, io.ErrUnexpectedEOF // the source ends before the sample does
		}
		if end-pos > n && n == trackWindowBytes {
			rest, err := readSample(sr.src, pos, uint32(end-pos)) // larger than a window: read as it is
			if err != nil {
				return nil, err
			}
			return append(out, rest...), nil
		}
		w := sr.oldest()
		if cap(w.buf) < int(n) {
			w.buf = make([]byte, n)
		}
		w.buf, w.off = w.buf[:n], pos
		if err := sr.fill(w); err != nil {
			w.buf = w.buf[:0]
			return nil, err
		}
	}
	return out, nil
}

// oldest returns the window to refill: the one ending first in the file, as
// the samples are asked for in decode order - the bytes the other tracks will
// still ask for sit in the windows ending last.
func (sr *trackWindows) oldest() *trackWindow {
	w := &sr.wins[0]
	for i := range sr.wins {
		if o := &sr.wins[i]; o.off+int64(len(o.buf)) < w.off+int64(len(w.buf)) {
			w = o
		}
	}
	return w
}

// holding returns the window holding the byte at pos, nil when none does.
func (sr *trackWindows) holding(pos int64) *trackWindow {
	for i := range sr.wins {
		if w := &sr.wins[i]; pos >= w.off && pos < w.off+int64(len(w.buf)) {
			return w
		}
	}
	return nil
}

func (sr *trackWindows) fill(w *trackWindow) error {
	if sr.at != nil {
		_, err := sr.at.ReadAt(w.buf, w.off)
		return err
	}
	if _, err := sr.src.Seek(w.off, io.SeekStart); err != nil {
		return err
	}
	_, err := io.ReadFull(sr.src, w.buf)
	return err
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

// editedDurationMs is the longest track presentation when an edit list sets one, each other track ending with its media; 0 when no track has an edit list.
func editedDurationMs(mv *movie) int64 {
	var max int64
	edited := false
	for i := range mv.tracks {
		t := &mv.tracks[i]
		end := t.presentMs
		if end > 0 {
			edited = true
		} else if t.sttsClipped && t.durationMs > 0 {
			end = t.durationMs
		} else if s := t.samples; len(s) > 0 {
			end = s[len(s)-1].ctsMs
		} else if t.sampleEndMs > 0 {
			end = t.sampleEndMs
		} else {
			end = t.durationMs
		}
		max = maxInt64(max, end)
	}
	if !edited {
		return 0
	}
	return max
}

func movieDurationMs(mv *movie) int64 {
	var max int64
	for i := range mv.tracks {
		t := &mv.tracks[i]
		var end int64
		if t.sttsClipped && t.durationMs > 0 {
			end = t.durationMs // its header's: the timeline was clipped
		} else if s := t.samples; len(s) > 0 {
			end = s[len(s)-1].ctsMs
		} else {
			// sampleKeyframes mode builds no sample table but records the end.
			end = t.sampleEndMs
		}
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

// Bounds on the samples read whole outside a segment: a probe, a subtitle, the first frame a sample entry comes from.
const (
	maxProbeSampleBytes    = 64 << 10
	maxSubtitleSampleBytes = 1 << 20
	maxFrameSampleBytes    = 64 << 20
)

// readSampleMax is readSample refusing, before reading, a sample the table declares larger than limit.
func readSampleMax(r io.ReadSeeker, off int64, size uint32, limit int, what string) ([]byte, error) {
	if int64(size) > int64(limit) {
		return nil, errf("a %s sample of %d bytes, past the %d one holds: the file's sample table is damaged", what, size, limit)
	}
	return readSample(r, off, size)
}

func readSample(r io.ReadSeeker, off int64, size uint32) ([]byte, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf, err := readExact(r, int64(size))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
