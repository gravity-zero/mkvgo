package mp4

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/internal/poison"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// mux.go - RemuxToMP4: read a Matroska/WebM source and write a progressive
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
	// frameSamples is the samples one frame covers at the track's rate, from
	// its header (frameSamplesFromHeader) or its first payload (learnFrame); 0 unknown.
	frameSamples int64
	frameLearned bool
	mp3Delay     bool // Options.MP3ContainerDelay: carry an MP3 delay as an edit list
	// conv, when non-nil (Options.FrameConverter claimed this track), re-encodes
	// each frame just before it is written to a segment. The track's identity
	// (Codec, CodecPrivate, spec) has already been rebound to the output codec.
	conv TrackConverter

	samples    trackSamples
	pending    []byte // current chunk's sample bytes, not yet written to mdat
	pendingCnt int
	// pendingSized counts the chunk's bytes known by size alone (a sizing pass walked by block headers).
	pendingSized int64
	// hasher accumulates the track's content SHA-256 while its samples stream
	// (Options.ContentHashes); stored as a freeform ilst atom for `verify`.
	hasher hash.Hash

	// one-cue lookahead state for timed-text tracks.
	hasPendingCue bool
	pendCuePTS    int64
	pendCueDur    int64
	pendCueText   []byte
	mediaEndMs    int64 // the source's duration, which a cue stating none never runs past (0 unknown)

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
		if err := t.emitSample(cw, encodeChapterSample(ch.Title), ch.StartMs, ch.StartMs, dur, true); err != nil {
			return err
		}
	}
	return flushChunk(cw, t)
}

// emitSample records one sample's metadata and buffers its bytes for the next
// chunk flush. It is the shared low-level append used by both the media and the
// timed-text paths. blockPts is the source block's stored timecode (pts for
// synthetic/timed-text samples).
func (t *outTrack) emitSample(cw *countWriter, data []byte, pts, blockPts, dur int64, sync bool) error {
	if uint64(len(data)) > math.MaxUint32 {
		return errf("track %d: sample of %d bytes exceeds MP4 sample size limit", t.mkv.ID, len(data))
	}
	t.samples.addDur(uint32(len(data)), pts, blockPts, dur, sync)
	if t.hasher != nil {
		_, _ = t.hasher.Write(data) // hash.Hash.Write never errors
	}
	if t.pending == nil {
		t.pending = make([]byte, 0, chunkByteThreshold+len(data)) // a chunk flushes past the threshold
	}
	t.pending = append(t.pending, data...)
	t.pendingCnt++
	if int64(len(t.pending))+t.pendingSized >= chunkByteThreshold || t.pendingCnt >= chunkSampleThreshold {
		return flushChunk(cw, t)
	}
	return nil
}

// emitSized is emitSample for a sample known by its size alone: the sizing pass of a fast start.
func (t *outTrack) emitSized(cw *countWriter, size, pts, blockPts, dur int64, sync bool) error {
	if size > math.MaxUint32 {
		return errf("track %d: sample of %d bytes exceeds MP4 sample size limit", t.mkv.ID, size)
	}
	t.samples.addDur(uint32(size), pts, blockPts, dur, sync)
	t.pendingSized += size
	t.pendingCnt++
	if int64(len(t.pending))+t.pendingSized >= chunkByteThreshold || t.pendingCnt >= chunkSampleThreshold {
		return flushChunk(cw, t)
	}
	return nil
}

// RemuxToMP4 reads the Matroska/WebM file at srcPath and writes a progressive
// MP4 to dstPath. It never transcodes: each track's compressed samples are
// copied verbatim into MP4 sample tables.
//
// Supported codecs are H.264, HEVC, AV1 and VP9 (video) and AAC, Opus, AC-3, E-AC-3,
// FLAC, MP3 and DTS (audio), plus SRT subtitles (carried as tx3g timed text).
// By default a video or audio track in an unsupported codec (e.g. TrueHD)
// aborts the remux with an error, so the output never silently omits content;
// set Options.SkipUnsupported to drop such tracks instead (each reported via
// Options.OnDrop). The output places moov after mdat (not "fast start"); play it
// from a local file, or post-process for progressive HTTP streaming.
//
// Composition reordering (B-frames) is preserved via a signed ctts box. Timing
// uses a 1 ms movie timescale, which is exact for the default Matroska
// TimecodeScale (finer source scales are quantised to milliseconds); audio uses
// its sample rate as the media timescale, keeping the audio path sample-exact.
//
// Metadata NOT carried into MP4: attachments (fonts, cover art), track-targeted
// tags, and global tags outside the mapped set (ARTIST, ALBUM, DATE_RELEASED,
// GENRE, COMMENT, ENCODER, COMPOSER, DESCRIPTION → iTunes ilst). Chapters are
// flattened (sub-chapters and untitled chapters are omitted) and the Nero chpl
// list caps at 255 entries; the QuickTime chapter track carries the full list.
func RemuxToMP4(ctx context.Context, srcPath, dstPath string, opts ...Options) (err error) {
	o := optionsFrom(opts)
	fs := o.FS

	// The attachment payloads stay on disk: only the cover art is carried into
	// the MP4, and it is read on its own below (loadCoverArt). The in-band
	// fallback completes the colour and HDR10 static metadata the sample entry
	// is written from, for a track whose container left them short.
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData(), reader.WithInBandColourFallback())
	if err != nil {
		return err
	}
	cover, err := loadCoverArt(fs, c.Attachments)
	if err != nil {
		return err
	}
	tracks, brands, err := planTracks(c, o)
	if err != nil {
		return err
	}

	total := int64(-1)
	if o.Progress != nil {
		if fi, e := fs.DoStat(srcPath); e == nil {
			total = fi.Size()
		}
	}
	// openBlocks opens one pass over the source's blocks; with several passes
	// the progress runs once from 0 to the end over all of them.
	openBlocks := func(pass, passes int) (*reader.BlockReader, func(), error) {
		src, err := fs.DoOpen(srcPath)
		if err != nil {
			return nil, nil, err
		}
		br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
		if err != nil {
			src.Close()
			return nil, nil, err
		}
		br.SetTrackDefaultDurations(reader.TrackDefaultDurations(c.Tracks))
		if o.Progress != nil {
			if total < 0 {
				br.SetProgress(o.Progress, total)
			} else {
				base, all := int64(pass)*total, int64(passes)*total
				br.SetProgress(func(done, _ int64) { o.Progress(base+done, all) }, total)
			}
		}
		return br, func() { src.Close() }, nil
	}

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

	meta := movieMeta{
		title:    c.Info.Title,
		tags:     globalTags(c),
		chapters: c.Chapters,
		cover:    cover,
	}
	// A remote source is read once: its fast start goes through a temporary file.
	remote := o.FastStart && hintsRanges(fs, srcPath)
	return writeMP4(ctx, dst, srcPath, dstPath, remote, c, openBlocks, tracks, brands, meta, o)
}

// movieMeta bundles the file-level metadata carried into the moov box.
type movieMeta struct {
	title    string
	tags     []mkv.SimpleTag
	chapters []mkv.Chapter
	cover    *coverArt
	// hashes maps MP4 track_ID -> hex content SHA-256, filled once the samples
	// have streamed (Options.ContentHashes).
	hashes map[uint32]string
}

// collectHashes finalises the per-track content digests after the sample pass.
func collectHashes(tracks []*outTrack) map[uint32]string {
	var out map[uint32]string
	for _, t := range tracks {
		if t.hasher == nil || t.isChapter {
			continue
		}
		if out == nil {
			out = map[uint32]string{}
		}
		out[t.mp4ID] = hex.EncodeToString(t.hasher.Sum(nil))
	}
	return out
}

// globalTags collects the file-level (non track-targeted) SimpleTags of a container,
// which to-mp4 carries into the MP4 movie metadata (see buildMovieMeta).
func globalTags(c *mkv.Container) []mkv.SimpleTag {
	var out []mkv.SimpleTag
	for _, t := range c.Tags {
		if t.TargetID == 0 {
			out = append(out, t.SimpleTags...)
		}
	}
	return out
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

		// An encrypted track's bytes are not its frames: mkvgo does not decrypt.
		if t.Encryption != "" {
			reason := "encrypted (" + t.Encryption + "): mkvgo does not decrypt"
			if o.SkipUnsupported {
				o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec, Reason: reason})
				continue
			}
			return nil, nil, errf("track %d (%s) is %s (set Options.SkipUnsupported to drop it)", t.ID, t.Codec, reason)
		}
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
			// zlib and bzlib payloads are inflated into the samples; a scheme
			// mkvgo cannot decode would reach the player still compressed.
			switch t.Compression {
			case mkv.CompressionNone, mkv.CompressionHeaderStrip, mkv.CompressionZlib, mkv.CompressionBzlib:
			default:
				if o.SkipUnsupported {
					o.report(DroppedTrack{ID: t.ID, Type: t.Type, Codec: t.Codec,
						Reason: "ContentCompression " + t.Compression.String() + " cannot be decoded"})
					continue
				}
				return nil, nil, errf("track %d: ContentCompression %s cannot be decoded for MP4 (zlib and bzlib are) - re-mux the track uncompressed first, or set Options.SkipUnsupported to drop it", t.ID, t.Compression)
			}
			spec = s
		}
		ot := &outTrack{
			mkv:        t,
			spec:       spec,
			mp4ID:      nextID,
			frameDurMs: frameDurationMs(t),
			mp3Delay:   o.MP3ContainerDelay,
			mediaEndMs: c.DurationMs,
		}
		ot.frameSamples = frameSamplesFromHeader(&t)
		if o.ContentHashes {
			ot.hasher = sha256.New()
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
	// from every media track via tref/chap, for Apple-player chapter support;
	// buildMoov ends it with the media, as it does chpl.
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

// blockOpener opens pass number pass (0-based) of passes over the source's blocks.
type blockOpener func(pass, passes int) (*reader.BlockReader, func(), error)

// sizeByHeaders makes a sizing pass walk the block headers alone of every
// track whose samples are its stored bytes: nothing of their payloads is
// read but each track's first frame. A hashed track reads its bytes.
func sizeByHeaders(br *reader.BlockReader, tracks []*outTrack) {
	var ids, sized []uint64
	for _, t := range tracks {
		if t.isChapter {
			continue
		}
		ids = append(ids, t.mkv.ID)
		if !t.spec.text && t.hasher == nil && t.mkv.Compression.Verbatim() {
			sized = append(sized, t.mkv.ID)
		}
	}
	if len(sized) == 0 {
		return
	}
	br.KeepTracks(ids...)
	br.SetHeaderOnly(true)
	br.SetHeaderOnlyTracks(sized...)
	br.SetSparse(true)
}

// routeTracks maps each source track number to its output track.
func routeTracks(tracks []*outTrack) map[uint64]*outTrack {
	routing := make(map[uint64]*outTrack, len(tracks))
	for _, t := range tracks {
		routing[t.mkv.ID] = t
	}
	return routing
}

func writeMP4(ctx context.Context, dst mkv.WriteSeekCloser, srcPath, dstPath string, remote bool, c *mkv.Container, open blockOpener, tracks []*outTrack, brands []string, meta movieMeta, o Options) error {
	ftyp := buildFtyp(brands)
	switch {
	case o.FastStart && remote:
		return writeFastStartViaTemp(ctx, dst, dstPath, open, tracks, ftyp, meta, o)
	case o.FastStart:
		return writeFastStart(ctx, dst, srcPath, c, open, tracks, ftyp, meta, o)
	}
	br, done, err := open(0, 1)
	if err != nil {
		return err
	}
	defer done()

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
	dataLen, err := streamSamples(ctx, br, tracks, routeTracks(tracks), cw, nil)
	if err != nil {
		return err
	}

	base := int64(len(ftyp)) + mdatHeaderLen
	co64 := needCo64(dataLen)
	meta.hashes = collectHashes(tracks)
	if _, err := buf.Write(buildMoov(tracks, base, co64, meta)); err != nil {
		return errf("write moov: %w", err)
	}
	if err := buf.Flush(); err != nil {
		return errf("flush output: %w", err)
	}
	return patchMdatSize(dst, int64(len(ftyp)), int64(len(ftyp))+mdatHeaderLen+dataLen)
}

// writeFastStart produces a "fast start" file (ftyp, moov, mdat) so a player can
// begin before the whole file is available. A sizing pass runs the sample
// pipeline into a discarding writer: the sample tables, hence the moov, are
// then exact before a byte of media is written. The write pass streams the
// media straight behind the moov, in one pass, with no temporary file and no
// copy of the mdat. It costs a second walk of the source instead - by the
// block headers alone (sizeByHeaders) - which is why a remote source goes
// through writeFastStartViaTemp.
func writeFastStart(ctx context.Context, dst mkv.WriteSeekCloser, srcPath string, c *mkv.Container, open blockOpener, tracks []*outTrack, ftyp []byte, meta movieMeta, o Options) error {
	br, done, err := open(0, 2)
	if err != nil {
		return err
	}
	// The first frames are read through their own handle: the walk's keeps its place.
	src, err := o.FS.DoOpen(srcPath)
	if err != nil {
		done()
		return err
	}
	if directReads(&o, src) {
		sizeByHeaders(br, tracks)
	}
	dataLen, err := streamSamples(ctx, br, tracks, routeTracks(tracks), &countWriter{w: io.Discard}, src)
	src.Close()
	done()
	if err != nil {
		return err
	}
	co64 := needCo64(dataLen)
	meta.hashes = collectHashes(tracks)
	moovSize := int64(len(buildMoov(tracks, 0, co64, meta))) // size is base-independent
	base := int64(len(ftyp)) + moovSize + mdatHeaderLen
	moov := buildMoov(tracks, base, co64, meta)

	// The write pass runs on fresh track state; the drops were reported once.
	wo := o
	wo.OnDrop = nil
	wtracks, _, err := planTracks(c, wo)
	if err != nil {
		return err
	}
	br, done, err = open(1, 2)
	if err != nil {
		return err
	}
	defer done()
	out := bufio.NewWriterSize(dst, 256<<10)
	if _, err := out.Write(ftyp); err != nil {
		return errf("write ftyp: %w", err)
	}
	if _, err := out.Write(moov); err != nil {
		return errf("write moov: %w", err)
	}
	if _, err := out.Write(mdatHeader(dataLen)); err != nil {
		return errf("write mdat header: %w", err)
	}
	n, err := streamSamples(ctx, br, wtracks, routeTracks(wtracks), &countWriter{w: out}, nil)
	if err != nil {
		return err
	}
	// The moov written up front must describe the media written behind it.
	meta.hashes = collectHashes(wtracks)
	if n != dataLen || !bytes.Equal(buildMoov(wtracks, base, co64, meta), moov) {
		return errf("fast start: the media differs between the sizing and the write pass: %w", ErrSourceChanged)
	}
	return out.Flush()
}

// writeFastStartViaTemp is the fast start of a source read once (a remote
// one, which a second pass would download again): the media goes to a
// temporary file first, then the moov is written ahead of it and the media
// copied behind.
func writeFastStartViaTemp(ctx context.Context, dst mkv.WriteSeekCloser, dstPath string, open blockOpener, tracks []*outTrack, ftyp []byte, meta movieMeta, o Options) (err error) {
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

	br, done, err := open(0, 1)
	if err != nil {
		return err
	}
	defer done()
	tbuf := bufio.NewWriterSize(tmp, 256<<10)
	cw := &countWriter{w: tbuf}
	dataLen, err := streamSamples(ctx, br, tracks, routeTracks(tracks), cw, nil)
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
	meta.hashes = collectHashes(tracks)
	moovSize := int64(len(buildMoov(tracks, 0, co64, meta))) // size is base-independent
	base := int64(len(ftyp)) + moovSize + mdatHeaderLen

	out := bufio.NewWriterSize(dst, 256<<10)
	if _, err := out.Write(ftyp); err != nil {
		return errf("write ftyp: %w", err)
	}
	if _, err := out.Write(buildMoov(tracks, base, co64, meta)); err != nil {
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
// total payload length and errors if no track produced any sample. A block
// walked by its header alone (sizeByHeaders) is sized, its first frame read
// from src.
func streamSamples(ctx context.Context, br *reader.BlockReader, tracks []*outTrack, routing map[uint64]*outTrack, cw *countWriter, src io.ReadSeeker) (int64, error) {
	// emitSample copies each sample into its chunk at once, so one buffer serves every block of a
	// track whose sample entry is built (text cues and a first-frame config may hold on to theirs).
	var blockBuf []byte
	br.SetBlockBuffer(func(track uint64, size int) []byte {
		if t := routing[track]; t == nil || t.spec.text || t.sampleEntry == nil {
			return nil
		}
		blockBuf = poison.Reuse(blockBuf, size)
		return blockBuf
	})
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		b, err := br.Next()
		if isSourceEnd(err) {
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
		if b.Data == nil {
			if err := learnFirstFrame(t, b, src); err != nil {
				return 0, err
			}
			if err := t.emitSized(cw, int64(len(t.mkv.HeaderStripping))+b.Size, b.Timecode, b.BlockTimecode, 0, b.Keyframe); err != nil {
				return 0, err
			}
			continue
		}
		data, err := t.mkv.DecodePayload(b.Data)
		if err != nil {
			return 0, err
		}
		t.learnFrame(data)
		// Codecs without CodecPrivate derive their config box from the first frame.
		if t.sampleEntry == nil {
			entry, err := t.spec.sampleEntry(&t.mkv, data)
			if err != nil {
				return 0, err
			}
			t.sampleEntry = entry
		}
		if err := t.emitSample(cw, data, b.Timecode, b.BlockTimecode, 0, b.Keyframe); err != nil {
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
	return mdatDataLen > co64Above
}

// co64Above is the payload length past which chunk offsets are 64-bit: 4 GiB
// less a margin for the boxes ahead of the mdat (a variable for the tests).
var co64Above int64 = 0xFFFFFFFF - (256 << 20)

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
	cw.pos += t.pendingSized // bytes known by size alone: the sizing pass writes nothing
	t.samples.addChunk(offset, t.pendingCnt)
	t.pending = t.pending[:0]
	t.pendingCnt, t.pendingSized = 0, 0
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
