package mp4

// forensic.go - single-source forensic A/B watermarking. PlanWatermark
// (watermark.go) needs TWO pre-encoded variants; a ForensicPlan derives the
// second variant from ONE source, with no encoder: variant B of a segment is
// the segment with a single disposable H.264 frame removed at the sample
// level (nal_ref_idc == 0 - a frame no other frame references, so decoding
// stays clean; the viewer sees at most a ~one-frame-duration hold). The
// difference lives in the coded samples, so it survives a remux; it does not
// survive a re-encode (that robustness class needs a perceptual watermark,
// out of scope for a muxer).
//
// The removal is timing-compensated: the dropped sample's duration is added
// to its predecessor, so the segment's total duration - its #EXTINF, and the
// decode timeline of every following segment - is byte-identical to variant
// A's. A segment with no disposable frame (all-intra, or every frame
// referenced) yields no variant: Distinct reports false and the caller
// treats that segment as carrying no watermark bit.

import (
	"context"
	"encoding/binary"
)

// ForensicPlan serves a single-source A/B session-watermarked HLS
// presentation on demand: variant A segments are the plan's ordinary
// segments, variant B segments have one disposable frame dropped. The
// manifest, init segment and segment durations are identical across
// variants, so one plan serves every session; only the per-segment A/B bit
// differs (see WatermarkPlan for the two-encode flavor and for the code
// assignment being the caller's policy).
type ForensicPlan struct {
	p *HLSPlan
}

// PlanForensic plans a single source for session watermarking. The source's
// video must be H.264 (disposable frames are recognized by their NAL
// headers; HEVC has a different non-referenced signalling and is not
// supported yet). Encryption is refused like PlanWatermark - encrypt the
// served bytes at the edge.
func PlanForensic(ctx context.Context, src string, opts ...Options) (*ForensicPlan, error) {
	o := optionsFrom(opts)
	if o.Encrypt != nil || o.CENC != nil {
		return nil, errf("forensic planning does not support encryption in this version (encrypt the served bytes at your edge, or splice before encrypting)")
	}
	p, err := PlanHLS(ctx, src, o)
	if err != nil {
		return nil, err
	}
	codec := ""
	for _, t := range p.tracks {
		if t.ft.outTrack.spec.video {
			codec = t.ft.outTrack.mkv.Codec
			break
		}
	}
	if codec != "h264" {
		return nil, errf("forensic variant generation supports H.264 video only in this version (source video is %q)", codec)
	}
	return &ForensicPlan{p: p}, nil
}

// NumSegments returns the media segment count.
func (f *ForensicPlan) NumSegments() int { return f.p.NumSegments() }

// MasterPlaylist returns the master playlist (shared across variants).
func (f *ForensicPlan) MasterPlaylist() []byte { return f.p.MasterPlaylist() }

// MediaPlaylist returns the media playlist (shared - variant B segments keep
// variant A's durations by construction).
func (f *ForensicPlan) MediaPlaylist() []byte { return f.p.MediaPlaylist() }

// InitSegment returns the (shared) init segment.
func (f *ForensicPlan) InitSegment() []byte { return f.p.InitSegment() }

// SegmentName returns the URI name of the n-th (0-based) segment.
func (f *ForensicPlan) SegmentName(n int) string { return f.p.SegmentName(n) }

// Segment builds the n-th (0-based) media segment for one session: variant B
// (one disposable frame dropped) when fromB is true, variant A (the ordinary
// segment) otherwise. When a segment has no disposable frame, both variants
// are identical - check Distinct to know which segments carry a bit.
func (f *ForensicPlan) Segment(ctx context.Context, n int, fromB bool) ([]byte, error) {
	seg, err := f.p.Segment(ctx, n)
	if err != nil || !fromB {
		return seg, err
	}
	out, _ := DropNonRefSample(seg)
	return out, nil
}

// Distinct reports whether segment n's variants differ - whether the segment
// carries a watermark bit. It costs building the segment once.
func (f *ForensicPlan) Distinct(ctx context.Context, n int) (bool, error) {
	seg, err := f.p.Segment(ctx, n)
	if err != nil {
		return false, err
	}
	_, dropped := DropNonRefSample(seg)
	return dropped, nil
}

// SegmentForPattern is the serve-time entry point, mirroring
// WatermarkPlan.SegmentForPattern: pattern is the session's bit code, one bit
// per segment (LSB-first within each byte); bit n selects B when set.
func (f *ForensicPlan) SegmentForPattern(ctx context.Context, n int, pattern []byte) ([]byte, error) {
	return f.Segment(ctx, n, patternBit(pattern, n))
}

// DropNonRefSample derives the forensic variant of one video media segment
// produced by this package (styp + moof with a single traf + mdat): it
// removes the first disposable H.264 sample - one whose VCL NALs all have
// nal_ref_idc == 0 (nothing references the frame) - and compensates the
// timing by extending the previous sample's duration, so the segment's total
// duration is unchanged. The moof (sizes, sample_count, data_offset, the
// trun entries) and the mdat are rewritten accordingly; the result is a
// valid, decodable fMP4 segment.
//
// Returns (segment, false) untouched when no sample is droppable or the
// segment does not have the expected single-video-track shape (an audio
// rendition segment, an encrypted segment, a foreign file).
func DropNonRefSample(segment []byte) ([]byte, bool) {
	fs := parseForensicSegment(segment)
	if fs == nil {
		return segment, false
	}
	k := fs.pickDisposable(segment)
	if k < 0 {
		return segment, false
	}
	return fs.rebuildWithout(segment, k), true
}

// forensicSegment is the byte geometry of one segment, as needed to remove a
// sample surgically. All offsets are absolute within the segment slice.
type forensicSegment struct {
	moofOff, moofLen int64
	trafOff, trafLen int64
	trunOff, trunLen int64
	entriesOff       int64
	entrySize        int
	dataOffset       int32
	mdatOff, mdatHdr int64
	mdatLen          int64 // whole mdat box, header included
	sizes            []uint32
	durs             []uint32
	sflags           []uint32
}

// parseForensicSegment maps the segment's boxes, requiring the exact shape
// this package's writer produces for a video rendition: one moof with one
// traf and one per-sample trun, one mdat whose payload tiles exactly into
// the trun's sample sizes. Anything else returns nil (not an error: the
// caller simply has no variant for this segment).
func parseForensicSegment(seg []byte) *forensicSegment {
	fs := &forensicSegment{moofOff: -1, mdatOff: -1}
	total := int64(len(seg))

	boxAt := func(off int64) (typ string, hdr, size int64, ok bool) {
		if off+8 > total {
			return "", 0, 0, false
		}
		size = int64(binary.BigEndian.Uint32(seg[off:]))
		typ = string(seg[off+4 : off+8])
		hdr = 8
		if size == 1 {
			if off+16 > total {
				return "", 0, 0, false
			}
			size = int64(binary.BigEndian.Uint64(seg[off+8:]))
			hdr = 16
		}
		if size < hdr || off+size > total {
			return "", 0, 0, false
		}
		return typ, hdr, size, true
	}

	for off := int64(0); off < total; {
		typ, hdr, size, ok := boxAt(off)
		if !ok {
			return nil
		}
		switch typ {
		case "moof":
			if fs.moofOff >= 0 {
				return nil
			}
			fs.moofOff, fs.moofLen = off, size
		case "mdat":
			if fs.mdatOff >= 0 || fs.moofOff < 0 {
				return nil
			}
			fs.mdatOff, fs.mdatHdr, fs.mdatLen = off, hdr, size
		}
		off += size
	}
	if fs.moofOff < 0 || fs.mdatOff < 0 {
		return nil
	}

	// Inside the moof: exactly one traf.
	for off := fs.moofOff + 8; off < fs.moofOff+fs.moofLen; {
		typ, _, size, ok := boxAt(off)
		if !ok {
			return nil
		}
		if typ == "traf" {
			if fs.trafOff >= 0 && fs.trafLen > 0 {
				return nil
			}
			fs.trafOff, fs.trafLen = off, size
		}
		off += size
	}
	if fs.trafLen == 0 {
		return nil
	}

	// Inside the traf: exactly one trun; a senc child means CENC (refuse).
	for off := fs.trafOff + 8; off < fs.trafOff+fs.trafLen; {
		typ, _, size, ok := boxAt(off)
		if !ok {
			return nil
		}
		switch typ {
		case "trun":
			if fs.trunLen > 0 {
				return nil
			}
			fs.trunOff, fs.trunLen = off, size
		case "senc", "saiz", "saio":
			return nil
		}
		off += size
	}
	if fs.trunLen == 0 || fs.trunOff+16 > fs.trafOff+fs.trafLen {
		return nil
	}

	flags := binary.BigEndian.Uint32(seg[fs.trunOff+8:]) & 0x00FFFFFF
	const wanted = trunDataOffset | trunSampleDuration | trunSampleSize | trunSampleFlags
	if flags&wanted != wanted {
		return nil
	}
	fs.entrySize = 12
	if flags&trunSampleCTS != 0 {
		fs.entrySize += 4
	}
	count := int64(binary.BigEndian.Uint32(seg[fs.trunOff+12:]))
	fs.dataOffset = int32(binary.BigEndian.Uint32(seg[fs.trunOff+16:]))
	fs.entriesOff = fs.trunOff + 20
	if fs.entriesOff+count*int64(fs.entrySize) != fs.trunOff+fs.trunLen {
		return nil
	}

	fs.sizes = make([]uint32, count)
	fs.durs = make([]uint32, count)
	fs.sflags = make([]uint32, count)
	var totalData int64
	for i := int64(0); i < count; i++ {
		e := fs.entriesOff + i*int64(fs.entrySize)
		fs.durs[i] = binary.BigEndian.Uint32(seg[e:])
		fs.sizes[i] = binary.BigEndian.Uint32(seg[e+4:])
		fs.sflags[i] = binary.BigEndian.Uint32(seg[e+8:])
		totalData += int64(fs.sizes[i])
	}
	// The single track's samples must tile the mdat payload exactly, and the
	// trun must point at it (moof-relative, default-base-is-moof).
	if totalData != fs.mdatLen-fs.mdatHdr {
		return nil
	}
	if int64(fs.dataOffset) != fs.moofLen+fs.mdatHdr {
		return nil
	}
	return fs
}

// pickDisposable returns the index of the first droppable sample: a non-sync
// sample past the first whose bytes tile exactly into length-prefixed NALs
// (4-byte lengths, the avcC form this package writes) where every VCL NAL is
// a non-referenced non-IDR slice (nal_ref_idc == 0, type 1). Returns -1 when
// none qualifies.
func (fs *forensicSegment) pickDisposable(seg []byte) int {
	payload := seg[fs.mdatOff+fs.mdatHdr : fs.mdatOff+fs.mdatLen]
	var cum int64
	for i := range fs.sizes {
		start := cum
		cum += int64(fs.sizes[i])
		if i == 0 || fs.sflags[i] != sampleFlagsNonSync {
			continue
		}
		if h264AllDisposable(payload[start:cum]) {
			return i
		}
	}
	return -1
}

// h264AllDisposable walks a sample's length-prefixed NALs and reports whether
// it contains at least one VCL NAL and every VCL NAL is a disposable non-IDR
// slice. A sample that does not tile as 4-byte-length-prefixed NALs reports
// false (foreign codec, different NAL length size, or not video at all).
func h264AllDisposable(sample []byte) bool {
	vcl := 0
	for off := 0; off < len(sample); {
		if len(sample)-off < 5 {
			return false
		}
		n := int(binary.BigEndian.Uint32(sample[off:]))
		if n <= 0 || off+4+n > len(sample) {
			return false
		}
		h := sample[off+4]
		if h&0x80 != 0 { // forbidden_zero_bit
			return false
		}
		refIdc := (h >> 5) & 0x3
		typ := h & 0x1F
		if typ >= 1 && typ <= 5 { // VCL NAL
			if typ != 1 || refIdc != 0 {
				return false
			}
			vcl++
		}
		off += 4 + n
	}
	return vcl > 0
}

// rebuildWithout reassembles the segment with sample k removed: the trun
// loses its entry (sample_count, box sizes and data_offset adjusted), the
// previous sample absorbs the dropped duration, and the mdat loses the
// sample's bytes.
func (fs *forensicSegment) rebuildWithout(seg []byte, k int) []byte {
	es := int64(fs.entrySize)
	dropBytes := int64(fs.sizes[k])
	out := make([]byte, 0, int64(len(seg))-es-dropBytes)

	// Everything before the moof (styp) is unchanged.
	out = append(out, seg[:fs.moofOff]...)

	// The moof, minus entry k, with its sizes and offsets patched.
	moof := append([]byte(nil), seg[fs.moofOff:fs.moofOff+fs.moofLen]...)
	rel := func(abs int64) int64 { return abs - fs.moofOff }
	patch := func(abs int64, v uint32) { binary.BigEndian.PutUint32(moof[rel(abs):], v) }
	patch(fs.moofOff, uint32(fs.moofLen-es))
	patch(fs.trafOff, uint32(fs.trafLen-es))
	patch(fs.trunOff, uint32(fs.trunLen-es))
	patch(fs.trunOff+12, uint32(len(fs.sizes)-1))
	patch(fs.trunOff+16, uint32(fs.dataOffset-int32(es)))
	// Timing compensation: the predecessor holds through the dropped frame.
	patch(fs.entriesOff+int64(k-1)*es, fs.durs[k-1]+fs.durs[k])
	entRel := rel(fs.entriesOff + int64(k)*es)
	out = append(out, moof[:entRel]...)
	out = append(out, moof[entRel+es:]...)

	// The mdat header with its size reduced, in whichever form it uses.
	mdatHdr := append([]byte(nil), seg[fs.mdatOff:fs.mdatOff+fs.mdatHdr]...)
	if fs.mdatHdr == 16 {
		binary.BigEndian.PutUint64(mdatHdr[8:], uint64(fs.mdatLen-dropBytes))
	} else {
		binary.BigEndian.PutUint32(mdatHdr, uint32(fs.mdatLen-dropBytes))
	}
	out = append(out, mdatHdr...)

	// The payload minus sample k's bytes.
	payload := seg[fs.mdatOff+fs.mdatHdr : fs.mdatOff+fs.mdatLen]
	var start int64
	for i := 0; i < k; i++ {
		start += int64(fs.sizes[i])
	}
	out = append(out, payload[:start]...)
	out = append(out, payload[start+dropBytes:]...)

	// Anything after the mdat (nothing in this package's segments) verbatim.
	out = append(out, seg[fs.mdatOff+fs.mdatLen:]...)
	return out
}
