package mp4

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// mux.go — RemuxToMP4: read a Matroska/WebM source and write a progressive
// (non-fragmented) MP4. Media data is streamed into mdat in a single pass; the
// moov, whose size depends on the final sample tables, is written afterwards.
// Only the per-sample metadata is held in memory, so the working set scales with
// the sample count, not the file size.

const (
	// chunkByteThreshold and chunkSampleThreshold bound how much sample data is
	// buffered per track before a chunk is flushed to mdat. They keep the
	// stsc/stco tables compact (one entry per chunk, not per sample) while
	// holding memory to roughly threshold × number-of-tracks.
	chunkByteThreshold   = 1 << 20 // 1 MiB
	chunkSampleThreshold = 2048
)

// outTrack is the mux-time state for one output track.
type outTrack struct {
	mkv         mkv.Track
	spec        codecSpec
	mp4ID       uint32 // 1-based MP4 track_ID
	sampleEntry []byte // pre-built stsd sample entry (validated up front)
	frameDurMs  int64  // duration hint for the final sample (0 = unknown)
	mp3Delay    bool   // Options.MP3ContainerDelay: carry an MP3 delay as an edit list

	samples    trackSamples
	pending    []byte // current chunk's sample bytes, not yet written to mdat
	pendingCnt int

	// one-cue lookahead state for timed-text tracks.
	hasPendingCue bool
	pendCuePTS    int64
	pendCueDur    int64
	pendCueText   []byte

	// chapter-track state. isChapter marks the synthetic QuickTime chapter track
	// (its samples are the titles in chapterList); chapterRefID, set on the media
	// tracks, is the chapter track's ID they reference via tref/chap.
	isChapter    bool
	chapterRefID uint32
	chapterList  []mkv.Chapter
}

// emitChapterSamples writes the chapter titles as QuickTime text samples.
func (t *outTrack) emitChapterSamples(cw *countWriter) error {
	for i, ch := range t.chapterList {
		var dur int64
		switch {
		case i+1 < len(t.chapterList):
			dur = t.chapterList[i+1].StartMs - ch.StartMs
		case ch.EndMs > ch.StartMs:
			dur = ch.EndMs - ch.StartMs
		default:
			dur = defaultCueDurMs
		}
		if dur <= 0 {
			dur = 1
		}
		if err := t.emitSample(cw, encodeChapterSample(ch.Title), ch.StartMs, dur, true); err != nil {
			return err
		}
	}
	return flushChunk(cw, t)
}

// emitSample records one sample's metadata and buffers its bytes for the next
// chunk flush. It is the shared low-level append used by both the media and the
// timed-text paths.
func (t *outTrack) emitSample(cw *countWriter, data []byte, pts, dur int64, sync bool) error {
	if len(data) > math.MaxUint32 {
		return errf("track %d: sample of %d bytes exceeds MP4 sample size limit", t.mkv.ID, len(data))
	}
	t.samples.addDur(uint32(len(data)), pts, dur, sync)
	t.pending = append(t.pending, data...)
	t.pendingCnt++
	if len(t.pending) >= chunkByteThreshold || t.pendingCnt >= chunkSampleThreshold {
		return flushChunk(cw, t)
	}
	return nil
}

// RemuxToMP4 reads the Matroska/WebM file at srcPath and writes a progressive
// MP4 to dstPath. It never transcodes: each track's compressed samples are
// copied verbatim into MP4 sample tables.
//
// Supported codecs are H.264, HEVC and AV1 (video) and AAC, Opus, AC-3, E-AC-3,
// FLAC, MP3 and DTS (audio), plus SRT subtitles (carried as tx3g timed text).
// By default a video or audio track in an unsupported codec (e.g. TrueHD)
// aborts the remux with an error, so the output never silently omits content;
// set Options.SkipUnsupported to drop such tracks instead (each reported via
// Options.OnDrop). The output places moov after mdat (not "fast start"); play it
// from a local file, or post-process for progressive HTTP streaming.
//
// Composition reordering (B-frames) is preserved via a signed ctts box. Timing
// uses a 1 ms timescale, which is exact for the default Matroska TimecodeScale.
func RemuxToMP4(ctx context.Context, srcPath, dstPath string, opts ...Options) (err error) {
	o := optionsFrom(opts)
	fs := o.FS

	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}
	tracks, brands, err := planTracks(c, o)
	if err != nil {
		return err
	}

	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
	if err != nil {
		return err
	}
	if o.Progress != nil {
		total := int64(-1)
		if fi, e := fs.DoStat(srcPath); e == nil {
			total = fi.Size()
		}
		br.SetProgress(o.Progress, total)
	}

	dst, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = errf("close output: %w", cerr)
		}
	}()

	return writeMP4(ctx, dst, dstPath, br, tracks, brands, c.Info.Title, c.Chapters, o)
}

// planTracks selects the output tracks and validates each codec up front so the
// remux fails before touching the destination if a track cannot be carried.
func planTracks(c *mkv.Container, o Options) ([]*outTrack, []string, error) {
	var (
		tracks []*outTrack
		brands []string
		nextID uint32 = 1
	)
	for i := range c.Tracks {
		t := c.Tracks[i]

		// Subtitles never fail the remux: they are either carried or dropped with a
		// reason, so a stray subtitle codec cannot block an otherwise-fine file.
		var spec codecSpec
		if t.Type == mkv.SubtitleTrack {
			s, ok := subtitleCarriage(t.Codec, o.FlattenStyledSubs, o.NativeWebVTT)
			if !ok {
				o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
					Reason: subtitleDropReason(t.Codec)})
				continue
			}
			spec = s
		} else {
			s, ok := lookupCodec(t.Codec)
			if !ok {
				if o.SkipUnsupported {
					o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
						Reason: "codec not supported for MP4"})
					continue
				}
				return nil, nil, errf("track %d: codec %q cannot be remuxed to MP4 (set Options.SkipUnsupported to drop it)", t.ID, t.Codec)
			}
			spec = s
		}
		ot := &outTrack{
			mkv:        t,
			spec:       spec,
			mp4ID:      nextID,
			frameDurMs: frameDurationMs(t),
			mp3Delay:   o.MP3ContainerDelay,
		}
		// Codecs whose config comes from CodecPrivate are validated now (fail
		// fast); those needing the first frame are built lazily during streaming.
		if !spec.needsFirstFrame {
			entry, err := spec.sampleEntry(&t, nil)
			if err != nil {
				if o.SkipUnsupported {
					o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec, Reason: err.Error()})
					continue
				}
				return nil, nil, err
			}
			ot.sampleEntry = entry
		}
		tracks = append(tracks, ot)
		brands = append(brands, spec.brand)
		nextID++
	}
	if len(tracks) == 0 {
		return nil, nil, errf("no MP4-compatible video or audio tracks found")
	}
	// Synthesize a QuickTime chapter track (titles as text samples), referenced
	// from every media track via tref/chap, for Apple-player chapter support.
	if chs := flattenChapters(c.Chapters); len(chs) > 0 {
		chapID := nextID
		for _, t := range tracks {
			if !t.spec.text { // reference from audio/video only, not subtitle tracks
				t.chapterRefID = chapID
			}
		}
		tracks = append(tracks, &outTrack{
			spec:        codecSpec{handler: "text"},
			mp4ID:       chapID,
			isChapter:   true,
			chapterList: chs,
			sampleEntry: buildChapterTextEntry(),
		})
		brands = append(brands, "")
	}
	return tracks, brands, nil
}

// mdatHeaderLen is the fixed size of the 64-bit-largesize mdat box header.
const mdatHeaderLen = 16

func writeMP4(ctx context.Context, dst mkv.WriteSeekCloser, dstPath string, br *reader.BlockReader, tracks []*outTrack, brands []string, title string, chapters []mkv.Chapter, o Options) error {
	ftyp := buildFtyp(brands)
	routing := make(map[uint64]*outTrack, len(tracks))
	for _, t := range tracks {
		routing[t.mkv.ID] = t
	}
	if o.FastStart {
		return writeFastStart(ctx, dst, dstPath, br, tracks, ftyp, routing, title, chapters, o)
	}

	// Normal layout: ftyp, mdat (header + data), then moov at the end. The mdat
	// box size is backpatched once the data length is known.
	buf := bufio.NewWriterSize(dst, 256<<10)
	if _, err := buf.Write(ftyp); err != nil {
		return errf("write ftyp: %w", err)
	}
	if _, err := buf.Write(mdatHeaderPlaceholder()); err != nil {
		return errf("write mdat header: %w", err)
	}
	cw := &countWriter{w: buf} // pos counts mdat payload only (relative offsets)
	dataLen, err := streamSamples(ctx, br, tracks, routing, cw)
	if err != nil {
		return err
	}

	base := int64(len(ftyp)) + mdatHeaderLen
	co64 := needCo64(dataLen)
	if _, err := buf.Write(buildMoov(tracks, base, co64, title, chapters)); err != nil {
		return errf("write moov: %w", err)
	}
	if err := buf.Flush(); err != nil {
		return errf("flush output: %w", err)
	}
	return patchMdatSize(dst, int64(len(ftyp)), int64(len(ftyp))+mdatHeaderLen+dataLen)
}

// writeFastStart produces a "fast start" file (ftyp, moov, mdat) so a player can
// begin before the whole file is available. The media is streamed to a temporary
// file first; the moov (whose size is fixed once the chunk-offset width is known)
// is then written ahead of it, with chunk offsets pointing past it.
func writeFastStart(ctx context.Context, dst mkv.WriteSeekCloser, dstPath string, br *reader.BlockReader, tracks []*outTrack, ftyp []byte, routing map[uint64]*outTrack, title string, chapters []mkv.Chapter, o Options) (err error) {
	fs := o.FS
	tmpPath := dstPath + ".mdat.tmp"
	tmp, err := fs.DoCreate(tmpPath)
	if err != nil {
		return errf("create temp: %w", err)
	}
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			tmp.Close()
		}
		_ = fs.DoRemove(tmpPath) // best-effort cleanup of the temporary mdat file
	}()

	tbuf := bufio.NewWriterSize(tmp, 256<<10)
	cw := &countWriter{w: tbuf}
	dataLen, err := streamSamples(ctx, br, tracks, routing, cw)
	if err != nil {
		return err
	}
	if err := tbuf.Flush(); err != nil {
		return errf("flush temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return errf("close temp: %w", err)
	}
	tmpClosed = true

	co64 := needCo64(dataLen)
	moovSize := int64(len(buildMoov(tracks, 0, co64, title, chapters))) // size is base-independent
	base := int64(len(ftyp)) + moovSize + mdatHeaderLen

	out := bufio.NewWriterSize(dst, 256<<10)
	if _, err := out.Write(ftyp); err != nil {
		return errf("write ftyp: %w", err)
	}
	if _, err := out.Write(buildMoov(tracks, base, co64, title, chapters)); err != nil {
		return errf("write moov: %w", err)
	}
	if _, err := out.Write(mdatHeader(dataLen)); err != nil {
		return errf("write mdat header: %w", err)
	}
	in, err := fs.DoOpen(tmpPath)
	if err != nil {
		return errf("reopen temp: %w", err)
	}
	defer in.Close()
	if _, err := io.Copy(out, in); err != nil {
		return errf("copy media: %w", err)
	}
	return out.Flush()
}

// streamSamples reads every block, writes the media payload to cw (whose pos is
// the mdat-relative offset), and fills each track's sample table. It returns the
// total payload length and errors if no track produced any sample.
func streamSamples(ctx context.Context, br *reader.BlockReader, tracks []*outTrack, routing map[uint64]*outTrack, cw *countWriter) (int64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, errf("read block: %w", err)
		}
		t, ok := routing[b.TrackNumber]
		if !ok {
			continue
		}
		if t.spec.text {
			if err := t.addTextBlock(cw, b); err != nil {
				return 0, err
			}
			continue
		}
		data := t.mkv.RestoreHeader(b.Data)
		// Codecs without CodecPrivate derive their config box from the first frame.
		if t.sampleEntry == nil {
			entry, err := t.spec.sampleEntry(&t.mkv, data)
			if err != nil {
				return 0, err
			}
			t.sampleEntry = entry
		}
		if err := t.emitSample(cw, data, b.Timecode, 0, b.Keyframe); err != nil {
			return 0, err
		}
	}
	// Flush the final buffered cue of each text track, emit chapter samples, then
	// flush all pending chunks.
	for _, t := range tracks {
		if t.spec.text && t.hasPendingCue {
			if err := t.flushPendingCue(cw, -1); err != nil {
				return 0, err
			}
		}
	}
	for _, t := range tracks {
		if t.isChapter {
			if err := t.emitChapterSamples(cw); err != nil {
				return 0, err
			}
		}
	}
	for _, t := range tracks {
		if err := flushChunk(cw, t); err != nil {
			return 0, err
		}
	}

	hasSamples := false
	for _, t := range tracks {
		if len(t.samples.samples) > 0 {
			hasSamples = true
			break
		}
	}
	if !hasSamples {
		return 0, errf("no samples found in any track")
	}
	return cw.pos, nil
}

// needCo64 reports whether 64-bit chunk offsets are required. It decides from the
// payload length alone (with a safety margin for the preceding boxes) so the moov
// size is stable for the fast-start two-build layout.
func needCo64(mdatDataLen int64) bool {
	return mdatDataLen > 0xFFFFFFFF-(256<<20)
}

// flushChunk writes a track's buffered samples to mdat as one chunk and records
// its mdat-relative offset.
func flushChunk(cw *countWriter, t *outTrack) error {
	if t.pendingCnt == 0 {
		return nil
	}
	offset := uint64(cw.pos)
	if _, err := cw.Write(t.pending); err != nil {
		return errf("write mdat: %w", err)
	}
	t.samples.addChunk(offset, t.pendingCnt)
	t.pending = t.pending[:0]
	t.pendingCnt = 0
	return nil
}

// frameDurationMs converts a track's nominal frame rate to a per-frame duration
// in milliseconds, used as the final sample's duration. Returns 0 when unknown.
func frameDurationMs(t mkv.Track) int64 {
	if t.FrameRate != nil && *t.FrameRate > 0 {
		return int64(math.Round(1000 / *t.FrameRate))
	}
	return 0
}

// mdatHeaderPlaceholder returns a 16-byte mdat box header using the 64-bit
// largesize form (size==1). The largesize field is zeroed and patched later.
func mdatHeaderPlaceholder() []byte { return mdatHeader(-mdatHeaderLen) }

// mdatHeader returns a 16-byte mdat box header (64-bit largesize form) for a
// payload of dataLen bytes. The largesize covers the header plus the payload.
func mdatHeader(dataLen int64) []byte {
	var w bw
	w.u32(1) // size==1 → 64-bit largesize follows the type
	w.fourcc("mdat")
	w.u64(uint64(mdatHeaderLen + dataLen))
	return w.b
}

// patchMdatSize writes the final mdat box size into the reserved largesize field.
func patchMdatSize(dst io.WriteSeeker, mdatStart, mdatEnd int64) error {
	size := uint64(mdatEnd - mdatStart)
	if _, err := dst.Seek(mdatStart+8, io.SeekStart); err != nil {
		return errf("seek to patch mdat size: %w", err)
	}
	var w bw
	w.u64(size)
	if _, err := dst.Write(w.b); err != nil {
		return errf("patch mdat size: %w", err)
	}
	return nil
}

// countWriter tracks the absolute number of bytes written, which is the file
// offset of the next byte (used to record chunk offsets).
type countWriter struct {
	w   io.Writer
	pos int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.pos += int64(n)
	return n, err
}
