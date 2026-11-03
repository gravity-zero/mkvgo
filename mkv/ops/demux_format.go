package ops

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// demuxFormat writes one track's frames as the file its codec's tools read: ADTS for AAC, Annex-B with the parameter
// sets for H.264 and HEVC, Ogg for Opus and Vorbis, a FLAC stream with its STREAMINFO, a .sup for PGS. A codec with
// none, or a track lacking what its format needs (an AudioSpecificConfig, a decoder record), keeps its payloads as
// they are stored.
type demuxFormat struct {
	ext   string
	head  func(w io.Writer) error
	frame func(w io.Writer, blk mkv.Block, data []byte) error
	end   func(w io.Writer) error
}

// demuxFormatOf picks t's format; raw keeps every track's stored payloads.
func demuxFormatOf(t mkv.Track, raw bool) *demuxFormat {
	plain := &demuxFormat{
		ext:   sanitizeCodec(demuxCodecName(t)),
		frame: func(w io.Writer, _ mkv.Block, data []byte) error { _, err := w.Write(data); return err },
	}
	if raw {
		return plain
	}
	var f *demuxFormat
	switch t.Codec {
	case "aac":
		f = adtsFormat(t)
	case "h264":
		f = annexBFormat("h264", t.CodecPrivate, avcNALLengthSize(t.CodecPrivate), avcParameterSetsAnnexB(t.CodecPrivate))
	case "hevc":
		f = annexBFormat("hevc", t.CodecPrivate, hevcNALLengthSize(t.CodecPrivate), hevcParameterSetsAnnexB(t.CodecPrivate))
	case "flac":
		f = flacFormat(t)
	case "opus":
		f = oggOpusFormat(t)
	case "vorbis":
		f = oggVorbisFormat(t)
	case "pgs":
		f = supFormat()
	}
	if f == nil {
		return plain
	}
	return f
}

// --- AAC: ADTS -------------------------------------------------------------------------------------------------

// adtsSampleRates are the rates an ADTS sampling_frequency_index names.
var adtsSampleRates = [13]float64{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// adtsFormat prefixes each AAC frame with a 7-byte ADTS header built from the AudioSpecificConfig: the core coder's
// profile (an HE-AAC stream carries its SBR and PS implicitly), its core rate and channel configuration.
func adtsFormat(t mkv.Track) *demuxFormat {
	cfg := reader.ParseAACConfig(t.CodecPrivate)
	cc, ok := ascChannelConfig(t.CodecPrivate)
	if !ok || cfg.CoreObjectType < 1 || cfg.CoreObjectType > 4 {
		return nil
	}
	sf := -1
	for i, r := range adtsSampleRates {
		if r == cfg.SampleRate {
			sf = i
		}
	}
	if sf < 0 {
		return nil
	}
	profile := cfg.CoreObjectType - 1
	return &demuxFormat{ext: "aac", frame: func(w io.Writer, _ mkv.Block, data []byte) error {
		n := len(data) + 7
		if n >= 1<<13 {
			return fmt.Errorf("AAC frame of %d bytes is too long for ADTS", len(data))
		}
		h := [7]byte{
			0xFF, 0xF1, // syncword, MPEG-4, layer 0, no CRC
			byte(profile<<6) | byte(sf<<2) | byte(cc>>2),
			byte(cc&3)<<6 | byte(n>>11),
			byte(n >> 3),
			byte(n&7)<<5 | 0x1F, // buffer fullness 0x7FF: variable rate
			0xFC,                // ... and one raw data block
		}
		if _, err := w.Write(h[:]); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	}}
}

// ascChannelConfig reads an AudioSpecificConfig's channelConfiguration; ok false for one in a program config
// element (0), which ADTS cannot carry, or a config too short to hold it.
func ascChannelConfig(asc []byte) (uint32, bool) {
	var pos uint
	bits := func(n uint) uint32 {
		var v uint32
		for range n {
			if int(pos>>3) >= len(asc) {
				return 0
			}
			v = v<<1 | uint32(asc[pos>>3]>>(7-pos&7)&1)
			pos++
		}
		return v
	}
	if bits(5) == 31 {
		bits(6)
	}
	if bits(4) == 15 {
		bits(24)
	}
	cc := bits(4)
	return cc, cc > 0 && int((pos+7)>>3) <= len(asc)
}

// --- H.264 / HEVC: Annex-B -------------------------------------------------------------------------------------

// annexBFormat rewrites length-prefixed NAL units with start codes, the parameter sets of the decoder record ahead of
// the first frame and of every keyframe, so the stream decodes from any of them.
func annexBFormat(ext string, record []byte, lenSize int, params []byte) *demuxFormat {
	if len(record) == 0 || len(params) == 0 {
		return nil
	}
	first := true
	return &demuxFormat{ext: ext, frame: func(w io.Writer, blk mkv.Block, data []byte) error {
		out, err := annexBFromLengthPrefixed(data, lenSize)
		if err != nil {
			return err
		}
		if first || blk.Keyframe {
			first = false
			if _, err := w.Write(params); err != nil {
				return err
			}
		}
		_, err = w.Write(out)
		return err
	}}
}

// --- FLAC ------------------------------------------------------------------------------------------------------

// flacFormat writes the stream marker and the metadata blocks (STREAMINFO first) the CodecPrivate holds, then the
// frames as stored: a FLAC file.
func flacFormat(t mkv.Track) *demuxFormat {
	cp := t.CodecPrivate
	if len(cp) >= 4 && string(cp[:4]) == "fLaC" {
		cp = cp[4:]
	}
	if len(cp) < 4+34 || cp[0]&0x7F != 0 { // a STREAMINFO block first
		return nil
	}
	return &demuxFormat{ext: "flac",
		head: func(w io.Writer) error {
			if _, err := w.Write([]byte("fLaC")); err != nil {
				return err
			}
			_, err := w.Write(cp)
			return err
		},
		frame: func(w io.Writer, _ mkv.Block, data []byte) error { _, err := w.Write(data); return err },
	}
}

// --- PGS: .sup -------------------------------------------------------------------------------------------------

// supFormat splits each block into its PGS segments and prefixes each with the .sup header: "PG", the presentation
// and decoding timestamps (90 kHz; the decoding one 0, as players read it).
func supFormat() *demuxFormat {
	return &demuxFormat{ext: "sup", frame: func(w io.Writer, blk mkv.Block, data []byte) error {
		pts := uint32(blk.Timecode * 90)
		for len(data) > 0 {
			if len(data) < 3 {
				return fmt.Errorf("PGS block ends inside a segment header")
			}
			n := 3 + int(binary.BigEndian.Uint16(data[1:3]))
			if n > len(data) {
				return fmt.Errorf("PGS segment of %d bytes past its block's %d", n, len(data))
			}
			var h [10]byte
			h[0], h[1] = 'P', 'G'
			binary.BigEndian.PutUint32(h[2:], pts)
			if _, err := w.Write(h[:]); err != nil {
				return err
			}
			if _, err := w.Write(data[:n]); err != nil {
				return err
			}
			data = data[n:]
		}
		return nil
	}}
}

// --- Ogg: Opus, Vorbis -----------------------------------------------------------------------------------------

// oggStream writes one logical Ogg bitstream, one packet a page.
type oggStream struct {
	serial uint32
	seq    uint32
	held   []byte // the last packet, written once the next one (or the end) says whether it is the last
	heldGP int64
	begun  bool
}

// page writes one page holding packet, with its granule position and flags (2 first, 4 last).
func (s *oggStream) page(w io.Writer, packet []byte, granule int64, flags byte) error {
	var lacing []byte
	for n := len(packet); ; n -= 255 {
		if n < 255 {
			lacing = append(lacing, byte(n))
			break
		}
		lacing = append(lacing, 255)
	}
	if len(lacing) > 255 {
		return fmt.Errorf("Ogg packet of %d bytes past one page", len(packet))
	}
	h := make([]byte, 27, 27+len(lacing)+len(packet))
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], uint64(granule))
	binary.LittleEndian.PutUint32(h[14:], s.serial)
	binary.LittleEndian.PutUint32(h[18:], s.seq)
	h[26] = byte(len(lacing))
	h = append(append(h, lacing...), packet...)
	binary.LittleEndian.PutUint32(h[22:], oggCRC(h))
	s.seq++
	_, err := w.Write(h)
	return err
}

// packet queues a media packet ending at granule, writing the one before it.
func (s *oggStream) packet(w io.Writer, packet []byte, granule int64) error {
	if s.held != nil {
		if err := s.page(w, s.held, s.heldGP, 0); err != nil {
			return err
		}
	}
	s.held, s.heldGP = append(s.held[:0], packet...), granule
	return nil
}

// end writes the last packet with the end-of-stream flag.
func (s *oggStream) end(w io.Writer) error {
	if s.held == nil {
		return nil
	}
	return s.page(w, s.held, s.heldGP, 4)
}

// oggCRCTable is the Ogg page checksum's table: CRC-32, polynomial 0x04C11DB7, unreflected.
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = c<<8 ^ oggCRCTable[byte(c>>24)^x]
	}
	return c
}

// oggOpusFormat writes an Ogg Opus file: the OpusHead the CodecPrivate holds, an OpusTags, then a packet a page,
// each page's granule the 48 kHz samples decoded so far, the pre-skip included (RFC 7845).
func oggOpusFormat(t mkv.Track) *demuxFormat {
	head := t.CodecPrivate
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return nil
	}
	s := &oggStream{serial: uint32(t.ID)}
	var granule int64 // the samples decoded so far, the pre-skip among them
	return &demuxFormat{ext: "opus",
		head: func(w io.Writer) error {
			if err := s.page(w, head, 0, 2); err != nil {
				return err
			}
			tags := append([]byte("OpusTags"), 5, 0, 0, 0)
			tags = append(append(tags, "mkvgo"...), 0, 0, 0, 0)
			return s.page(w, tags, 0, 0)
		},
		frame: func(w io.Writer, _ mkv.Block, data []byte) error {
			granule += int64(opusPacketSamples(data))
			return s.packet(w, data, granule)
		},
		end: s.end,
	}
}

// opusPacketSamples is how many 48 kHz samples an Opus packet decodes to, from its TOC byte and frame count
// (RFC 6716 3.1); 0 for an empty or malformed packet.
func opusPacketSamples(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	toc := p[0]
	cfg := toc >> 3
	var per int
	switch {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		per = [4]int{480, 960, 1920, 2880}[cfg&3]
	case cfg < 16: // hybrid: 10, 20 ms
		per = [2]int{480, 960}[cfg&1]
	default: // CELT: 2.5, 5, 10, 20 ms
		per = [4]int{120, 240, 480, 960}[cfg&3]
	}
	switch toc & 3 {
	case 0:
		return per
	case 1, 2:
		return 2 * per
	}
	if len(p) < 2 {
		return 0
	}
	return int(p[1]&0x3F) * per
}

// oggVorbisFormat writes an Ogg Vorbis file: the three headers the CodecPrivate holds (Xiph-laced), the
// identification one on a page of its own, then a packet a page. A page's granule is the end of its packet in
// samples at the stream's rate, from the block timestamps: exact for the decoder, which counts samples itself, and
// to the timestamps' millisecond for a seek.
func oggVorbisFormat(t mkv.Track) *demuxFormat {
	headers, ok := xiphHeaders(t.CodecPrivate)
	if !ok || len(headers) != 3 || len(headers[0]) < 16 || string(headers[0][1:7]) != "vorbis" {
		return nil
	}
	rate := int64(binary.LittleEndian.Uint32(headers[0][12:16]))
	if rate <= 0 {
		return nil
	}
	s := &oggStream{serial: uint32(t.ID)}
	return &demuxFormat{ext: "ogg",
		head: func(w io.Writer) error {
			if err := s.page(w, headers[0], 0, 2); err != nil {
				return err
			}
			if err := s.page(w, headers[1], 0, 0); err != nil {
				return err
			}
			return s.page(w, headers[2], 0, 0)
		},
		frame: func(w io.Writer, blk mkv.Block, data []byte) error {
			endMs := blk.Timecode + blk.Duration
			return s.packet(w, data, endMs*rate/1000)
		},
		end: s.end,
	}
}

// xiphHeaders splits a Xiph-laced CodecPrivate: the count less one, the sizes of all but the last (each a run of
// 255s and a final byte), then the packets.
func xiphHeaders(cp []byte) ([][]byte, bool) {
	if len(cp) < 1 {
		return nil, false
	}
	n := int(cp[0]) + 1
	pos := 1
	sizes := make([]int, n-1)
	for i := range sizes {
		for {
			if pos >= len(cp) {
				return nil, false
			}
			sizes[i] += int(cp[pos])
			pos++
			if cp[pos-1] != 255 {
				break
			}
		}
	}
	out := make([][]byte, 0, n)
	for _, sz := range sizes {
		if pos+sz > len(cp) {
			return nil, false
		}
		out = append(out, cp[pos:pos+sz])
		pos += sz
	}
	return append(out, cp[pos:]), true
}
