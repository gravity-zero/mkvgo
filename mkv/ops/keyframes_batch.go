package ops

// keyframes_batch.go - ExtractKeyframes / WriteKeyframeStream: the batch form
// of ExtractKeyframeSample, for trickplay strips and storyboards. One
// metadata + Cues read, one source handle, one block reader whose window is
// reused from keyframe to keyframe, offsets visited in ascending order and
// each keyframe payload read once. Selection follows the Cues: either one
// keyframe every N ms (the first cued keyframe, then the first cue at least N
// ms after the previous one kept - what `select='gte(t-prev_selected_t,N)'`
// does on keyframes) or the cued keyframe at or before each requested time.
//
// Every sample handed out is checked on its own bitstream (see
// keyframe_access.go): a cue landing on a picture a decoder cannot start on is
// skipped and counted, never emitted, so the number of samples is the number
// of pictures a decoder fed the samples must output.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/gravity-zero/mkvgo/internal/poison"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

var (
	// ErrNoKeyframeIndex: the source has no Cues for its video track, the
	// index batch extraction seeks through.
	ErrNoKeyframeIndex = errors.New("no keyframe index (Cues) for the video track - run `mkvgo reindex` to add one")
	// ErrNoVideo: the source has no video track.
	ErrNoVideo = errors.New("no video track")
	// ErrUnsupportedCodec: the video codec is not one keyframe extraction
	// packs (H.264, HEVC, VP8, VP9, AV1).
	ErrUnsupportedCodec = errors.New("keyframe extraction supports H.264, HEVC, VP8, VP9 and AV1 only")
)

// KeyframeSelect says which cued keyframes a batch extraction returns.
type KeyframeSelect struct {
	// EveryMs > 0 keeps the first cued keyframe at or after StartMs, then the
	// first cued keyframe at least EveryMs after the previous one KEPT. A cue
	// that is skipped (not a decodable access point, or not found) leaves the
	// next cue candidate for the same slot.
	EveryMs int64
	// TargetsMs (used when EveryMs is 0) asks for the cued keyframe at or
	// before each time (the first cued keyframe for a time before it). Order
	// does not matter; targets resolving to the same keyframe yield it once.
	TargetsMs []int64
	// StartMs and EndMs bound the selection to cue times in [StartMs, EndMs);
	// EndMs 0 means to the end.
	StartMs, EndMs int64
}

// KeyframeBatchSample is one keyframe of a batch extraction.
type KeyframeBatchSample struct {
	// KeyframeSample holds the sample packed for a standalone decoder, as
	// ExtractKeyframeSample returns it: Annex-B with the parameter sets
	// prepended (H.264/HEVC) or a one-frame IVF file (VP8/VP9/AV1).
	KeyframeSample
	// Payload is the frame as Matroska stores it, decompressed: length-
	// prefixed NAL units (H.264/HEVC), an OBU temporal unit (AV1, with the
	// track's sequence header OBU prepended when the frame has none) or a
	// VP8/VP9 frame. WriteKeyframeStream writes these bytes.
	Payload []byte
	Index   int    // 0-based position in the emitted sequence
	Access  string // AccessIDR, AccessCRA, AccessBLA, AccessIntra or AccessKey
	// TargetMs is the time that selected the sample: the requested target,
	// or the slot (previous kept + EveryMs, StartMs for the first).
	TargetMs int64
	// Offset and Size locate the block's payload in the source.
	Offset, Size int64
}

// KeyframeStats reports what a batch extraction did.
type KeyframeStats struct {
	Samples int     // samples emitted
	PtsMs   []int64 // their presentation times, in emission (= ascending) order
	// Skipped counts cues not emitted: the block was not a decodable access
	// point, or no video keyframe matched the cue in its cluster.
	Skipped int
	// Cues is the number of video cues; MedianCueGapMs the median distance
	// between consecutive ones (0 with fewer than two). A gap above the
	// trickplay interval means the strip will be sparser than asked.
	Cues           int
	MedianCueGapMs int64
	// BytesRead, Reads and Seeks are counted on the source handle the
	// extraction reads its blocks through (metadata reads excluded, see
	// MetaBytesRead): what was asked of the FS, which a remote FS may round
	// up to its own request size.
	BytesRead, Reads, Seeks int64
	MetaBytesRead           int64
}

// ExtractKeyframes walks the selected keyframes of srcPath's first video
// track in ascending order and calls fn with each. sample (Data and Payload
// included) is only valid during the call: its buffers are reused for the
// next one, so fn must copy what it keeps. An error from fn stops the walk
// and is returned as is; so does ctx's.
func ExtractKeyframes(ctx context.Context, srcPath string, sel KeyframeSelect, fn func(*KeyframeBatchSample) error, opts ...mkv.Options) (KeyframeStats, error) {
	return walkKeyframes(ctx, srcPath, sel, true, func(_ *mkv.Track, s *KeyframeBatchSample) error { return fn(s) }, opts)
}

// WriteKeyframeStream writes the selected keyframes of srcPath's first video
// track to w as a Matroska stream holding that track alone: EBML header, an
// unknown-size Segment, Info, Tracks (the source track's codec, codec private
// data, dimensions, Colour with its mastering metadata, Dolby Vision
// configuration), then one Cluster per keyframe with the keyframe's real
// timestamp - no Cues, no SeekHead, no other track. Payloads are written
// decompressed, so the track declares no ContentEncoding. Matroska
// BlockAdditions are not carried (in-band data such as a Dolby Vision RPU NAL
// is, being part of the frame).
//
// The stream is written as it is read: the header first, then each Cluster in
// one Write as soon as its keyframe is read, so a decoder reading w starts on
// the first picture without waiting for the walk. A Write that blocks (a
// paused consumer) holds the walk; ctx is checked between keyframes, and a
// Write blocked on a consumer that never reads again is released by closing
// the consumer's end. w is not closed.
func WriteKeyframeStream(ctx context.Context, srcPath string, sel KeyframeSelect, w io.Writer, opts ...mkv.Options) (KeyframeStats, error) {
	bw := bufio.NewWriterSize(w, 64<<10)
	var sw *writer.StreamWriter
	return walkKeyframes(ctx, srcPath, sel, false, func(t *mkv.Track, s *KeyframeBatchSample) error {
		if sw == nil {
			var err error
			if sw, err = writer.NewStreamWriter(bw, mkv.SegmentInfo{TimecodeScale: 1_000_000}, []mkv.Track{keyframeStreamTrack(t)}); err != nil {
				return err
			}
		}
		if err := sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: s.PtsMs, Keyframe: true, Data: s.Payload}); err != nil {
			return err
		}
		return bw.Flush()
	}, opts)
}

// keyframeStreamTrack is the source video track as the keyframe stream
// declares it: track 1, payloads decompressed, no lacing-related defaults.
func keyframeStreamTrack(src *mkv.Track) mkv.Track {
	t := *src
	t.ID = 1
	if t.UID == 0 {
		t.UID = 1
	}
	t.IsDefault = true
	t.Compression = mkv.CompressionNone
	t.HeaderStripping = nil
	return t
}

// walkKeyframes is the shared walk. pack says whether the decoder-ready
// KeyframeSample (Data, Codec, Ext) is built; the stream writer only needs
// Payload.
func walkKeyframes(ctx context.Context, srcPath string, sel KeyframeSelect, pack bool, emit func(*mkv.Track, *KeyframeBatchSample) error, opts []mkv.Options) (st KeyframeStats, err error) {
	fs := mkv.FSFrom(opts)
	metaFS, metaCount := countingFS(fs)
	c, err := reader.OpenMetaWithFS(ctx, srcPath, metaFS, reader.WithCues())
	st.MetaBytesRead = metaCount.bytes
	isMP4 := errors.Is(err, reader.ErrNotMatroska)
	if err != nil && !isMP4 {
		return st, err
	}

	raw, err := fs.DoOpenContext(ctx, srcPath)
	if err != nil {
		return st, err
	}
	defer raw.Close()
	src := &countingSource{ReadSeekCloser: raw}
	var meta countingSource // the MP4 movie header's share of src's counts
	defer func() {
		st.BytesRead, st.Reads, st.Seeks = src.bytes-meta.bytes, src.reads-meta.reads, src.seeks-meta.seeks
	}()

	// The candidates: the video track's cues, or an MP4's sync samples.
	var video *mkv.Track
	var points []kfPoint
	if isMP4 {
		var syncs []mp4.SyncSample
		video, syncs, err = mp4.VideoSyncSamples(ctx, src)
		meta.bytes, meta.reads, meta.seeks = src.bytes, src.reads, src.seeks
		st.MetaBytesRead += meta.bytes
		if err != nil {
			return st, err
		}
		for _, s := range syncs {
			if len(points) > 0 && points[len(points)-1].TimeMs == s.PtsMs {
				continue
			}
			points = append(points, kfPoint{TimeMs: s.PtsMs, off: s.Offset, size: s.Size})
		}
	} else if video = firstVideoTrack(c); video != nil {
		for _, cue := range videoCues(c, video.ID) {
			points = append(points, kfPoint{TimeMs: cue.TimeMs, cue: cue})
		}
	}
	if video == nil {
		return st, fmt.Errorf("%s: %w", shown.Path(srcPath), ErrNoVideo)
	}
	switch video.Codec {
	case "h264", "hevc", "vp8", "vp9", "av1":
	default:
		return st, fmt.Errorf("%s: %w (track codec %q)", shown.Path(srcPath), ErrUnsupportedCodec, video.Codec)
	}
	st.Cues = len(points)
	st.MedianCueGapMs = medianGapMs(points)
	if len(points) == 0 {
		return st, fmt.Errorf("%s: %w", shown.Path(srcPath), ErrNoKeyframeIndex)
	}

	kw := &keyframeWalker{c: c, video: video, src: src, pack: pack}
	if video.Codec == "av1" {
		kw.av1Seq = av1SequenceHeaderOBU(video.CodecPrivate)
	}
	kw.nalLen = 4
	switch video.Codec {
	case "h264":
		kw.nalLen = avcNALLengthSize(video.CodecPrivate)
		kw.paramSets = avcParameterSetsAnnexB(video.CodecPrivate)
		kw.avcPOC = newAVCPOCRenumber(video.CodecPrivate)
	case "hevc":
		kw.nalLen = hevcNALLengthSize(video.CodecPrivate)
		kw.paramSets = hevcParameterSetsAnnexB(video.CodecPrivate)
		kw.hevcPOC = newHEVCPOCRenumber(video.CodecPrivate)
	}

	try := func(p kfPoint, target int64, lastPts int64) (*KeyframeBatchSample, error) {
		s, err := kw.read(p)
		if err != nil || s == nil {
			return nil, err
		}
		if st.Samples > 0 && s.PtsMs <= lastPts {
			return nil, nil // never emit out of order or twice
		}
		s.Index, s.TargetMs = st.Samples, target
		return s, nil
	}
	accept := func(s *KeyframeBatchSample) error {
		if err := emit(video, s); err != nil {
			return err
		}
		st.Samples++
		st.PtsMs = append(st.PtsMs, s.PtsMs)
		return nil
	}
	inRange := func(ms int64) bool { return ms >= sel.StartMs && (sel.EndMs <= 0 || ms < sel.EndMs) }

	var lastPts int64
	if sel.EveryMs > 0 {
		due := sel.StartMs
		for _, p := range points {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			if sel.EndMs > 0 && p.TimeMs >= sel.EndMs {
				break
			}
			if p.TimeMs < due {
				continue
			}
			s, err := try(p, due, lastPts)
			if err != nil {
				return st, err
			}
			if s == nil {
				st.Skipped++
				continue
			}
			if err := accept(s); err != nil {
				return st, err
			}
			lastPts = s.PtsMs
			due = s.PtsMs + sel.EveryMs
		}
		return st, nil
	}

	targets := append([]int64(nil), sel.TargetsMs...)
	sort.Slice(targets, func(a, b int) bool { return targets[a] < targets[b] })
	prev := -1
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		// The cue at or before target; the first one for a target before it.
		i := sort.Search(len(points), func(k int) bool { return points[k].TimeMs > target }) - 1
		if i < 0 {
			i = 0
		}
		if i == prev || !inRange(points[i].TimeMs) {
			continue
		}
		prev = i
		s, err := try(points[i], target, lastPts)
		if err != nil {
			return st, err
		}
		if s == nil {
			st.Skipped++
			continue
		}
		if err := accept(s); err != nil {
			return st, err
		}
		lastPts = s.PtsMs
	}
	return st, nil
}

func firstVideoTrack(c *mkv.Container) *mkv.Track {
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.VideoTrack {
			return &c.Tracks[i]
		}
	}
	return nil
}

// videoCues returns the cues of track id in time order, one per time.
func videoCues(c *mkv.Container, id uint64) []mkv.CuePoint {
	out := make([]mkv.CuePoint, 0, len(c.Cues))
	for _, cu := range c.Cues {
		if cu.Track == id {
			out = append(out, cu)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].TimeMs < out[b].TimeMs })
	n := 0
	for i, cu := range out {
		if i > 0 && cu.TimeMs == out[n-1].TimeMs {
			continue
		}
		out[n] = cu
		n++
	}
	return out[:n]
}

// kfPoint is a keyframe candidate: a Matroska video cue, or an MP4 sync
// sample with its exact byte range (size > 0).
type kfPoint struct {
	TimeMs    int64
	cue       mkv.CuePoint
	off, size int64
}

func medianGapMs(points []kfPoint) int64 {
	if len(points) < 2 {
		return 0
	}
	gaps := make([]int64, len(points)-1)
	for i := 1; i < len(points); i++ {
		gaps[i-1] = points[i].TimeMs - points[i-1].TimeMs
	}
	sort.Slice(gaps, func(a, b int) bool { return gaps[a] < gaps[b] })
	return gaps[len(gaps)/2]
}

// relSeekMin is the CueRelativePosition from which the walker seats on the
// block itself (one extra small read of the cluster header) instead of
// walking the cluster from its start: below it, the bytes walked over cost
// less than the extra round trip.
const relSeekMin = 64 << 10

// keyframeWalker reads cued keyframes with one block reader and reusable
// buffers.
type keyframeWalker struct {
	c         *mkv.Container
	video     *mkv.Track
	src       *countingSource
	br        *reader.BlockReader
	pack      bool
	nalLen    int
	paramSets []byte
	av1Seq    []byte
	hevcPOC   *hevcPOCRenumber
	avcPOC    *avcPOCRenumber

	block   []byte // payload read buffer (the block reader reads into it)
	payload []byte // decompressed / sequence-header-completed frame
	data    []byte // decoder-ready packing
	sample  KeyframeBatchSample

	// The cluster the reader sits in and the last block time it returned,
	// to keep walking forward instead of re-seating within one cluster.
	clusterStart int64
	lastTc       int64

	// recent holds the last keyframe sizes, to size the range hinted to a
	// remote source (see hintKeyframe).
	recent [8]int64
	nSeen  int
}

// hintKeyframe tells a remote source (mkv.RangeHinter; a local file ignores
// it) that the next reads cover [off, off+lead+the expected keyframe size):
// about one request of about the keyframe, instead of a fixed read window -
// over HTTP on 1080p/2160p films (781/795 keyframes) the default 512 KiB
// window fetched 410/419 MB for 61/80 MB of keyframes. The size expected is
// one and a half times the median of the last eight keyframes: replayed on
// the keyframe sizes of four films (1080p/2160p, HDR10, Dolby Vision) it
// beat the largest-recent and last-size estimates on bytes for at most 1.25
// requests per keyframe; a keyframe larger than that costs one more request
// for its tail. A hint only widens a request (httpfs asks for at least its
// WindowSize), so a small WindowSize is what lets the hint size them.
// Nothing is hinted before the first keyframe has been read.
func (kw *keyframeWalker) hintKeyframe(off, lead int64) {
	n := min(kw.nSeen, len(kw.recent))
	if n == 0 {
		return
	}
	var sorted [len(kw.recent)]int64
	copy(sorted[:], kw.recent[:n])
	slices.Sort(sorted[:n])
	median := sorted[n/2]
	kw.src.hintRange(off, off+lead+median+median/2+4096)
}

func (kw *keyframeWalker) remember(size int64) {
	kw.recent[kw.nSeen%len(kw.recent)] = size
	kw.nSeen++
}

// read returns the keyframe p names, or nil when it does not lead to a
// decodable access point (the caller counts it as skipped).
func (kw *keyframeWalker) read(p kfPoint) (*KeyframeBatchSample, error) {
	if p.size > 0 {
		return kw.readExact(p)
	}
	cue := p.cue
	clusterStart := kw.c.SegmentStart + cue.ClusterPos
	if err := kw.seat(cue, clusterStart); err != nil {
		return nil, err
	}
	for {
		b, err := kw.br.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, reader.ErrClusterLimit) {
			kw.clusterStart = -1
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read block at cue %dms: %w", cue.TimeMs, err)
		}
		if kw.br.ClusterOffset() != clusterStart {
			// Walked out of the cued cluster without meeting the keyframe:
			// the cue is wrong. The reader now sits past it, so re-seat next time.
			kw.clusterStart = -1
			return nil, nil
		}
		kw.lastTc = b.Timecode
		if b.TrackNumber != kw.video.ID || b.Timecode < cue.TimeMs || (!b.Keyframe && b.Timecode != cue.TimeMs) {
			continue
		}
		kw.remember(int64(len(b.Data)))
		return kw.build(b)
	}
}

// readExact reads an MP4 sync sample: its byte range is known, so one read
// of exactly that range.
func (kw *keyframeWalker) readExact(p kfPoint) (*KeyframeBatchSample, error) {
	if p.size > maxKeyframeSize {
		return nil, nil
	}
	kw.block = poison.Reuse(kw.block, int(p.size))
	buf := kw.block
	if _, err := kw.src.Seek(p.off, io.SeekStart); err != nil {
		return nil, err
	}
	kw.src.hintRange(p.off, p.off+p.size)
	if _, err := io.ReadFull(kw.src, buf); err != nil {
		return nil, fmt.Errorf("read sync sample at %d ms: %w", p.TimeMs, err)
	}
	kw.remember(p.size)
	return kw.build(mkv.Block{TrackNumber: kw.video.ID, Timecode: p.TimeMs, Keyframe: true, Data: buf, DataOffset: p.off})
}

// maxKeyframeSize bounds the sample an MP4 sample table may claim (the block
// reader's own bound on a Matroska block).
const maxKeyframeSize = 64 << 20

// seat places the reader so its next block is at or before the cue's block.
func (kw *keyframeWalker) seat(cue mkv.CuePoint, clusterStart int64) error {
	if kw.br != nil && kw.clusterStart == clusterStart && kw.lastTc < cue.TimeMs && cue.RelativePos < relSeekMin {
		return nil // same cluster, still ahead of the reader: keep walking
	}
	kw.clusterStart = clusterStart
	kw.lastTc = -1 << 62
	if kw.br == nil {
		br, err := reader.NewBlockReaderAt(kw.src, kw.c.Info.TimecodeScale, clusterStart)
		if err != nil {
			return err
		}
		br.SetSegmentEnd(kw.c.SegmentEnd)
		br.KeepTracks(kw.video.ID)
		br.SetBlockBuffer(func(_ uint64, size int) []byte {
			kw.block = poison.Reuse(kw.block, size)
			return kw.block
		})
		kw.br = br
	} else if cue.RelativePos < relSeekMin {
		if err := kw.br.RestartAt(clusterStart); err != nil {
			return err
		}
	}
	if cue.RelativePos >= relSeekMin {
		kw.src.hintRange(clusterStart, clusterStart+24) // the cluster header ResolveClusterBlock reads
		pos, err := reader.ResolveClusterBlock(kw.src, clusterStart, cue.RelativePos)
		if err == nil && pos.ClusterTS >= 0 {
			kw.hintKeyframe(pos.Off, 64)
			return kw.br.SeekTo(pos)
		}
		if err := kw.br.RestartAt(clusterStart); err != nil {
			return err
		}
	}
	// The cluster header and whatever precedes the keyframe in it come first.
	kw.hintKeyframe(clusterStart, cue.RelativePos+256)
	return nil
}

// build checks the block's bitstream and fills the reusable sample.
func (kw *keyframeWalker) build(b mkv.Block) (*KeyframeBatchSample, error) {
	frame, err := kw.video.DecodePayload(b.Data)
	if err != nil {
		return nil, nil // undecodable payload: not a usable keyframe
	}
	kind, hasSeq, err := accessPoint(kw.video.Codec, kw.nalLen, frame)
	if err != nil {
		if errors.Is(err, ErrUnsupportedCodec) {
			return nil, err
		}
		return nil, nil
	}
	payload := frame
	switch kw.video.Codec {
	case "av1":
		if !hasSeq {
			if kw.av1Seq == nil {
				return nil, nil // no sequence header anywhere: undecodable alone
			}
			kw.payload = append(append(poison.Reuse(kw.payload, 0), kw.av1Seq...), frame...)
			payload = kw.payload
		}
	case "h264":
		// See keyframe_avc_poc.go: open-GOP I keyframes get renumbered.
		kw.payload, err = kw.avcPOC.renumber(poison.Reuse(kw.payload, 0), frame, kw.nalLen)
		if err != nil {
			return nil, nil
		}
		payload = kw.payload
	case "hevc":
		// See keyframe_hevc_poc.go: CRA keyframes get renumbered POCs.
		kw.payload, err = kw.hevcPOC.renumber(poison.Reuse(kw.payload, 0), frame, kw.nalLen)
		if err != nil {
			return nil, nil
		}
		payload = kw.payload
	}

	s := &kw.sample
	*s = KeyframeBatchSample{Payload: payload, Access: kind, Offset: b.DataOffset, Size: int64(len(b.Data))}
	s.PtsMs = b.Timecode
	s.Codec = kw.video.Codec
	if !kw.pack {
		return s, nil
	}
	switch kw.video.Codec {
	case "h264", "hevc":
		kw.data = append(poison.Reuse(kw.data, 0), kw.paramSets...)
		kw.data, err = appendAnnexB(kw.data, payload, kw.nalLen)
		if err != nil {
			return nil, nil
		}
		s.Ext = "." + kw.video.Codec
	default:
		kw.data = appendIVF(poison.Reuse(kw.data, 0), kw.video, b.Timecode, payload)
		s.Ext = ".ivf"
	}
	s.Data = kw.data
	return s, nil
}

// appendAnnexB appends length-prefixed NAL units to dst as Annex-B.
func appendAnnexB(dst, data []byte, lenSize int) ([]byte, error) {
	if lenSize < 1 || lenSize > 4 {
		return dst, fmt.Errorf("bad NAL length size %d", lenSize)
	}
	for pos := 0; pos < len(data); {
		if pos+lenSize > len(data) {
			return dst, fmt.Errorf("truncated NAL length at %d", pos)
		}
		var n int
		for i := 0; i < lenSize; i++ {
			n = n<<8 | int(data[pos+i])
		}
		pos += lenSize
		if n <= 0 || n > len(data)-pos {
			return dst, fmt.Errorf("NAL size %d out of bounds at %d", n, pos)
		}
		dst = append(dst, 0, 0, 0, 1)
		dst = append(dst, data[pos:pos+n]...)
		pos += n
	}
	return dst, nil
}

// appendIVF appends a one-frame IVF file holding frame to dst.
func appendIVF(dst []byte, t *mkv.Track, ptsMs int64, frame []byte) []byte {
	fourcc := map[string]string{"vp8": "VP80", "vp9": "VP90", "av1": "AV01"}[t.Codec]
	var w, h uint16
	if t.Width != nil {
		w = uint16(*t.Width)
	}
	if t.Height != nil {
		h = uint16(*t.Height)
	}
	dst = append(dst, 'D', 'K', 'I', 'F', 0, 0, 32, 0)
	dst = append(dst, fourcc...)
	dst = binary.LittleEndian.AppendUint16(dst, w)
	dst = binary.LittleEndian.AppendUint16(dst, h)
	dst = binary.LittleEndian.AppendUint32(dst, 1000) // timebase denominator (ms)
	dst = binary.LittleEndian.AppendUint32(dst, 1)    // timebase numerator
	dst = binary.LittleEndian.AppendUint32(dst, 1)    // frame count
	dst = append(dst, 0, 0, 0, 0)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(frame)))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(ptsMs))
	return append(dst, frame...)
}

// countingSource counts what a walk asks of its source handle. A seek is
// counted when it moves the offset. Range hints go through to a source that
// takes them.
type countingSource struct {
	mkv.ReadSeekCloser
	bytes, reads, seeks int64
	off                 int64
}

func (c *countingSource) hintRange(off, end int64) {
	if h, ok := c.ReadSeekCloser.(mkv.RangeHinter); ok {
		h.HintRange(off, end)
	}
}

func (c *countingSource) Read(p []byte) (int, error) {
	n, err := c.ReadSeekCloser.Read(p)
	c.reads++
	c.bytes += int64(n)
	c.off += int64(n)
	return n, err
}

func (c *countingSource) Seek(offset int64, whence int) (int64, error) {
	n, err := c.ReadSeekCloser.Seek(offset, whence)
	if err == nil && n != c.off {
		c.seeks++
		c.off = n
	}
	return n, err
}

type byteCount struct{ bytes int64 }

// countingFS wraps fs so the bytes read through the handles it opens are
// counted (the metadata read's share of the I/O).
func countingFS(fs *mkv.FS) (*mkv.FS, *byteCount) {
	bc := &byteCount{}
	wrapped := &mkv.FS{}
	if fs != nil {
		*wrapped = *fs
	}
	wrapped.Open = func(path string) (mkv.ReadSeekCloser, error) {
		f, err := fs.DoOpen(path)
		if err != nil {
			return nil, err
		}
		return &metaCounter{ReadSeekCloser: f, bc: bc}, nil
	}
	if fs != nil && fs.OpenContext != nil {
		wrapped.OpenContext = func(ctx context.Context, path string) (mkv.ReadSeekCloser, error) {
			f, err := fs.OpenContext(ctx, path)
			if err != nil {
				return nil, err
			}
			return &metaCounter{ReadSeekCloser: f, bc: bc}, nil
		}
	}
	return wrapped, bc
}

type metaCounter struct {
	mkv.ReadSeekCloser
	bc *byteCount
}

func (m *metaCounter) Read(p []byte) (int, error) {
	n, err := m.ReadSeekCloser.Read(p)
	m.bc.bytes += int64(n)
	return n, err
}
