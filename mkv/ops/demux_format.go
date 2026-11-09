package ops

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// demuxFormat writes one track's frames as the file its codec's tools read: ADTS for AAC, Annex-B with the parameter
// sets for H.264 and HEVC, a low-overhead OBU stream for AV1, IVF for VP8 and VP9, MPEG-4 Part 2 behind its headers,
// Ogg for Opus and Vorbis, a FLAC stream with its STREAMINFO, CAF for ALAC, WAV for PCM, a .sup for PGS. A codec with
// none, or a track lacking what its format needs (an AudioSpecificConfig, a decoder record), keeps its payloads as
// they are stored. finish, when set, runs once the stream is flushed, on the file itself: the sizes a header
// states (WAV, CAF, an IVF frame count) are patched in then; a file that cannot seek keeps the placeholders.
type demuxFormat struct {
	ext    string
	head   func(w io.Writer) error
	frame  func(w io.Writer, blk mkv.Block, data []byte) error
	end    func(w io.Writer) error
	finish func(f io.WriteSeeker) error
}

// frameError is a frame its track's file cannot hold, as opposed to a write that failed.
type frameError struct{ err error }

func (e *frameError) Error() string { return e.err.Error() }
func (e *frameError) Unwrap() error { return e.err }

// cannotHold marks err as a frame the track's file cannot hold.
func cannotHold(format string, a ...any) error { return &frameError{fmt.Errorf(format, a...)} }

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
	case "av1":
		f = obuFormat(t)
	case "vp8", "vp9":
		f = ivfFormat(t)
	case "V_MPEG4/ISO/SP", "V_MPEG4/ISO/ASP", "V_MPEG4/ISO/AP":
		f = headersFirstFormat("m4v", t.CodecPrivate)
	case "A_ALAC":
		f = cafALACFormat(t)
	case "pcm", "A_PCM/INT/LIT", "A_PCM/INT/BIG", "A_PCM/FLOAT/IEEE":
		f = wavFormat(t)
	}
	if f == nil {
		return plain
	}
	return f
}

// --- AV1: low-overhead OBU stream -------------------------------------------------------------------------------

// obuFormat writes the blocks as temporal units of a low-overhead bitstream, each opened by a temporal delimiter
// (the stored block holds none), a keyframe that carries no sequence header taking the av1C's configOBUs: what a
// decoder reads as .obu. Written bare, the stream decoded nothing.
func obuFormat(t mkv.Track) *demuxFormat {
	var config []byte
	if len(t.CodecPrivate) > 4 {
		config = t.CodecPrivate[4:]
	}
	return &demuxFormat{ext: "obu", frame: func(w io.Writer, blk mkv.Block, data []byte) error {
		if obuType(data) == 2 { // the muxer kept the temporal delimiter
			_, err := w.Write(data)
			return err
		}
		if _, err := w.Write([]byte{0x12, 0x00}); err != nil {
			return err
		}
		if blk.Keyframe && len(config) > 0 && !holdsOBU(data, 1) {
			if _, err := w.Write(config); err != nil {
				return err
			}
		}
		_, err := w.Write(data)
		return err
	}}
}

// obuType is the type of the OBU p opens on, -1 when p is empty.
func obuType(p []byte) int {
	if len(p) == 0 {
		return -1
	}
	return int(p[0]>>3) & 0xF
}

// holdsOBU reports whether an OBU of the type sits among p's OBUs; a stream forEachOBU cannot step through counts as
// holding it, so nothing is inserted into what is not understood.
func holdsOBU(p []byte, typ byte) bool {
	found := false
	err := forEachOBU(p, func(t byte, _, _ []byte) bool {
		found = found || t == typ
		return !found
	})
	return found || err != nil
}

// --- VP8, VP9: IVF ------------------------------------------------------------------------------------------------

// ivfFormat writes the frames in IVF (DKIF header, each frame behind its size and its timestamp in milliseconds),
// the frame count patched into the header once the stream ends.
func ivfFormat(t mkv.Track) *demuxFormat {
	fourcc := map[string]string{"vp8": "VP80", "vp9": "VP90"}[t.Codec]
	var w, h uint16
	if t.Width != nil {
		w = uint16(*t.Width)
	}
	if t.Height != nil {
		h = uint16(*t.Height)
	}
	var count uint32
	return &demuxFormat{ext: "ivf",
		head: func(out io.Writer) error {
			hdr := append([]byte{'D', 'K', 'I', 'F', 0, 0, 32, 0}, fourcc...)
			hdr = binary.LittleEndian.AppendUint16(hdr, w)
			hdr = binary.LittleEndian.AppendUint16(hdr, h)
			hdr = binary.LittleEndian.AppendUint32(hdr, 1000) // timebase denominator: milliseconds
			hdr = binary.LittleEndian.AppendUint32(hdr, 1)
			hdr = binary.LittleEndian.AppendUint32(hdr, 0) // the frame count, patched by finish
			hdr = append(hdr, 0, 0, 0, 0)
			_, err := out.Write(hdr)
			return err
		},
		frame: func(out io.Writer, blk mkv.Block, data []byte) error {
			var fh [12]byte
			binary.LittleEndian.PutUint32(fh[:], uint32(len(data)))
			binary.LittleEndian.PutUint64(fh[4:], uint64(max(blk.Timecode, 0)))
			if _, err := out.Write(fh[:]); err != nil {
				return err
			}
			count++
			_, err := out.Write(data)
			return err
		},
		finish: func(f io.WriteSeeker) error { return patchU32LE(f, 24, count) },
	}
}

// patchU32LE writes v little-endian at off; a file that cannot seek (a pipe) keeps what was written.
func patchU32LE(f io.WriteSeeker, off int64, v uint32) error {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, err := f.Write(b[:])
	return err
}

// --- MPEG-4 Part 2 --------------------------------------------------------------------------------------------------

// headersFirstFormat writes the CodecPrivate (the VOS, VO and VOL headers of MPEG-4 Part 2) ahead of the frames as
// stored: the decoder reads its configuration from the stream.
func headersFirstFormat(ext string, headers []byte) *demuxFormat {
	if len(headers) == 0 {
		return nil
	}
	return &demuxFormat{ext: ext,
		head:  func(w io.Writer) error { _, err := w.Write(headers); return err },
		frame: func(w io.Writer, _ mkv.Block, data []byte) error { _, err := w.Write(data); return err },
	}
}

// --- ALAC: CAF ------------------------------------------------------------------------------------------------------

// cafALACFormat writes a CAF: the audio description from the 24-byte ALACSpecificConfig, the config as the magic
// cookie (kuki), the frames in the data chunk, its size patched once the stream ends, and then the packet table
// (pakt) every packet's size, which a variable-rate codec needs. No ALAC tool reads bare frames.
func cafALACFormat(t mkv.Track) *demuxFormat {
	cookie := t.CodecPrivate
	if len(cookie) < 24 {
		return nil
	}
	framesPerPacket := binary.BigEndian.Uint32(cookie[0:4])
	bits, channels := uint32(cookie[5]), uint32(cookie[9])
	rate := float64(binary.BigEndian.Uint32(cookie[20:24]))
	if rate == 0 && t.SampleRate != nil {
		rate = *t.SampleRate
	}
	var table []byte // the packet table: each packet's size as a varint, 1 or 2 bytes a packet
	var dataSizePos, total, packets int64
	return &demuxFormat{ext: "caf",
		head: func(w io.Writer) error {
			var b []byte
			b = append(b, 'c', 'a', 'f', 'f', 0, 1, 0, 0)
			b = cafChunk(b, "desc", 32)
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(rate))
			b = append(b, 'a', 'l', 'a', 'c')
			b = binary.BigEndian.AppendUint32(b, 0) // format flags
			b = binary.BigEndian.AppendUint32(b, 0) // bytes per packet: variable
			b = binary.BigEndian.AppendUint32(b, framesPerPacket)
			b = binary.BigEndian.AppendUint32(b, channels)
			b = binary.BigEndian.AppendUint32(b, bits)
			b = cafChunk(b, "kuki", int64(len(cookie)))
			b = append(b, cookie...)
			dataSizePos = int64(len(b)) + 4
			b = cafChunk(b, "data", -1)             // unknown until finish: valid as the last chunk if the file cannot seek
			b = binary.BigEndian.AppendUint32(b, 0) // edit count
			_, err := w.Write(b)
			return err
		},
		frame: func(w io.Writer, _ mkv.Block, data []byte) error {
			if len(data) == 0 {
				return cannotHold("empty ALAC packet")
			}
			table = cafVarint(table, uint64(len(data)))
			total += int64(len(data))
			packets++
			_, err := w.Write(data)
			return err
		},
		finish: func(f io.WriteSeeker) error {
			if _, err := f.Seek(dataSizePos, io.SeekStart); err != nil {
				return nil
			}
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], uint64(4+total))
			if _, err := f.Write(b[:]); err != nil {
				return err
			}
			if _, err := f.Seek(0, io.SeekEnd); err != nil {
				return err
			}
			var pakt []byte
			pakt = cafChunk(pakt, "pakt", int64(24+len(table)))
			pakt = binary.BigEndian.AppendUint64(pakt, uint64(packets))
			pakt = binary.BigEndian.AppendUint64(pakt, uint64(packets)*uint64(framesPerPacket))
			pakt = binary.BigEndian.AppendUint32(pakt, 0) // priming frames
			pakt = binary.BigEndian.AppendUint32(pakt, 0) // remainder frames
			pakt = append(pakt, table...)
			_, err := f.Write(pakt)
			return err
		},
	}
}

// cafChunk appends a chunk header: its type and its size (-1 when unknown).
func cafChunk(b []byte, typ string, size int64) []byte {
	b = append(b, typ...)
	return binary.BigEndian.AppendUint64(b, uint64(size))
}

// cafVarint appends v as CAF's packet table integer: 7 bits a byte, high bit set on every byte but the last.
func cafVarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	i := len(tmp)
	for {
		i--
		tmp[i] = byte(v & 0x7F)
		v >>= 7
		if v == 0 {
			break
		}
	}
	for j := i; j < len(tmp)-1; j++ {
		tmp[j] |= 0x80
	}
	return append(b, tmp[i:]...)
}

// --- PCM: WAV --------------------------------------------------------------------------------------------------------

// wavFormat writes PCM as a WAV, the sizes patched once the stream ends: integers as PCM, floats as IEEE float,
// integers above 16 bits or any layout above two channels in the extensible form; big-endian samples go out little-endian, the one order WAV
// has. A track missing its rate, channels or bit depth keeps its payloads bare.
func wavFormat(t mkv.Track) *demuxFormat {
	if t.SampleRate == nil || t.Channels == nil || t.BitDepth == nil || *t.BitDepth == 0 || *t.BitDepth%8 != 0 {
		return nil
	}
	rate, channels, bits := uint32(*t.SampleRate), uint16(*t.Channels), uint16(*t.BitDepth)
	float := t.Codec == "A_PCM/FLOAT/IEEE"
	big := t.Codec == "A_PCM/INT/BIG"
	width := int(bits / 8)
	var dataSizePos, total int64
	return &demuxFormat{ext: "wav",
		head: func(w io.Writer) error {
			tag := uint16(1)
			if float {
				tag = 3
			}
			extensible := channels > 2 || (!float && bits > 16)
			var fmtChunk []byte
			if extensible {
				fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, 0xFFFE)
			} else {
				fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, tag)
			}
			fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, channels)
			fmtChunk = binary.LittleEndian.AppendUint32(fmtChunk, rate)
			fmtChunk = binary.LittleEndian.AppendUint32(fmtChunk, rate*uint32(channels)*uint32(width))
			fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, channels*uint16(width))
			fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, bits)
			if extensible {
				fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, 22)
				fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, bits) // valid bits
				fmtChunk = binary.LittleEndian.AppendUint32(fmtChunk, 0)    // channel mask: unspecified
				fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, tag)
				fmtChunk = append(fmtChunk, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71)
			}
			var b []byte
			b = append(b, 'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'A', 'V', 'E', 'f', 'm', 't', ' ')
			b = binary.LittleEndian.AppendUint32(b, uint32(len(fmtChunk)))
			b = append(b, fmtChunk...)
			b = append(b, 'd', 'a', 't', 'a')
			dataSizePos = int64(len(b))
			b = append(b, 0, 0, 0, 0)
			_, err := w.Write(b)
			return err
		},
		frame: func(w io.Writer, _ mkv.Block, data []byte) error {
			if big && width > 1 {
				swapped := make([]byte, len(data))
				for i := 0; i+width <= len(data); i += width {
					for j := 0; j < width; j++ {
						swapped[i+j] = data[i+width-1-j]
					}
				}
				data = swapped
			}
			total += int64(len(data))
			_, err := w.Write(data)
			return err
		},
		finish: func(f io.WriteSeeker) error {
			if err := patchU32LE(f, 4, uint32(min(dataSizePos-4+total, 0xFFFFFFFF))); err != nil {
				return err
			}
			return patchU32LE(f, dataSizePos, uint32(min(total, 0xFFFFFFFF)))
		},
	}
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
	var h [7]byte // reused frame to frame
	return &demuxFormat{ext: "aac", frame: func(w io.Writer, _ mkv.Block, data []byte) error {
		n := len(data) + 7
		if n >= 1<<13 {
			return cannotHold("AAC frame of %d bytes is too long for ADTS", len(data))
		}
		h = [7]byte{
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
	var buf []byte // reused frame to frame
	return &demuxFormat{ext: ext, frame: func(w io.Writer, blk mkv.Block, data []byte) error {
		out, err := annexBAppend(buf[:0], data, lenSize)
		if err != nil {
			return &frameError{err}
		}
		buf = out
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
				return cannotHold("PGS block ends inside a segment header")
			}
			n := 3 + int(binary.BigEndian.Uint16(data[1:3]))
			if n > len(data) {
				return cannotHold("PGS segment of %d bytes past its block's %d", n, len(data))
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

// oggStream writes one logical Ogg bitstream, a packet a page (or more, for a packet longer than one).
type oggStream struct {
	serial uint32
	seq    uint32
	held   []byte // the last packet, written once the next one (or the end) says whether it is the last
	heldGP int64
}

// page writes packet with its granule position and flags (2 first, 4 last): on one page, or on as many as its 255
// lacing values a page take, each one after the first continuing it (flag 1) and only the last one ending it.
func (s *oggStream) page(w io.Writer, packet []byte, granule int64, flags byte) error {
	const pageMax = 255 * 255 // a page's body when its 255 lacing values are all 255: the packet goes on
	for cont := byte(0); ; cont = 1 {
		body, gp, f := packet, granule, flags|cont
		if len(packet) >= pageMax {
			body, gp, f = packet[:pageMax], -1, cont|flags&2 // no packet ends on this page
		}
		lacing := make([]byte, 0, 255)
		for n := len(body); len(lacing) < 255; n -= 255 {
			if n < 255 {
				lacing = append(lacing, byte(n))
				break
			}
			lacing = append(lacing, 255)
		}
		h := make([]byte, 27, 27+len(lacing)+len(body))
		copy(h, "OggS")
		h[5] = f
		binary.LittleEndian.PutUint64(h[6:], uint64(gp))
		binary.LittleEndian.PutUint32(h[14:], s.serial)
		binary.LittleEndian.PutUint32(h[18:], s.seq)
		h[26] = byte(len(lacing))
		h = append(append(h, lacing...), body...)
		binary.LittleEndian.PutUint32(h[22:], oggCRC(h))
		s.seq++
		if _, err := w.Write(h); err != nil {
			return err
		}
		if len(packet) < pageMax {
			return nil
		}
		packet, flags = packet[pageMax:], flags&^2
	}
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
		frame: func(w io.Writer, blk mkv.Block, data []byte) error {
			granule += int64(opusPacketSamples(data))
			trim := max(blk.DiscardPaddingNs, 0) * 48 / 1_000_000 // the end trim in 48 kHz samples (RFC 7845 4.4)
			return s.packet(w, data, granule-min(trim, granule))
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
// samples at the stream's rate. With the block sizes and the modes the headers give, it counts the samples the
// decoder yields (a quarter of the packet's block and of the one before, none for the first), from the first block's
// start, so the last page says where the audio ends, less the block's DiscardPadding (Vorbis I A.2). Without them it
// comes from the block timestamps: the next
// packet's start, the block's duration, or for the last one without either its start plus the step before it.
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
	head := func(w io.Writer) error {
		if err := s.page(w, headers[0], 0, 2); err != nil {
			return err
		}
		if err := s.page(w, headers[1], 0, 0); err != nil {
			return err
		}
		return s.page(w, headers[2], 0, 0)
	}
	if vb, ok := parseVorbisBlocks(headers[0], headers[2]); ok {
		granule, prev := int64(-1), 0
		return &demuxFormat{ext: "ogg", head: head, end: s.end,
			frame: func(w io.Writer, blk mkv.Block, data []byte) error {
				if granule < 0 {
					granule = max(blk.Timecode, 0) * rate / 1000
				}
				if cur := vb.blockSize(data); cur > 0 {
					if prev > 0 {
						granule += int64(prev/4 + cur/4)
					}
					prev = cur
				}
				trim := (max(blk.DiscardPaddingNs, 0)*rate + 500_000_000) / 1_000_000_000 // in ns: the nearest sample
				return s.packet(w, data, granule-min(trim, granule))
			},
		}
	}
	prevStart, step := int64(-1), int64(0)
	return &demuxFormat{ext: "ogg", head: head, end: s.end,
		frame: func(w io.Writer, blk mkv.Block, data []byte) error {
			start := max(blk.Timecode, 0) * rate / 1000
			if s.held != nil && start > prevStart {
				s.heldGP, step = start, start-prevStart // the packet held ends where this one starts
			}
			prevStart = start
			end := start + step
			if blk.Duration > 0 {
				end = (max(blk.Timecode, 0) + blk.Duration) * rate / 1000
			}
			return s.packet(w, data, end)
		},
	}
}

// vorbisBlocks is what a Vorbis audio packet's sample count takes: the two block sizes and each mode's blockflag.
type vorbisBlocks struct {
	size     [2]int
	long     []bool
	modeBits int
}

// blockSize is the block an audio packet codes, 0 for an empty or a header packet or a mode the setup lacks.
func (vb vorbisBlocks) blockSize(p []byte) int {
	if len(p) == 0 || p[0]&1 != 0 {
		return 0
	}
	mode := int(p[0]>>1) & (1<<vb.modeBits - 1)
	if mode >= len(vb.long) {
		return 0
	}
	if vb.long[mode] {
		return vb.size[1]
	}
	return vb.size[0]
}

// parseVorbisBlocks reads the block sizes from the identification header and the modes' blockflags from the end of
// the setup header, read backwards from its framing bit as the modes are the last thing it holds; the codebooks
// before them need no parsing. The mode count is the last one whose 6-bit field agrees with the modes read.
func parseVorbisBlocks(ident, setup []byte) (vorbisBlocks, bool) {
	var vb vorbisBlocks
	if len(ident) < 30 || len(setup) < 8 || setup[0] != 5 || string(setup[1:7]) != "vorbis" {
		return vb, false
	}
	e0, e1 := int(ident[28]&0x0F), int(ident[28]>>4)
	if e0 < 6 || e1 > 13 || e0 > e1 {
		return vb, false
	}
	vb.size = [2]int{1 << e0, 1 << e1}
	r := backBits{b: setup, pos: len(setup) * 8}
	for {
		bit, ok := r.read(1)
		if !ok {
			return vb, false
		}
		if bit == 1 {
			break
		}
	}
	var flags []bool
	count := 0
	for len(flags) < 64 && r.pos >= 41+6 {
		mapping, _ := r.read(8)
		transform, _ := r.read(16)
		window, _ := r.read(16)
		if mapping > 63 || transform != 0 || window != 0 {
			break
		}
		flag, _ := r.read(1)
		flags = append(flags, flag == 1)
		if n, _ := r.peek(6); int(n)+1 == len(flags) {
			count = len(flags)
		}
	}
	if count == 0 {
		return vb, false
	}
	for i := range count { // read last mode first
		vb.long = append(vb.long, flags[count-1-i])
	}
	for 1<<vb.modeBits < count {
		vb.modeBits++
	}
	return vb, true
}

// backBits reads an LSB-first bit stream backwards from bit pos, each value most significant bit first: a field
// written last comes back first, its value intact.
type backBits struct {
	b   []byte
	pos int
}

func (r *backBits) read(n int) (uint32, bool) {
	if r.pos < n {
		return 0, false
	}
	var v uint32
	for range n {
		r.pos--
		v = v<<1 | uint32(r.b[r.pos/8]>>(r.pos%8)&1)
	}
	return v, true
}

func (r backBits) peek(n int) (uint32, bool) { return r.read(n) }

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
