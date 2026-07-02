package reader

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ReadOption configures an optional behaviour of ReadMeta / OpenMeta.
type ReadOption func(*readOpts)

type readOpts struct {
	inBandColour     bool
	sampledKeyframes int  // 0 = off; >0 = number of Cluster timestamps to sample
	keyframeIndex    bool // build the COMPLETE keyframe index (sequential pass)
	bitrate          bool // follow the SeekHead to Tags for per-track BPS → Track.Bitrate
	cues             bool // keep the raw CuePoints on the metadata path (WithCues)
	tags             bool // keep the Tags element on the metadata path (WithTags)
}

// WithTags keeps Container.Tags populated on the metadata-only path (normally
// left nil). The Tags element is reached through its SeekHead entry — one seek
// to one element, no Cluster scan — so the read stays head-only.
func WithTags() ReadOption {
	return func(o *readOpts) { o.tags = true }
}

// WithCues keeps Container.Cues populated on the metadata-only path (they are
// normally consumed into Keyframes and dropped). Each CuePoint's ClusterPos is
// relative to Container.SegmentStart — together they let a caller seek straight
// to the cluster holding a given time, which is what cue-driven readers (e.g.
// on-demand HLS segmenting) need. The read stays head-only.
func WithCues() ReadOption {
	return func(o *readOpts) { o.cues = true }
}

// WithBitrate fills each track's Bitrate from the Matroska "BPS" tag (the per-track
// bitrate ffmpeg/mkvmerge write, which ffprobe shows as TAG:BPS — ffprobe leaves its
// own bit_rate field N/A for Matroska, so this gives more than ffprobe) on the
// metadata-only path. The default OpenMeta/ReadMeta stops before Tags, so
// Track.Bitrate is nil for Matroska; this option follows the head SeekHead straight
// to the Tags element (one seek, no Cluster scan — the muxer references Tags from the
// SeekHead near the head) and reads only it. The raw Tags stay nil, exactly the
// metadata contract; a full Read sets Bitrate regardless. No effect on MP4, whose
// Bitrate comes from btrt/esds and does equal ffprobe's bit_rate.
func WithBitrate() ReadOption {
	return func(o *readOpts) { o.bitrate = true }
}

// WithKeyframeIndex builds a COMPLETE video keyframe index for a Matroska that
// carries no Cues — every keyframe, equal to `ffprobe -skip_frame nokey`, not a
// sample. AFTER the head parse, and only when no Cues were found, it makes a
// single sequential structural pass over the Segment (cluster by cluster, no
// per-block seek), reading element headers and skipping block payloads by size —
// no demux, no decode. Use it for the "no external fallback" path; use the
// cheaper WithSampledKeyframes when a coarse index suffices. Files with Cues are
// never scanned (the head-only Cues index is used).
func WithKeyframeIndex() ReadOption {
	return func(o *readOpts) { o.keyframeIndex = true }
}

// defaultKeyframeSamples is the sample count WithSampledKeyframes uses for a
// non-positive n: enough Cluster timestamps for a usable seek index on a
// feature-length file without an unreasonable number of seeks.
const defaultKeyframeSamples = 200

// WithSampledKeyframes enables a bounded, coarse keyframe index for a Matroska
// that carries no Cues (so the head-only keyframe index would be empty). AFTER
// the head parse, and only when no Cues were found, it probes n evenly-spaced
// byte offsets in the Segment body, resyncing to the next Cluster at each and
// reading that Cluster's Timestamp — every Cluster start being a real seek point.
// n ≤ 0 uses defaultKeyframeSamples. The result is coarse-but-valid (one keyframe
// per sampled interval, deduplicated), bounded to about n seeks; it spares the
// caller an external ffprobe fallback. Files that already carry Cues never sample.
func WithSampledKeyframes(n int) ReadOption {
	if n <= 0 {
		n = defaultKeyframeSamples
	}
	return func(o *readOpts) { o.sampledKeyframes = n }
}

// WithInBandColourFallback enables a bounded colour fallback. By default a read
// is head-only and never touches a cluster. With this option, AFTER the head
// parse, any video track whose colour is absent from BOTH the container and the
// codec-private SPS — a bare hvcC with no NAL arrays, as streaming-style muxes
// write when they keep the parameter sets in-band — triggers a read of the first
// video sample to recover the SPS and parse its colour VUI.
//
// The cost is paid only for those tracks (≈the HDR files a header-only probe
// would otherwise report as "no colour"); files that already carry colour in the
// header never read a frame. The read is bounded to the first sample per track.
func WithInBandColourFallback() ReadOption {
	return func(o *readOpts) { o.inBandColour = true }
}

// maxInBandBlocks bounds the in-band scan: the parameter sets sit in the first
// access unit of each video track, so the first block is enough; this only caps
// how far we look if a needed track's first block is unexpectedly late.
const maxInBandBlocks = 128

// fillColourFromFirstSample is the WithInBandColourFallback worker. For every
// video track that still lacks colour and carries a bare hvcC (no in-record SPS),
// it reads that track's first sample, extracts the SPS NAL and parses its VUI.
// Best-effort: any failure (no such track, unreadable cluster, no SPS in the
// frame) silently leaves the colour unset, exactly as before the option.
func fillColourFromFirstSample(ctx context.Context, r io.ReadSeeker, c *mkv.Container) {
	need := make(map[uint64]*mkv.Track)
	for i := range c.Tracks {
		if t := &c.Tracks[i]; needsInBandColour(t) {
			need[t.ID] = t
		}
	}
	if len(need) == 0 {
		return
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return
	}
	br, err := NewBlockReader(r, c.Info.TimecodeScale)
	if err != nil {
		return
	}
	for i := 0; i < maxInBandBlocks && len(need) > 0; i++ {
		if ctx.Err() != nil {
			return
		}
		blk, err := br.Next()
		if err != nil {
			return
		}
		t, ok := need[blk.TrackNumber]
		if !ok {
			continue
		}
		delete(need, blk.TrackNumber) // first sample carries the parameter sets
		applyInBandSPSColour(t, blk.Data)
	}
}

// NeedsInBandColour reports whether t is an HEVC video track whose colour can
// only come from an in-band SPS (no container/codec-private colour, bare hvcC).
// Exposed so the mp4 package can drive the same fallback off its sample table.
func NeedsInBandColour(t *mkv.Track) bool { return needsInBandColour(t) }

// ApplyInBandColour fills t's colour from one length-prefixed HEVC access unit
// (the first sample): the SPS VUI and an Alternative Transfer Characteristics
// SEI override if present. Safe on any input; leaves colour unset on failure.
func ApplyInBandColour(t *mkv.Track, frame []byte) { applyInBandSPSColour(t, frame) }

// needsInBandColour reports whether t is a video track whose colour can only come
// from an in-band SPS: no container/SPS colour yet, HEVC, and a hvcC that holds
// no NAL arrays (numOfArrays == 0, byte 22 of the configuration record).
func needsInBandColour(t *mkv.Track) bool {
	if t.Type != mkv.VideoTrack {
		return false
	}
	if t.ColorTransfer != nil || t.ColorPrimaries != nil || t.ColorSpace != nil {
		return false
	}
	if !isHEVCCodec(t.Codec) {
		return false
	}
	cp := t.CodecPrivate
	return len(cp) >= 23 && cp[0] == 1 && cp[22] == 0
}

func isHEVCCodec(codec string) bool {
	switch codec {
	case "hevc", "h265", "V_MPEGH/ISO/HEVC":
		return true
	default:
		return false
	}
}

// applyInBandSPSColour reads a length-prefixed HEVC access unit (prefix width
// from the hvcC's lengthSizeMinusOne) and fills the track's colour from it: the
// SPS VUI via the existing SPS parser, then an Alternative Transfer
// Characteristics SEI override if present.
func applyInBandSPSColour(t *mkv.Track, frame []byte) {
	nalLen := 4
	if len(t.CodecPrivate) >= 22 {
		nalLen = int(t.CodecPrivate[21]&0x03) + 1
	}
	if sps := firstHEVCNAL(frame, nalLen, 33); len(sps) >= 3 { // 33 = SPS_NUT
		bc := &bitstreamColour{}
		parseHEVCSPS(unescapeRBSP(sps[2:]), bc) // strip the 2-byte NAL header
		mergeBitstreamColour(t, bc)
	}
	// The Alternative Transfer Characteristics SEI (payload type 147) is HLG's
	// standard compatibility signal: the SPS VUI advertises bt2020-10 (14) for
	// legacy decoders while the real arib-std-b67 (18) travels in this SEI. When
	// present it is authoritative for the transfer, exactly as ffmpeg treats it.
	if tc, ok := atcTransferFromFrame(frame, nalLen); ok {
		t.ColorTransfer = &tc
	}
}

// forEachHEVCNAL walks the length-prefixed NAL units of a frame (prefix nalLen
// bytes, big-endian), calling fn(nal_unit_type, nal) for each; fn returns true to
// stop. Malformed lengths end the walk.
func forEachHEVCNAL(frame []byte, nalLen int, fn func(nalType int, nal []byte) bool) {
	if nalLen < 1 || nalLen > 4 {
		return
	}
	for off := 0; off+nalLen <= len(frame); {
		n := 0
		for i := 0; i < nalLen; i++ {
			n = n<<8 | int(frame[off+i])
		}
		off += nalLen
		if n < 2 || off+n > len(frame) {
			return
		}
		nal := frame[off : off+n]
		off += n
		if fn(int((nal[0]>>1)&0x3f), nal) {
			return
		}
	}
}

// firstHEVCNAL returns the first NAL of nal_unit_type want, with its 2-byte
// header intact, or nil.
func firstHEVCNAL(frame []byte, nalLen, want int) []byte {
	var found []byte
	forEachHEVCNAL(frame, nalLen, func(nt int, nal []byte) bool {
		if nt == want {
			found = nal
			return true
		}
		return false
	})
	return found
}

// atcTransferFromFrame returns the preferred_transfer_characteristics from an
// Alternative Transfer Characteristics SEI in any SEI NAL (prefix 39 / suffix 40)
// of the frame, or ok=false if none is present.
func atcTransferFromFrame(frame []byte, nalLen int) (transfer uint16, ok bool) {
	forEachHEVCNAL(frame, nalLen, func(nt int, nal []byte) bool {
		if nt == 39 || nt == 40 { // PREFIX_SEI_NUT / SUFFIX_SEI_NUT
			if tc, found := atcFromSEI(unescapeRBSP(nal[2:])); found {
				transfer, ok = tc, true
				return true
			}
		}
		return false
	})
	return transfer, ok
}

// atcFromSEI scans an SEI RBSP's messages for payload type 147 (Alternative
// Transfer Characteristics) and returns its single byte,
// preferred_transfer_characteristics.
func atcFromSEI(rbsp []byte) (uint16, bool) {
	for i := 0; i+1 < len(rbsp); { // need at least a type and a size byte
		pt, ok := readSEIValue(rbsp, &i)
		if !ok {
			return 0, false
		}
		ps, ok := readSEIValue(rbsp, &i)
		if !ok || i+ps > len(rbsp) {
			return 0, false
		}
		if pt == 147 && ps >= 1 {
			return uint16(rbsp[i]), true
		}
		i += ps
	}
	return 0, false
}

// readSEIValue reads an SEI ff-coded value (sum of 0xFF bytes plus the final
// byte), advancing *i. ok is false on truncation.
func readSEIValue(b []byte, i *int) (int, bool) {
	v := 0
	for *i < len(b) && b[*i] == 0xff {
		v += 255
		*i++
	}
	if *i >= len(b) {
		return 0, false
	}
	v += int(b[*i])
	*i++
	return v, true
}
