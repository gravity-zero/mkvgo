// Package livefixture builds tiny Matroska files shaped like a live recording,
// for tests: an unknown-size Segment holding unknown-size Clusters, no
// Duration, no SeekHead, no Cues, first timestamp at 12.345 s - the structure
// of test4.mkv in the official Matroska test suite - optionally with a run of
// junk bytes in the head, which that file also carries right after its Segment
// header.
//
// The files are a few hundred bytes and are generated on the fly, so a test
// needs neither a download nor a binary in the repository. Their shape (VP8
// and PCM tracks with picture and audio parameters) is one an external
// demuxer reads without complaint: 2 streams, 12 packets each.
package livefixture

import (
	"bytes"
	"hash/crc32"
	"math"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

const (
	// Clusters, BlocksPerTrack and Tracks size the media a fixture carries.
	Clusters       = 3
	BlocksPerTrack = 4 // per cluster
	Tracks         = 2
	// Blocks is the total number of blocks in a fixture.
	Blocks = Clusters * BlocksPerTrack * Tracks
	// VideoTrack is the video track's number. Its keyframes: every block when
	// the fixture uses SimpleBlocks, the first of each cluster with BlockGroups.
	VideoTrack = 1
	// FirstTimestampMs is the first cluster's timestamp; LastBlockMs is the
	// timestamp of the last block.
	FirstTimestampMs = 12345
	LastBlockMs      = FirstTimestampMs + (Clusters-1)*1000 + (BlocksPerTrack-1)*250
)

// Options selects the shape of the fixture. The zero value is a clean live
// recording.
type Options struct {
	// JunkHead is the number of junk bytes written right after the Segment
	// header, before Info.
	JunkHead int
	// JunkMid is the number of junk bytes written between Info and Tracks.
	JunkMid int
	// TailTags appends a Tags element after the last (unknown-size) Cluster:
	// the element that has to end it.
	TailTags bool
	// ShortUnknown writes the unknown sizes on one byte (0xFF) instead of
	// eight.
	ShortUnknown bool
	// SizedSegment gives the Segment its real size, keeping the Clusters
	// unknown-size.
	SizedSegment bool
	// BlockGroups stores every block in a BlockGroup instead of a SimpleBlock,
	// as test4.mkv does. A Block has no keyframe flag: the first video frame of
	// each cluster is the keyframe (its group names no ReferenceBlock), the
	// others reference the frame before them. Audio groups never reference.
	BlockGroups bool
	// BlockDurations (with BlockGroups) gives every group a BlockDuration: the
	// shape that keeps a frame in a BlockGroup when a file is rewritten.
	BlockDurations bool
	// LacedAudio stores two audio frames per block (fixed-size lacing). The
	// audio track states no frame duration, so nothing in the file says where
	// such a block ends - the shape of test4.mkv's audio.
	LacedAudio bool
	// PositionHints gives every cluster a Position and a PrevSize element
	// (with deliberately stale values): the hints a rewrite has to restate
	// when it moves the cluster.
	PositionHints bool
	// InfoCRC opens the Info element with a CRC-32 over the rest of its body,
	// as some muxers write it (test4.mkv's does).
	InfoCRC bool
	// Overrun plants, in the middle of the second cluster, an element that
	// declares 2 GiB: it runs past the end of the file while the third cluster
	// follows intact. The blocks of the second cluster behind it are out of
	// reach of a walk that resumes on the next cluster (OverrunLostBlocks).
	Overrun bool
	// Stray plants, in the middle of the second cluster (ahead of the Overrun
	// element when both are set), a block naming StrayTrack - a track the file
	// does not declare: bytes of a damaged region that happen to parse.
	Stray bool
}

// StrayTrack is the undeclared track number a Stray block names.
const StrayTrack = 87

// OverrunLostBlocks is how many blocks an Overrun fixture hides from a walk
// that skips to the next cluster: the second half of the second cluster.
const OverrunLostBlocks = BlocksPerTrack / 2 * Tracks

// JunkByte is the value the junk runs are filled with: as an element ID it
// announces a 5-byte width, which no reader can decode.
const JunkByte = 0x0A

// Build returns the fixture's bytes.
func Build(o Options) []byte {
	var body bytes.Buffer
	body.Write(bytes.Repeat([]byte{JunkByte}, o.JunkHead))
	info := uintElem(mkv.IDTimecodeScale, 1_000_000, 3)
	if o.InfoCRC {
		sum := crc32.ChecksumIEEE(info)
		info = join(elem(0xBF, []byte{byte(sum), byte(sum >> 8), byte(sum >> 16), byte(sum >> 24)}), info)
	}
	body.Write(elem(mkv.IDInfo, info))
	body.Write(bytes.Repeat([]byte{JunkByte}, o.JunkMid))
	body.Write(elem(mkv.IDTracks, join(
		track(1, 1, "V_VP8", elem(0xE0, join(uintElem(0xB0, 320, 2), uintElem(0xBA, 240, 2)))),
		track(2, 2, "A_PCM/INT/LIT", elem(0xE1, join(floatElem(0xB5, 48000), uintElem(0x9F, 2, 1)))),
	)))
	for c := 0; c < Clusters; c++ {
		body.Write(unknownSizeHeader(mkv.IDCluster, o.ShortUnknown))
		body.Write(uintElem(mkv.IDTimestamp, uint64(FirstTimestampMs+c*1000), 2))
		if o.PositionHints {
			body.Write(uintElem(0xA7, 0x7777, 2)) // Position
			body.Write(uintElem(0xAB, 0x6666, 2)) // PrevSize
		}
		for b := 0; b < BlocksPerTrack; b++ {
			if o.Stray && c == 1 && b == BlocksPerTrack/2 {
				body.Write(elem(mkv.IDSimpleBlock, []byte{0x80 | StrayTrack, 0x00, 0x00, 0x80, 0xDE, 0xAD}))
			}
			if o.Overrun && c == 1 && b == BlocksPerTrack/2 {
				body.Write([]byte{0xEC, 0x01, 0x00, 0x00, 0x00, 0x7F, 0xFF, 0xFF, 0xFF}) // Void, 2 GiB
			}
			rel := b * 250
			for trk := byte(1); trk <= Tracks; trk++ {
				// The first payload byte doubles as the VP8 frame tag, whose low
				// bit says "not a keyframe": a demuxer that asks the codec
				// agrees with what the container states.
				keyframe := trk != VideoTrack || !o.BlockGroups || b == 0
				payload := []byte{0x00, byte(c<<4 | b), 0xAB}
				if !keyframe {
					payload[0] = 0x01
				}
				header := []byte{0x80 | trk, byte(rel >> 8), byte(rel)}
				var lacing byte
				if o.LacedAudio && trk != VideoTrack {
					// Fixed-size lacing: the frame count minus one, then the
					// frames, all the same size.
					lacing = 0x04
					payload = join([]byte{0x01}, payload, payload)
				}
				if !o.BlockGroups {
					body.Write(elem(mkv.IDSimpleBlock, join(header, []byte{0x80 | lacing}, payload)))
					continue
				}
				group := elem(mkv.IDBlock, join(header, []byte{lacing}, payload))
				if o.BlockDurations {
					group = join(group, uintElem(mkv.IDBlockDuration, 250, 1))
				}
				if !keyframe {
					group = join(group, elem(mkv.IDReferenceBlock, []byte{0xFF, 0x06})) // -250: the previous frame
				}
				body.Write(elem(mkv.IDBlockGroup, group))
			}
		}
	}
	if o.TailTags {
		simple := elem(0x67C8, join(elem(0x45A3, []byte("TITLE")), elem(0x4487, []byte("tail"))))
		body.Write(elem(mkv.IDTags, elem(mkv.IDTag, join(elem(0x63C0, nil), simple))))
	}

	var out bytes.Buffer
	out.Write(elem(ebml.IDEBMLHeader, join(
		uintElem(0x4286, 1, 1), uintElem(0x42F7, 1, 1), uintElem(0x42F2, 4, 1), uintElem(0x42F3, 8, 1),
		elem(0x4282, []byte("matroska")), uintElem(0x4287, 2, 1), uintElem(0x4285, 2, 1),
	)))
	if o.SizedSegment {
		out.Write(elem(mkv.IDSegment, body.Bytes()))
		return out.Bytes()
	}
	out.Write(unknownSizeHeader(mkv.IDSegment, o.ShortUnknown))
	out.Write(body.Bytes())
	return out.Bytes()
}

// SegmentBodyOffset returns the offset of the first byte after the Segment
// header in the fixture Build(o) returns - where JunkHead starts.
func SegmentBodyOffset(o Options) int64 {
	o.JunkHead = 0
	return int64(bytes.Index(Build(o), []byte{0x15, 0x49, 0xA9, 0x66}))
}

func track(num, typ uint64, codec string, params []byte) []byte {
	return elem(mkv.IDTrackEntry, join(
		uintElem(mkv.IDTrackNumber, num, 1),
		uintElem(mkv.IDTrackUID, num, 1),
		uintElem(mkv.IDTrackType, typ, 1),
		elem(mkv.IDCodecID, []byte(codec)),
		params,
	))
}

func elem(id uint32, body []byte) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, int64(len(body))) //nolint:errcheck // bytes.Buffer never fails
	b.Write(body)
	return b.Bytes()
}

func uintElem(id uint32, v uint64, n int) []byte {
	var b bytes.Buffer
	ebml.WriteUint(&b, v, n) //nolint:errcheck // bytes.Buffer never fails
	return elem(id, b.Bytes())
}

func floatElem(id uint32, v float64) []byte {
	bits := math.Float32bits(float32(v))
	return elem(id, []byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)})
}

func unknownSizeHeader(id uint32, short bool) []byte {
	var b bytes.Buffer
	ebml.WriteElementID(&b, id) //nolint:errcheck // bytes.Buffer never fails
	if short {
		b.WriteByte(0xFF)
		return b.Bytes()
	}
	ebml.WriteDataSize(&b, -1) //nolint:errcheck // bytes.Buffer never fails
	return b.Bytes()
}

func join(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}
