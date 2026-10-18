package mp4

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// parseSampleEntry describes the first sample entry of an stsd on the track:
// a protected one through its original format, a PCM one by its layout, any
// other by its fourcc, tr.unsupported naming what a remux cannot carry. It
// reports false for a still picture (cover art), which the caller surfaces as
// a dropped track instead. An error is a malformed stsd.
func parseSampleEntry(tr *inTrack, stsdPayload []byte) (described bool, fourcc string, err error) {
	if len(stsdPayload) < 8 {
		return false, "", errf("stsd too short")
	}
	// fullbox header(4) + entry_count(4), then the sample entry box.
	entries, err := iterBoxes(stsdPayload[8:])
	if err != nil {
		return false, "", errf("parse stsd: %w", err)
	}
	if len(entries) == 0 {
		return false, "", errf("stsd has no sample entry")
	}
	return parseEntry(tr, entries[0].typ, entries[0].payload)
}

// parseEntry is parseSampleEntry on one entry; a protected entry recurses on its original format.
func parseEntry(tr *inTrack, typ string, payload []byte) (bool, string, error) {
	const visualHdr = 78
	audioHdr := audioExtOffset(payload)
	tr.codec = typ
	var err error
	switch typ {
	case "encv", "enca", "encs", "enct":
		original, scheme := protectedEntry(payload, visualHdr, audioHdr, 8, 16)
		tr.encryption = scheme
		if original == "" || original == typ {
			describeEntry(tr, payload)
			tr.unsupported = "protected sample entry " + quoteFourcc(typ) + " without an original format (sinf/frma)"
			return true, typ, nil
		}
		_, _, err := parseEntry(tr, original, payload)
		return true, typ, err
	case "jpeg", "png ", "gif ", "tiff", "jpg ":
		return false, typ, nil
	case "avc1", "avc3", "dvav", "dva1":
		// dvav/dva1 are Dolby Vision over AVC: an avcC plus a dvcC/dvvC box.
		tr.codec = "h264"
		err = extractVisual(tr, payload, visualHdr, "avcC")
	case "hvc1", "hev1", "dvhe", "dvh1":
		// dvhe/dvh1 are Dolby Vision over HEVC: an hvcC plus a dvcC/dvvC box.
		tr.codec = "hevc"
		err = extractVisual(tr, payload, visualHdr, "hvcC")
	case "av01", "dav1":
		// dav1 is Dolby Vision over AV1: an av1C plus a dvvC box.
		tr.codec = "av1"
		err = extractVisual(tr, payload, visualHdr, "av1C")
	case "vp09":
		// The vpcC (FullBox form) becomes the Matroska CodecPrivate, so the
		// colour/bit-depth it carries survives into MKV (the reader parses both
		// forms).
		tr.codec = "vp9"
		err = extractVisual(tr, payload, visualHdr, "vpcC")
	case "mp4a":
		var ok bool
		ok, err = parseMP4A(tr, payload, audioHdr)
		if err == nil && !ok {
			tr.unsupported = "mp4a entry of an audio object type mkvgo has no Matroska form for"
		}
	case "Opus":
		tr.codec = "opus"
		err = extractOpus(tr, payload, audioHdr)
	case "ac-3":
		tr.codec = "ac3"
		parseAudioFields(tr, payload)
		if dac3, err := childConfig(payload, audioHdr, "dac3"); err == nil {
			if ch := ac3Channels(dac3); ch > 0 {
				tr.channels = ch
			}
		}
	case "ec-3":
		tr.codec = "eac3"
		parseAudioFields(tr, payload)
		if dec3, err := childConfig(payload, audioHdr, "dec3"); err == nil {
			if ch := eac3Channels(dec3); ch > 0 {
				tr.channels = ch
			}
		}
	case "fLaC":
		tr.codec = "flac"
		err = extractFLAC(tr, payload, audioHdr)
	case "tx3g":
		tr.codec = "srt" // tx3g timed text → S_TEXT/UTF8
	case "wvtt":
		tr.codec = "webvtt" // native WebVTT → S_TEXT/WEBVTT
		extractWVTTConfig(tr, payload)
	case "twos", "sowt", "raw ", "in24", "in32", "fl32", "fl64", "ipcm", "fpcm", "lpcm":
		parsePCM(tr, typ, payload, audioHdr)
	case "apco", "apcs", "apcn", "apch", "ap4h", "ap4x":
		// ProRes: the fourcc states the profile's bit depth (10 up to HQ, 12 for 4444); ProRes RAW states none.
		describeEntry(tr, payload)
		tr.videoBitDepth = proResBitDepth(typ)
		tr.unsupported = "sample entry " + quoteFourcc(typ) + " has no Matroska form in mkvgo"
	case "samr", "sawb", "sawp":
		// AMR is mono by definition; the 3GPP AMRSampleEntry's channelcount is a reserved 2.
		describeEntry(tr, payload)
		tr.channels = 1
		tr.unsupported = "sample entry " + quoteFourcc(typ) + " has no Matroska form in mkvgo"
	case "mhm1", "mha1", "mhm2", "mha2":
		// MPEG-H: the entry's channelcount is 0; the mhaC's reference layout (CICP) gives the count.
		describeEntry(tr, payload)
		if mhaC, err := childConfig(payload, audioHdr, "mhaC"); err == nil && len(mhaC) >= 3 {
			if ch := cicpChannels(mhaC[2]); ch > 0 {
				tr.channels = ch
			}
		}
		tr.unsupported = "sample entry " + quoteFourcc(typ) + " has no Matroska form in mkvgo"
	default:
		describeEntry(tr, payload)
		tr.unsupported = "sample entry " + quoteFourcc(typ) + " has no Matroska form in mkvgo"
	}
	if err != nil {
		// The entry's configuration is not what its codec needs: described as it stands, not carried.
		describeEntry(tr, payload)
		tr.codec, tr.codecPrivate, tr.unsupported = typ, nil, err.Error()
	}
	if tr.trackType == mkv.VideoTrack && tr.unsupported == "" && (tr.width == 0 || tr.height == 0) {
		// Matroska PixelWidth/PixelHeight are mandatory and not 0: the SPS states the size, or the track is not carried.
		if w, h, ok := reader.PictureSizeFromCodecPrivate(tr.codec, tr.codecPrivate); ok {
			tr.width, tr.height = w, h
		} else {
			tr.unsupported = "video entry " + quoteFourcc(typ) + " states a " + strconv.Itoa(int(tr.width)) + "x" + strconv.Itoa(int(tr.height)) + " picture and its codec configuration no size"
		}
	}
	return true, typ, nil
}

// parseMP4A resolves an mp4a entry to either AAC or MP3 by reading the esds
// object type indication.
func parseMP4A(tr *inTrack, payload []byte, headerLen int) (bool, error) {
	parseAudioFields(tr, payload)
	esds, err := childConfig(payload, headerLen, "esds")
	if err != nil {
		return false, err
	}
	objType, asc, avgBitrate, err := parseESDS(esds)
	if err != nil {
		return false, err
	}
	if tr.bitrate == 0 { // a btrt box, when present, takes precedence
		tr.bitrate = avgBitrate
	}
	switch objType {
	case 0x40, 0x66, 0x67: // MPEG-4/2 AAC
		if len(asc) == 0 {
			return false, errf("mp4a/AAC without AudioSpecificConfig")
		}
		tr.codec = "aac"
		tr.codecPrivate = asc
		// The AudioSampleEntry channelcount and sample rate are unreliable for AAC:
		// multichannel layouts and HE-AAC SBR/PS live in the AudioSpecificConfig.
		cfg := parseAACConfig(asc)
		if cfg.channels > 0 {
			tr.channels = cfg.channels
		}
		if cfg.sampleRate > 0 {
			tr.sampleRate = cfg.sampleRate
		}
		tr.outputSampleRate = cfg.outputRate // SBR-doubled rate, 0 when not SBR
		return true, nil
	case 0x69, 0x6B: // MPEG-2/1 Audio Layer III
		tr.codec = "A_MPEG/L3"
		return true, nil
	case 0xA9, 0xAA, 0xAB, 0xAC: // DTS Coherent Acoustics / DTS-HD / DTS Express
		tr.codec = "dts"
		return true, nil
	default:
		return false, nil // unsupported audio object type - skip the track
	}
}

// extractFLAC rebuilds the MKV FLAC CodecPrivate ("fLaC" marker + metadata
// blocks) from a dfLa box (which holds the metadata blocks only).
func extractFLAC(tr *inTrack, payload []byte, headerLen int) error {
	parseAudioFields(tr, payload)
	dfla, err := childConfig(payload, headerLen, "dfLa")
	if err != nil {
		return err
	}
	if len(dfla) < 4 {
		return errf("dfLa too short")
	}
	meta := dfla[4:] // strip the fullbox version/flags
	cp := make([]byte, 0, 4+len(meta))
	cp = append(cp, "fLaC"...)
	cp = append(cp, meta...)
	tr.codecPrivate = cp
	return nil
}

// childConfig returns the payload of the named configuration box nested in a
// sample entry whose fixed header is headerLen bytes.
func childConfig(entryPayload []byte, headerLen int, configType string) ([]byte, error) {
	if len(entryPayload) < headerLen {
		return nil, errf("sample entry shorter than its %d-byte header", headerLen)
	}
	children, err := iterBoxes(entryPayload[headerLen:])
	if err != nil {
		return nil, err
	}
	cfg, ok := findMemBox(children, configType)
	if !ok {
		// QuickTime wraps an audio entry's config in a 'wave' extension
		// (siDecompressionParam: frma + the config + a terminator atom).
		if wave, wok := findMemBox(children, "wave"); wok {
			if sub, serr := iterBoxes(wave.payload); serr == nil {
				cfg, ok = findMemBox(sub, configType)
			}
		}
	}
	if !ok {
		return nil, errf("sample entry missing %s configuration box", configType)
	}
	return cfg.payload, nil
}

func extractVisual(tr *inTrack, payload []byte, headerLen int, configType string) error {
	if len(payload) >= 32 {
		tr.width = uint32(binary.BigEndian.Uint16(payload[24:26]))
		tr.height = uint32(binary.BigEndian.Uint16(payload[26:28]))
	}
	cfg, err := childConfig(payload, headerLen, configType)
	if err != nil {
		return err
	}
	tr.codecPrivate = append([]byte(nil), cfg...)
	parseColr(tr, payload, headerLen)
	parseHDRStatic(tr, payload, headerLen)
	parseSpatial(tr, payload, headerLen)
	parseDolbyVision(tr, payload, headerLen)
	parsePasp(tr, payload, headerLen)
	parseBitrate(tr, payload, headerLen)
	return nil
}

// parseBitrate reads a btrt box (BitRateBox: bufferSizeDB, maxBitrate, avgBitrate)
// from a sample entry and records the average bitrate - what probers report as
// bit_rate. It falls back to maxBitrate when the average is zero (some muxers
// only fill the max).
func parseBitrate(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	btrt, ok := findMemBox(children, "btrt")
	if !ok || len(btrt.payload) < 12 {
		return
	}
	avg := binary.BigEndian.Uint32(btrt.payload[8:12])
	if avg == 0 {
		avg = binary.BigEndian.Uint32(btrt.payload[4:8]) // maxBitrate fallback
	}
	if avg > 0 {
		tr.bitrate = avg
	}
}

// parsePasp reads a pasp box (PixelAspectRatio: hSpacing:vSpacing) from a visual
// sample entry and, when the pixels are non-square, records the display aspect as
// DisplayWidth:DisplayHeight = (codedWidth·hSpacing):(codedHeight·vSpacing),
// reduced. The ratio is stored exactly rather than as rounded display pixels:
// rounding would collapse a fine ratio (e.g. a 426:425 pasp) to square and yield
// the wrong sample/display aspect. Square pixels (1:1) leave the fields unset.
func parsePasp(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen || tr.width == 0 || tr.height == 0 {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	pasp, ok := findMemBox(children, "pasp")
	if !ok || len(pasp.payload) < 8 {
		return
	}
	h := binary.BigEndian.Uint32(pasp.payload[0:4])
	v := binary.BigEndian.Uint32(pasp.payload[4:8])
	if h == 0 || v == 0 || h == v {
		return // missing or square pixels
	}
	dw := uint64(tr.width) * uint64(h)
	dh := uint64(tr.height) * uint64(v)
	g := gcdU64(dw, dh)
	tr.displayWidth = uint32(dw / g)
	tr.displayHeight = uint32(dh / g)
}

// parseHDRStatic reads the HDR10 static-metadata boxes from a visual sample entry:
// clli (Content Light Level - MaxCLL/MaxFALL in cd/m²) and mdcv (Mastering Display
// Colour Volume, SMPTE ST 2086). mdcv stores its primaries in G,B,R order with
// chromaticities in units of 0.00002 and luminances in 0.0001 cd/m²; those are
// converted to the units MasteringDisplay holds. Absent boxes leave tr.hdr nil.
func parseHDRStatic(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	if clli, ok := findMemBox(children, "clli"); ok && len(clli.payload) >= 4 {
		h := ensureMP4HDR(tr)
		h.MaxCLL = uint32(binary.BigEndian.Uint16(clli.payload[0:2]))
		h.MaxFALL = uint32(binary.BigEndian.Uint16(clli.payload[2:4]))
	}
	if mdcv, ok := findMemBox(children, "mdcv"); ok && len(mdcv.payload) >= 24 {
		b := mdcv.payload
		chroma := func(off int) float64 { return float64(binary.BigEndian.Uint16(b[off:off+2])) / 50000 }
		lum := func(off int) float64 { return float64(binary.BigEndian.Uint32(b[off:off+4])) / 10000 }
		ensureMP4HDR(tr).MasteringDisplay = &mkv.MasteringDisplay{
			GreenX: chroma(0), GreenY: chroma(2), // mdcv primaries are G, B, R
			BlueX: chroma(4), BlueY: chroma(6),
			RedX: chroma(8), RedY: chroma(10),
			WhiteX: chroma(12), WhiteY: chroma(14),
			LuminanceMax: lum(16), LuminanceMin: lum(20),
		}
	}
}

// parseSpatial reads the spatial-media boxes from a visual sample entry: st3d
// (stereoscopic 3D, mapped to the Matroska StereoMode the model stores) and sv3d
// (spherical/360 projection). Absent boxes leave the fields unset.
func parseSpatial(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	if st3d, ok := findMemBox(children, "st3d"); ok && len(st3d.payload) >= 5 {
		if m := mp4StereoToMatroska(st3d.payload[4]); m != 0 { // [4] = stereo_mode, after version+flags
			sm := m
			tr.stereoMode = &sm
		}
	}
	if sv3d, ok := findMemBox(children, "sv3d"); ok {
		tr.projection = sv3dProjection(sv3d.payload)
	}
}

// mp4StereoToMatroska maps an st3d stereo_mode (0 mono, 1 top-bottom, 2 left-right)
// to the equivalent Matroska StereoMode value; 0 when mono or custom/unknown.
func mp4StereoToMatroska(mode byte) uint16 {
	switch mode {
	case 1:
		return 3 // top-bottom (left eye first)
	case 2:
		return 1 // side by side (left eye first)
	}
	return 0
}

// sv3dProjection returns the projection name from an sv3d box by the proj sub-box
// it carries (equi/cbmp/mesh); "" when none is recognised.
func sv3dProjection(payload []byte) string {
	children, err := iterBoxes(payload)
	if err != nil {
		return ""
	}
	proj, ok := findMemBox(children, "proj")
	if !ok {
		return ""
	}
	pb, err := iterBoxes(proj.payload)
	if err != nil {
		return ""
	}
	switch {
	case hasMemBox(pb, "equi"):
		return "equirectangular"
	case hasMemBox(pb, "cbmp"):
		return "cubemap"
	case hasMemBox(pb, "mesh"):
		return "mesh"
	}
	return ""
}

func ensureMP4HDR(tr *inTrack) *mkv.HDRStaticMetadata {
	if tr.hdr == nil {
		tr.hdr = &mkv.HDRStaticMetadata{}
	}
	return tr.hdr
}

// parseDolbyVision reads a dvcC or dvvC box from a visual sample entry and records
// the decoded Dolby Vision configuration. Both box types carry the same record;
// dvcC is used for profiles without a cross-compatibility id, dvvC for the rest.
func parseDolbyVision(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	for _, typ := range [...]string{"dvvC", "dvcC"} {
		if b, ok := findMemBox(children, typ); ok {
			if dv := mkv.ParseDolbyVisionConfig(b.payload); dv != nil {
				tr.dolbyVision = dv
				return
			}
		}
	}
}

// parseColr reads a colr box from a visual sample entry and records the colour
// code points. Two on-screen colour types are read: 'nclx' (CICP primaries /
// transfer / matrix plus a full-range flag) and 'nclc' (the QuickTime form  -
// identical but without the range byte). ICC-profile types ('rICC', 'prof') are
// ignored. Each CICP field is taken independently, so an SDR stream that
// specifies only the matrix (e.g. BT.709) while leaving primaries/transfer
// unspecified still reports its colour_space, matching mainstream probers.
func parseColr(tr *inTrack, payload []byte, headerLen int) {
	if len(payload) < headerLen {
		return
	}
	children, err := iterBoxes(payload[headerLen:])
	if err != nil {
		return
	}
	colr, ok := findMemBox(children, "colr")
	if !ok || len(colr.payload) < 10 {
		return
	}
	typ := string(colr.payload[:4])
	if typ != "nclx" && typ != "nclc" {
		return
	}
	tr.colourDetermined = true // an on-screen colr box is present
	p := binary.BigEndian.Uint16(colr.payload[4:6])
	tr.colorPrimaries = &p
	tc := binary.BigEndian.Uint16(colr.payload[6:8])
	tr.colorTransfer = &tc
	m := binary.BigEndian.Uint16(colr.payload[8:10])
	tr.colorMatrix = &m
	// Only 'nclx' carries the full-range flag. For 'nclc' the range is left unset
	// so the codec-bitstream fallback (SPS VUI) can supply it, as mainstream demuxers do.
	if typ == "nclx" && len(colr.payload) >= 11 {
		rng := uint16(1) // limited
		if colr.payload[10]&0x80 != 0 {
			rng = 2 // full
		}
		tr.colorRange = &rng
	}
}

func extractOpus(tr *inTrack, payload []byte, headerLen int) error {
	parseAudioFields(tr, payload)
	dops, err := childConfig(payload, headerLen, "dOps")
	if err != nil {
		return err
	}
	head, err := opusHeadFromDOps(dops)
	if err != nil {
		return err
	}
	tr.codecPrivate = head
	return nil
}

// audioExtOffset returns the offset of the first extension box inside an audio
// sample entry payload. ISO and QuickTime version 0 entries have a 28-byte
// fixed part; a QuickTime SoundDescription version 1 adds 16 bytes of
// per-packet compression fields, and version 2 is a 64-byte struct with its
// own float64 sample rate. The version field sits at payload[8:10] - reserved
// (zero) in ISO files, so version 0 keeps the ISO layout.
func audioExtOffset(payload []byte) int {
	if len(payload) < 10 {
		return 28
	}
	switch binary.BigEndian.Uint16(payload[8:10]) {
	case 1:
		return 44
	case 2:
		return 64
	}
	return 28
}

func parseAudioFields(tr *inTrack, payload []byte) {
	ext := audioExtOffset(payload)
	if ext == 64 {
		// QuickTime SoundDescription v2: float64 sample rate at 32, 32-bit
		// channel count at 40 (the v0 fields hold placeholder constants).
		if len(payload) >= 64 {
			tr.sampleRate = math.Float64frombits(binary.BigEndian.Uint64(payload[32:40]))
			tr.channels = uint8(binary.BigEndian.Uint32(payload[40:44]))
			parseBitrate(tr, payload, ext)
		}
		return
	}
	// AudioSampleEntry: reserved(8) channels(2) samplesize(2) pre(2) res(2) rate(4 fixed16.16)
	if len(payload) >= 28 {
		tr.channels = uint8(binary.BigEndian.Uint16(payload[16:18]))
		tr.sampleRate = float64(binary.BigEndian.Uint32(payload[24:28]) >> 16)
	}
	if len(payload) >= ext {
		parseBitrate(tr, payload, ext) // a btrt box may sit among the entry's children
	}
}

// parseESDS walks an esds box payload's MPEG-4 descriptor tree and returns the
// object type indication, the DecoderSpecificInfo (the AudioSpecificConfig for
// AAC; absent for MP3, in which case asc is nil) and the average bitrate from the
// DecoderConfigDescriptor (0 when unset).
func parseESDS(esds []byte) (objType byte, asc []byte, avgBitrate uint32, err error) {
	if len(esds) < 4 {
		return 0, nil, 0, errf("esds too short")
	}
	d := &descReader{buf: esds[4:]} // skip fullbox version/flags
	tag, body, err := d.next()
	if err != nil || tag != 0x03 {
		return 0, nil, 0, errf("esds: expected ES_Descriptor")
	}
	// ES_Descriptor: ES_ID(2) + flags(1) + optional fields.
	es := &descReader{buf: body}
	if err := es.skip(3); err != nil {
		return 0, nil, 0, err
	}
	flags := body[2]
	if flags&0x80 != 0 { // dependsOn_ES_ID
		if err := es.skip(2); err != nil {
			return 0, nil, 0, err
		}
	}
	if flags&0x40 != 0 { // URL
		if es.pos >= len(es.buf) {
			return 0, nil, 0, errf("esds: truncated URL length")
		}
		urlLen := int(es.buf[es.pos])
		if err := es.skip(1 + urlLen); err != nil {
			return 0, nil, 0, err
		}
	}
	if flags&0x20 != 0 { // OCR
		if err := es.skip(2); err != nil {
			return 0, nil, 0, err
		}
	}
	tag, dcfg, err := es.next()
	if err != nil || tag != 0x04 {
		return 0, nil, 0, errf("esds: expected DecoderConfigDescriptor")
	}
	if len(dcfg) < 1 {
		return 0, nil, 0, errf("esds: empty DecoderConfigDescriptor")
	}
	objType = dcfg[0]
	// DecoderConfigDescriptor header: objectType(1) streamType(1) buffer(3)
	// max(4) avg(4) = 13 bytes, optionally followed by a DecoderSpecificInfo.
	if len(dcfg) >= 13 {
		avgBitrate = binary.BigEndian.Uint32(dcfg[9:13])
	}
	dc := &descReader{buf: dcfg}
	if err := dc.skip(13); err != nil {
		return objType, nil, avgBitrate, nil // no DecoderSpecificInfo (e.g. MP3)
	}
	tag, dsi, err := dc.next()
	if err != nil || tag != 0x05 {
		return objType, nil, avgBitrate, nil
	}
	return objType, append([]byte(nil), dsi...), avgBitrate, nil
}

// opusHeadFromDOps rebuilds an OpusHead (RFC 7845) from a dOps box payload.
func opusHeadFromDOps(dops []byte) ([]byte, error) {
	if len(dops) < 11 {
		return nil, errf("dOps too short")
	}
	channels := dops[1]
	preSkip := binary.BigEndian.Uint16(dops[2:4])
	rate := binary.BigEndian.Uint32(dops[4:8])
	gain := binary.BigEndian.Uint16(dops[8:10])
	family := dops[10]

	head := make([]byte, 0, 19)
	head = append(head, "OpusHead"...)
	head = append(head, 1, channels)
	head = binary.LittleEndian.AppendUint16(head, preSkip)
	head = binary.LittleEndian.AppendUint32(head, rate)
	head = binary.LittleEndian.AppendUint16(head, gain)
	head = append(head, family)
	if family != 0 {
		need := 2 + int(channels)
		if len(dops) < 11+need {
			return nil, errf("dOps channel mapping truncated")
		}
		head = append(head, dops[11:11+need]...)
	}
	return head, nil
}

// descReader walks a sequence of MPEG-4 descriptors (tag + expandable length).
type descReader struct {
	buf []byte
	pos int
}

func (d *descReader) skip(n int) error {
	if n < 0 || d.pos+n > len(d.buf) {
		return errf("descriptor: skip past end")
	}
	d.pos += n
	return nil
}

func (d *descReader) next() (tag uint8, body []byte, err error) {
	if d.pos >= len(d.buf) {
		return 0, nil, errf("descriptor: end of data")
	}
	tag = d.buf[d.pos]
	d.pos++
	size := 0
	for i := 0; i < 4; i++ {
		if d.pos >= len(d.buf) {
			return 0, nil, errf("descriptor: truncated length")
		}
		b := d.buf[d.pos]
		d.pos++
		size = size<<7 | int(b&0x7F)
		if b&0x80 == 0 {
			break
		}
	}
	if size < 0 || d.pos+size > len(d.buf) {
		return 0, nil, errf("descriptor: body length %d exceeds data", size)
	}
	body = d.buf[d.pos : d.pos+size]
	d.pos += size
	return tag, body, nil
}

// oneSampleDescription refuses a table whose samples use sample descriptions that differ: a Matroska track carries
// one codec setup, and the samples past a switch would be decoded with the first. Identical copies are one.
func oneSampleDescription(stblBoxes []memBox) error {
	stsd, ok := findMemBox(stblBoxes, "stsd")
	if !ok || len(stsd.payload) < 8 {
		return nil
	}
	entries, err := iterBoxes(stsd.payload[8:])
	if err != nil || len(entries) < 2 {
		return nil
	}
	stsc, ok := findMemBox(stblBoxes, "stsc")
	if !ok || len(stsc.payload) < 8 {
		return nil
	}
	n := binary.BigEndian.Uint32(stsc.payload[4:8])
	for i := uint32(0); i < n && 20+int(i)*12 <= len(stsc.payload); i++ {
		sdi := binary.BigEndian.Uint32(stsc.payload[16+i*12:])
		if sdi < 1 || int(sdi) > len(entries) {
			return errf("chunk run %d names sample description %d of %d", i+1, sdi, len(entries))
		}
		if e := entries[sdi-1]; e.typ != entries[0].typ || !bytes.Equal(e.payload, entries[0].payload) {
			return errf("its samples switch between %d different sample descriptions (the codec setup changes partway), and a remux carries one: re-encode it, or set Options.SkipUnsupported to drop the track", len(entries))
		}
	}
	return nil
}
