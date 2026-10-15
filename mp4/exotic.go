package mp4

import (
	"encoding/binary"
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

// cicpChannelCounts is the channel count of each ChannelConfiguration of ISO/IEC 23001-8 (CICP), 1 to 20.
var cicpChannelCounts = [21]uint8{0, 1, 2, 3, 4, 5, 6, 8, 2, 3, 4, 7, 8, 24, 8, 12, 10, 12, 14, 12, 14}

// cicpChannels is the channel count of a CICP ChannelConfiguration; 0 when it names none.
func cicpChannels(cicp byte) uint8 {
	if int(cicp) < len(cicpChannelCounts) {
		return cicpChannelCounts[cicp]
	}
	return 0
}

// carriableTracks keeps the tracks a remux can carry, dropping the others
// under Options.SkipUnsupported and refusing the remux otherwise.
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
		if !o.SkipUnsupported {
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
