package subtitle

// vobsub.go - a decoder for DVD subpictures (S_VOBSUB), the bitmap subtitles of DVD rips. A Matroska block holds
// one subpicture unit: a four-colour picture, run-length coded as two interlaced fields, and the control sequences
// that give its colours, its place on the plane and when it is shown and removed. The sixteen colours the picture
// picks from and the plane's size are in the track's CodecPrivate, the text header of a .idx file.
//
// A subpicture stands alone: the only thing the decoder holds between blocks is the start of a unit that a muxer
// spread over several of them.
//
// Written from the format description, not ported: mkvgo is MIT and links no third-party subtitle decoder.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"
)

// Commands of a subpicture control sequence.
const (
	spuForcedStart = 0x00 // show, even with subtitles off
	spuStart       = 0x01
	spuStop        = 0x02
	spuColours     = 0x03 // the palette entry of each of the four pixel values
	spuContrast    = 0x04 // the opacity of each, 0..15
	spuArea        = 0x05
	spuOffsets     = 0x06 // where each field's data starts
	spuChange      = 0x07 // colour and contrast changes within the picture, not applied
	spuEnd         = 0xff
)

// vobsubMaxDim is the widest and tallest area the 12-bit coordinates can name: the bound on what one block decodes.
const vobsubMaxDim = 4096

// VobSubDecoder decodes a VobSub track block by block.
type VobSubDecoder struct {
	screenW, screenH int
	palette          [16][3]uint8
	pending          []byte // a unit whose first blocks hold less than it declares, at most 64 KiB
}

// Reset drops a unit left incomplete: call it when the read skips blocks, so the next one is not taken for its rest.
func (d *VobSubDecoder) Reset() { d.pending = nil }

// Continues reports whether Decode will take block as the rest of the unit the blocks before it started, rather
// than as the start of a unit: a caller asks it ahead of each Decode, a unit's time being that of its first block.
func (d *VobSubDecoder) Continues(block []byte) bool {
	return len(d.pending) > 0 && !wholeSPU(block)
}

// NewVobSubDecoder reads the plane size and the palette from the track's CodecPrivate.
func NewVobSubDecoder(codecPrivate []byte) (*VobSubDecoder, error) {
	d := &VobSubDecoder{}
	var sized, coloured bool
	for _, line := range strings.Split(string(bytes.TrimRight(codecPrivate, "\x00")), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "size":
			ws, hs, _ := strings.Cut(strings.ToLower(val), "x")
			w, errW := strconv.Atoi(strings.TrimSpace(ws))
			h, errH := strconv.Atoi(strings.TrimSpace(hs))
			if errW != nil || errH != nil || w <= 0 || h <= 0 || w > vobsubMaxDim || h > vobsubMaxDim {
				return nil, fmt.Errorf("VobSub: the CodecPrivate's size %q is not a width x height within 1..%d", val, vobsubMaxDim)
			}
			d.screenW, d.screenH, sized = w, h, true
		case "palette":
			cols := strings.Split(val, ",")
			if len(cols) != len(d.palette) {
				return nil, fmt.Errorf("VobSub: the CodecPrivate's palette holds %d colours, a subpicture picks from %d", len(cols), len(d.palette))
			}
			for i, c := range cols {
				rgb, err := strconv.ParseUint(strings.TrimSpace(c), 16, 24)
				if err != nil {
					return nil, fmt.Errorf("VobSub: the CodecPrivate's palette entry %d, %q, is not an RRGGBB colour", i, strings.TrimSpace(c))
				}
				d.palette[i] = [3]uint8{uint8(rgb >> 16), uint8(rgb >> 8), uint8(rgb)}
			}
			coloured = true
		}
	}
	if !sized || !coloured {
		return nil, fmt.Errorf("VobSub: the track's CodecPrivate lacks the size: or palette: line of a .idx header, without which its pictures have no colours")
	}
	return d, nil
}

// spuState is what the control sequences of one subpicture say.
type spuState struct {
	colour, alpha  [4]uint8 // per pixel value 0..3
	x1, y1, x2, y2 int
	offset         [2]int
	hasArea        bool
	hasOffsets     bool
	started        bool
	forced         bool
	stopped        bool
	locked         bool // the picture is on screen and visible: later sequences no longer restyle it
	startAt        int
	stopAt         int
}

func (s *spuState) visible() bool { return s.alpha != [4]uint8{} }

// Decode decodes one Matroska block, a whole subpicture unit: one display set holding its picture, cut down to its
// visible pixels, with the Delay before it shows and the Duration its stop command gives it (zero when it has
// none), or an empty set for a unit that shows nothing. A unit spread over several blocks gives nothing until the
// block that completes it, and its time is the first block's: Continues tells, ahead of Decode, whether a block
// starts a unit. A block that is a whole unit of its own never completes the one before, which is dropped.
//
// A block of a track stored compressed (mkv.Track.Compression) is decoded first: mkv.Track.DecodePayload.
func (d *VobSubDecoder) Decode(block []byte) ([]BitmapDisplaySet, error) {
	if d.Continues(block) {
		block = append(d.pending, block...)
	}
	d.pending = nil // a block that is a unit of its own does not complete the one before, which is dropped
	if len(block) < 4 {
		return nil, fmt.Errorf("VobSub: a subpicture needs 4 bytes of header, the block holds %d", len(block))
	}
	size := int(binary.BigEndian.Uint16(block[0:2]))
	ctrl := int(binary.BigEndian.Uint16(block[2:4]))
	if ctrl < 4 || ctrl+4 > size {
		return nil, fmt.Errorf("VobSub: the subpicture places its control sequences at byte %d of %d", ctrl, size)
	}
	if size > len(block) {
		d.pending = append([]byte(nil), block...)
		return nil, nil
	}
	spu := block[:size]
	st := spuState{colour: [4]uint8{0, 1, 2, 3}, alpha: [4]uint8{0, 15, 15, 15}}
	if err := st.read(spu, ctrl); err != nil {
		return nil, err
	}

	ds := BitmapDisplaySet{ScreenW: d.screenW, ScreenH: d.screenH}
	if !st.started {
		if !st.stopped {
			return nil, nil
		}
		ds.Delay = spuDate(st.stopAt)
		return []BitmapDisplaySet{ds}, nil
	}
	if st.stopped && st.stopAt == st.startAt { // shown and removed at once: nothing is seen
		ds.Delay = spuDate(st.startAt)
		return []BitmapDisplaySet{ds}, nil
	}
	if !st.hasArea || !st.hasOffsets {
		return nil, fmt.Errorf("VobSub: the subpicture starts a display without naming its area and its data")
	}
	img, err := d.picture(spu[:ctrl], &st)
	if err != nil {
		return nil, err
	}
	ds.Delay = spuDate(st.startAt)
	if st.stopped && st.stopAt > st.startAt {
		ds.Duration = spuDate(st.stopAt - st.startAt)
	}
	if img, at := trimTransparent(img); img != nil {
		ds.Objects = []BitmapObject{{X: st.x1 + at.X, Y: st.y1 + at.Y, Forced: st.forced, Image: img}}
	}
	return []BitmapDisplaySet{ds}, nil
}

// wholeSPU reports a block that holds exactly the unit its header declares, control sequences inside it.
func wholeSPU(block []byte) bool {
	if len(block) < 8 {
		return false
	}
	size, ctrl := int(binary.BigEndian.Uint16(block[0:2])), int(binary.BigEndian.Uint16(block[2:4]))
	return size == len(block) && ctrl >= 4 && ctrl+4 <= size
}

// spuDate converts a control sequence's date, in units of 1024 ticks of the 90 kHz clock.
func spuDate(units int) time.Duration {
	return time.Duration(units) * 1024 * time.Second / 90000
}

// read walks the chain of control sequences from ctrl, each naming the next; the last names itself.
func (s *spuState) read(spu []byte, ctrl int) error {
	for pos := ctrl; ; {
		if pos+4 > len(spu) {
			return fmt.Errorf("VobSub: a control sequence at byte %d runs past the subpicture's %d bytes", pos, len(spu))
		}
		date := int(binary.BigEndian.Uint16(spu[pos:]))
		next := int(binary.BigEndian.Uint16(spu[pos+2:]))
		if err := s.commands(spu, pos+4, date); err != nil {
			return err
		}
		if s.started && s.visible() {
			s.locked = true
		}
		if next == pos {
			return nil
		}
		if next < pos+4 {
			return fmt.Errorf("VobSub: the control sequence at byte %d names byte %d as the next one, which is not after it", pos, next)
		}
		pos = next
	}
}

// commands applies one control sequence. An unknown command ends it: its length is not known.
func (s *spuState) commands(spu []byte, p, date int) error {
	need := func(n int) error {
		if p+n > len(spu) {
			return fmt.Errorf("VobSub: command %#02x at byte %d needs %d more byte(s), the subpicture ends first", spu[p-1], p-1, n)
		}
		return nil
	}
	for p < len(spu) {
		cmd := spu[p]
		p++
		switch cmd {
		case spuForcedStart, spuStart:
			if !s.started {
				s.started, s.forced, s.startAt = true, cmd == spuForcedStart, date
			}
		case spuStop:
			if !s.stopped {
				s.stopped, s.stopAt = true, date
			}
		case spuColours, spuContrast:
			if err := need(2); err != nil {
				return err
			}
			v := [4]uint8{spu[p+1] & 0xf, spu[p+1] >> 4, spu[p] & 0xf, spu[p] >> 4}
			if !s.locked && cmd == spuColours {
				s.colour = v
			} else if !s.locked {
				s.alpha = v
			}
			p += 2
		case spuArea:
			if err := need(6); err != nil {
				return err
			}
			if !s.locked {
				s.x1 = int(spu[p])<<4 | int(spu[p+1])>>4
				s.x2 = int(spu[p+1]&0xf)<<8 | int(spu[p+2])
				s.y1 = int(spu[p+3])<<4 | int(spu[p+4])>>4
				s.y2 = int(spu[p+4]&0xf)<<8 | int(spu[p+5])
				s.hasArea = true
			}
			p += 6
		case spuOffsets:
			if err := need(4); err != nil {
				return err
			}
			if !s.locked {
				s.offset = [2]int{int(binary.BigEndian.Uint16(spu[p:])), int(binary.BigEndian.Uint16(spu[p+2:]))}
				s.hasOffsets = true
			}
			p += 4
		case spuChange:
			if err := need(2); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(spu[p:]))
			if n < 2 || p+n > len(spu) {
				return fmt.Errorf("VobSub: a colour change command at byte %d declares %d bytes, %d remain", p-1, n, len(spu)-p)
			}
			p += n
		default: // spuEnd, or a command this decoder does not know
			return nil
		}
	}
	return nil
}

// picture expands the two fields into one image: the even lines from the first, the odd lines from the second.
func (d *VobSubDecoder) picture(data []byte, s *spuState) (*image.NRGBA, error) {
	w, h := s.x2-s.x1+1, s.y2-s.y1+1
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("VobSub: the subpicture's area runs from %d,%d to %d,%d, which holds no pixel", s.x1, s.y1, s.x2, s.y2)
	}
	for _, off := range s.offset {
		if off < 4 || off >= len(data) {
			return nil, fmt.Errorf("VobSub: the subpicture places a field's data at byte %d, outside the %d bytes of picture data", off, len(data))
		}
	}
	var rgba [4][4]uint8
	for v := range rgba {
		c := d.palette[s.colour[v]]
		rgba[v] = [4]uint8{c[0], c[1], c[2], s.alpha[v] * 17}
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	nib := [2]int{s.offset[0] * 2, s.offset[1] * 2} // each field's read position, in nibbles
	for y := 0; y < h; y++ {
		pos := nib[y&1]
		for x := 0; x < w; {
			code := 0
			for n := 0; n < 4; n++ {
				if pos>>1 >= len(data) {
					return nil, fmt.Errorf("VobSub: the picture's data ends on line %d of %d", y, h)
				}
				code = code<<4 | int(data[pos>>1]>>(4*(1-pos&1)))&0xf
				pos++
				if code >= 4<<(2*n) {
					break
				}
			}
			run := code >> 2
			if run == 0 || run > w-x { // a zero run fills the rest of the line
				run = w - x
			}
			if c := rgba[code&3]; c[3] != 0 {
				off := img.PixOffset(x, y)
				for i := 0; i < run; i++ {
					copy(img.Pix[off:off+4], c[:])
					off += 4
				}
			}
			x += run
		}
		nib[y&1] = (pos + 1) &^ 1 // a line ends on a byte boundary
	}
	return img, nil
}
