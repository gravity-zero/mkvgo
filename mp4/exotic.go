package mp4

import (
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// exotic.go - the sample entries a probe describes without a remux being able
// to carry them: protected (encv/enca) entries read through their sinf, PCM
// entries mapped to the Matroska PCM CodecIDs, every other entry by its fourcc.

// protectedEntry reads a protected sample entry's sinf: the original format
// (frma) the entry wraps and the scheme (schm) its samples are encrypted with.
func protectedEntry(payload []byte, headerLens ...int) (original, scheme string) {
	scheme = "unspecified"
	for _, hdr := range headerLens {
		if len(payload) < hdr {
			continue
		}
		children, err := iterBoxes(payload[hdr:])
		if err != nil {
			continue
		}
		sinf, ok := findMemBox(children, "sinf")
		if !ok {
			continue
		}
		boxes, err := iterBoxes(sinf.payload)
		if err != nil {
			return "", scheme
		}
		if frma, ok := findMemBox(boxes, "frma"); ok && len(frma.payload) >= 4 {
			original = string(frma.payload[:4])
		}
		if schm, ok := findMemBox(boxes, "schm"); ok && len(schm.payload) >= 8 {
			if s := strings.TrimSpace(string(schm.payload[4:8])); s != "" {
				scheme = s
			}
		}
		return original, scheme
	}
	return "", scheme
}

// describeEntry records what any sample entry states about its track: the
// picture size of a visual entry, the channel count and rate of an audio one.
func describeEntry(tr *inTrack, payload []byte) {
	switch tr.trackType {
	case mkv.VideoTrack:
		if len(payload) >= 32 {
			tr.width = uint32(binary.BigEndian.Uint16(payload[24:26]))
			tr.height = uint32(binary.BigEndian.Uint16(payload[26:28]))
		}
		parseBitrate(tr, payload, 78)
	case mkv.AudioTrack:
		parseAudioFields(tr, payload)
	}
}

// proResBitDepth is the bit depth a ProRes fourcc states, as the mainstream decoder reads it: 10 for the
// proxy, LT, standard and HQ profiles, 12 for 4444 and 4444 XQ; 0 for a fourcc stating none (ProRes RAW).
func proResBitDepth(fourcc string) uint16 {
	switch fourcc {
	case "apco", "apcs", "apcn", "apch":
		return 10
	case "ap4h", "ap4x":
		return 12
	}
	return 0
}

// cicpChannelCounts is the channel count of each ChannelConfiguration of ISO/IEC 23001-8 (CICP), 1 to 20.
var cicpChannelCounts = [21]uint8{0, 1, 2, 3, 4, 5, 6, 8, 2, 3, 4, 7, 8, 24, 8, 12, 10, 12, 14, 12, 14}

// cicpChannels is the channel count of a CICP ChannelConfiguration; 0 when it names none.
func cicpChannels(cicp byte) uint8 {
	if int(cicp) < len(cicpChannelCounts) {
		return cicpChannelCounts[cicp]
	}
	return 0
}

// pcmFlags: the formatSpecificFlags of a QuickTime version 2 lpcm entry.
const (
	lpcmFloat          = 1 << 0
	lpcmBigEndian      = 1 << 1
	lpcmSignedInteger  = 1 << 2
	lpcmNonInterleaved = 1 << 5
)

// parsePCM maps a PCM sample entry to its Matroska CodecID, bit depth and
// bytes per frame, or names on tr.unsupported the layout Matroska has no form for.
func parsePCM(tr *inTrack, typ string, payload []byte, headerLen int) {
	parseAudioFields(tr, payload)
	tr.codec = typ
	var (
		bits      uint32
		float     bool
		bigEndian bool
		unsigned  bool
	)
	if len(payload) >= 20 {
		bits = uint32(binary.BigEndian.Uint16(payload[18:20]))
	}
	switch typ {
	case "twos":
		bigEndian = true
	case "sowt":
	case "raw ":
		unsigned = true
	case "in24", "in32", "fl32", "fl64":
		bits = map[string]uint32{"in24": 24, "in32": 32, "fl32": 32, "fl64": 64}[typ]
		float = typ[0] == 'f'
		bigEndian = !quickTimeLittleEndian(payload, headerLen)
	case "ipcm", "fpcm":
		pcmC, err := childConfig(payload, headerLen, "pcmC")
		if err != nil || len(pcmC) < 6 {
			tr.unsupported = "PCM entry " + quoteFourcc(typ) + " without a pcmC box"
			return
		}
		bigEndian = pcmC[4]&1 == 0
		bits = uint32(pcmC[5])
		float = typ == "fpcm"
	case "lpcm":
		if headerLen != 64 {
			tr.unsupported = "lpcm entry without the version 2 sound description"
			return
		}
		bits = binary.BigEndian.Uint32(payload[48:52])
		flags := binary.BigEndian.Uint32(payload[52:56])
		float, bigEndian, unsigned = flags&lpcmFloat != 0, flags&lpcmBigEndian != 0, flags&lpcmSignedInteger == 0
		if flags&lpcmNonInterleaved != 0 {
			tr.unsupported = "lpcm entry with non-interleaved channels"
			return
		}
		if frame := binary.BigEndian.Uint32(payload[56:60]); tr.channels > 0 && frame != uint32(tr.channels)*bits/8 {
			tr.unsupported = "lpcm entry whose packet is not one frame of packed samples"
			return
		}
	}
	switch {
	case bits == 0 || bits%8 != 0 || bits > 64:
		tr.unsupported = "PCM entry " + quoteFourcc(typ) + " of " + strconv.Itoa(int(bits)) + " bits per sample"
	case float && bigEndian:
		tr.unsupported = "big-endian float PCM: Matroska A_PCM/FLOAT/IEEE is little-endian"
	case float:
		tr.codec, tr.unsupported = "A_PCM/FLOAT/IEEE", ""
	case bits == 8 && !unsigned:
		tr.unsupported = "signed 8-bit PCM: Matroska 8-bit PCM is unsigned"
	case bits > 8 && unsigned:
		tr.unsupported = "unsigned " + strconv.Itoa(int(bits)) + "-bit PCM: Matroska integer PCM above 8 bits is signed"
	case bigEndian:
		tr.codec = "A_PCM/INT/BIG"
	default:
		tr.codec = "pcm" // A_PCM/INT/LIT
	}
	if tr.unsupported != "" {
		return
	}
	tr.bitDepth = uint8(bits)
	tr.pcmFrame = uint32(tr.channels) * bits / 8
	if tr.pcmFrame == 0 {
		tr.unsupported = "PCM entry " + quoteFourcc(typ) + " without a channel count"
	}
}

// quickTimeLittleEndian reports the enda flag of a QuickTime 'wave' extension (1 = little-endian samples).
func quickTimeLittleEndian(payload []byte, headerLen int) bool {
	enda, err := childConfig(payload, headerLen, "enda")
	return err == nil && len(enda) >= 2 && binary.BigEndian.Uint16(enda) == 1
}

// pcmPacketFrames bounds a block of PCM: the packet size the mainstream
// demuxer reads such a track in, so a remux's packets match the source's.
const pcmPacketFrames = 1024

// coalescePCM gathers a PCM track's one-frame samples into blocks of up to
// pcmPacketFrames contiguous frames, never across a chunk or trun boundary.
func coalescePCM(t *inTrack) {
	if t.pcmFrame == 0 || len(t.samples) == 0 {
		return
	}
	out := t.samples[:0]
	var cur inSample
	var frames uint32
	flush := func() {
		if frames > 0 {
			out = append(out, cur)
		}
		frames = 0
	}
	for _, s := range t.samples {
		n := s.size / t.pcmFrame
		if n == 0 {
			n = 1
		}
		if frames > 0 && (s.run || cur.offset+int64(cur.size) != s.offset || frames+n > pcmPacketFrames) {
			flush()
		}
		if frames == 0 {
			cur = s
			cur.sync = true
		} else {
			cur.size += s.size
			cur.durMs = ticksToMs(s.ctsTicks+ticksOf(s.durMs, t.timescale)-cur.ctsTicks, t.timescale)
		}
		frames += n
	}
	flush()
	t.samples = out
	t.frameDurNs = 0 // the blocks' lengths vary with the chunks: no constant frame duration
	t.frameCount = int64(len(out))
}

// ticksOf converts a millisecond duration back to ticks, the inverse of ticksToMs for a PCM sample's own duration.
func ticksOf(ms int64, timescale uint32) int64 {
	return ms * int64(timescale) / 1000
}

// fragmentEnds walks a fragmented file's moof boxes for each track's decode
// end (tfdt plus its trun durations), the duration a moov without mehd does
// not state; media bytes are never read.
func fragmentEnds(r io.ReadSeeker, size int64, moovBoxes []memBox, mv *movie) {
	trex := parseTrexDefaults(moovBoxes)
	ends := map[uint32]int64{}
	var hdr [16]byte
	for off := int64(0); off+8 <= size; {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			break
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			break
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < headerLen || off+boxSize > size {
			break
		}
		if typ == "moof" && boxSize-headerLen <= maxFragMoofBytes {
			payload, err := readExact(r, boxSize-headerLen)
			if err != nil {
				break
			}
			moofEnds(payload, trex, ends)
		}
		off += boxSize
	}
	for id, end := range ends {
		t := trackByID(mv, id)
		if t == nil || t.timescale == 0 {
			continue
		}
		endMs := ticksToMs(end, t.timescale)
		t.durationMs = max(t.durationMs, endMs)
		mv.durationMs = max(mv.durationMs, endMs)
	}
}

// moofEnds raises each track's decode end from the trafs of one moof.
func moofEnds(moof []byte, trex map[uint32]trexDefault, ends map[uint32]int64) {
	boxes, err := iterBoxes(moof)
	if err != nil {
		return
	}
	for _, b := range boxes {
		if b.typ != "traf" {
			continue
		}
		trafBoxes, err := iterBoxes(b.payload)
		if err != nil {
			continue
		}
		var h tfhd
		var haveTfhd, haveTfdt bool
		var start int64
		for _, tb := range trafBoxes {
			switch tb.typ {
			case "tfhd":
				if h, err = parseTfhd(tb.payload); err == nil {
					haveTfhd = true
				}
			case "tfdt":
				start, haveTfdt = parseTfdt(tb.payload), true
			}
		}
		if !haveTfhd {
			continue
		}
		if !haveTfdt {
			start = ends[h.trackID] // the fragment continues where the track's previous one ended
		}
		td := trex[h.trackID]
		if !h.haveDur {
			h.defDur = td.duration
		}
		if !h.haveFlags {
			h.defFlags = td.flags
		}
		var dur int64
		for _, tb := range trafBoxes {
			if tb.typ == "trun" {
				d, _, _, err := trunSpan(tb.payload, h.defDur, h.defFlags)
				if err != nil {
					return
				}
				dur += d
			}
		}
		if end := start + dur; end > ends[h.trackID] {
			ends[h.trackID] = end
		}
	}
}

// carriableTracks keeps the tracks a remux can carry. An audio or video track
// it cannot refuses the remux, or is dropped under Options.SkipUnsupported; a
// subtitle track never does - it is dropped with its reason, the rule of every
// operation that writes MP4.
func carriableTracks(mv *movie, o *Options) error {
	kept := mv.tracks[:0]
	for i := range mv.tracks {
		t := mv.tracks[i]
		reason := t.unsupported
		if t.encryption != "" {
			reason = "encrypted (" + t.encryption + "): mkvgo does not decrypt"
		}
		if reason == "" {
			kept = append(kept, t)
			continue
		}
		if !o.SkipUnsupported && t.trackType != mkv.SubtitleTrack {
			return errf("track %d (%s): %s (set Options.SkipUnsupported to drop it)", t.trackID, t.codec, reason)
		}
		o.report(DroppedTrack{ID: uint64(t.trackID), Type: t.trackType, Codec: t.codec, Reason: reason})
	}
	mv.tracks = kept
	if len(mv.tracks) == 0 {
		return errf("no convertible tracks found in MP4")
	}
	return nil
}

// sampleCount is how many samples a movie's tracks hold.
func sampleCount(mv *movie) int {
	var n int
	for i := range mv.tracks {
		n += len(mv.tracks[i].samples)
	}
	return n
}

// clusterSpanMax is the widest timecode spread a cluster's SimpleBlocks can
// encode at the millisecond timecode scale.
const clusterSpanMax = math.MaxInt16
