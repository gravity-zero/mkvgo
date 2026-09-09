package subtitle

// pgs.go - a decoder for HDMV Presentation Graphic Stream subtitles
// (S_HDMV/PGS), the bitmap subtitle format Blu-ray rips carry. There is no text
// in a PGS track: every cue is a run-length-encoded, palettized picture plus the
// screen position it is drawn at, so a consumer gets an image and a rectangle
// rather than a string.
//
// The unit of the format is the DISPLAY SET: a PCS naming what is on screen,
// optional WDS/PDS/ODS defining the windows, palette and pictures it refers to,
// and an END. Matroska stores one display set per block, and the block timecode
// is its presentation time - so this file decodes bytes only, and the caller
// supplies the timing.
//
// State spans blocks on purpose. Within an EPOCH a display set may reference an
// object or palette defined by an earlier one and re-sent by neither, so the
// decoder carries palettes and objects forward and clears them only when a PCS
// declares an epoch start. Consequence for callers: blocks must be fed in file
// order, from the start of the track, or a display set can land on objects that
// were never defined (those are skipped, not invented).
//
// Written from the format description, not ported: mkvgo is MIT and links no
// third-party subtitle decoder.

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
)

// Segment types, from the display set's segment header (type, 16-bit size).
const (
	pgsSegPDS = 0x14 // palette definition
	pgsSegODS = 0x15 // object definition (the picture, RLE)
	pgsSegPCS = 0x16 // presentation composition: what is on screen, and where
	pgsSegWDS = 0x17 // window definition
	pgsSegEND = 0x80 // end of display set
)

// Composition object flags (the byte after the window ID reference).
const (
	pgsObjectCropped = 0x80
	pgsObjectForced  = 0x40
)

// pgsEpochStart is the composition state that invalidates every palette and
// object carried over from the previous display sets.
const pgsEpochStart = 0x80

const (
	// pgsMaxObjectDim is the format's own ceiling on an object's width and
	// height. It is also the memory guard: composing allocates width*height*4
	// bytes, so a bogus dimension must be refused before it is believed.
	pgsMaxObjectDim = 4096
	// pgsMaxObjectBytes bounds the RLE accumulation buffer a single object's
	// declared data length asks for. Real subtitle objects are tens of KiB; a
	// 3-byte length field can otherwise claim 16 MiB per object.
	pgsMaxObjectBytes = 4 << 20
)

// PGSObject is one picture of a display set, already composed: the palette is
// applied and the crop, if the composition asked for one, is taken.
//
// Image is *image.NRGBA rather than *image.RGBA because a PGS palette carries
// STRAIGHT alpha, while Go's image.RGBA is alpha-premultiplied by convention -
// storing straight values there would make every draw.Draw and every encode
// wrong, and premultiplying first is lossy. *image.NRGBA satisfies image.Image
// and png.Encode writes it without a conversion.
type PGSObject struct {
	// X, Y are the object's position on the subtitle screen, as the PCS gives
	// it - not relative to the window, and not relative to the video frame if
	// the two differ in size (see PGSDisplaySet.ScreenW/ScreenH).
	X, Y int
	// Forced marks a composition the disc wants shown even when subtitles are
	// off: signs, foreign dialogue. It is per-composition, so a track can mix
	// forced and normal cues - the track's own forced flag cannot say this.
	Forced bool
	Image  *image.NRGBA
}

// PGSDisplaySet is one decoded display set. A set with no objects is the
// format's "clear the screen" instruction: it carries no picture, and its
// presentation time is the END time of whatever was shown before it.
type PGSDisplaySet struct {
	ScreenW, ScreenH int // the subtitle plane the positions are expressed in
	Objects          []PGSObject
}

// Clear reports whether this display set empties the screen rather than filling
// it.
func (ds *PGSDisplaySet) Clear() bool { return len(ds.Objects) == 0 }

// pgsPalette holds a palette's 256 entries, already converted to straight RGBA.
// Entries default to fully transparent: a PDS updates only the entries it
// carries, and an index the palette never defined must not paint anything.
type pgsPalette [256][4]uint8

// pgsObjectDef is a defined object, kept as its RLE rather than as a decoded
// bitmap. Two reasons: an epoch's objects stay alive across display sets, and
// the RLE is one to two orders of magnitude smaller than the bitmap; and the
// same object may be re-composed under a NEW palette, so the pixels cannot be
// resolved once and cached anyway.
type pgsObjectDef struct {
	w, h int
	rle  []byte
	want int // total RLE bytes the first ODS declared, for the multi-segment case
}

// PGSDecoder decodes a PGS track block by block, carrying epoch state forward.
// The zero value is ready to use.
type PGSDecoder struct {
	palettes map[uint8]*pgsPalette
	objects  map[uint16]*pgsObjectDef
	pending  *pgsObjectDef // object being accumulated across several ODS
	pendID   uint16
}

// NewPGSDecoder returns a decoder with no epoch state.
func NewPGSDecoder() *PGSDecoder { return &PGSDecoder{} }

// Reset drops every palette and object. Callers that seek in a track rather
// than reading it front to back should call it, so a display set cannot compose
// against pictures that belong to a different part of the file.
func (d *PGSDecoder) Reset() {
	d.palettes, d.objects, d.pending = nil, nil, nil
}

// Decode decodes one Matroska block into the display sets it holds. Almost
// always that is exactly one; the slice exists because the format allows a
// block to carry several, and dropping the extras would silently lose cues.
//
// The returned images are freshly allocated and owned by the caller.
func (d *PGSDecoder) Decode(block []byte) ([]PGSDisplaySet, error) {
	var out []PGSDisplaySet
	var comp *pgsComposition

	b := block
	for len(b) > 0 {
		if len(b) < 3 {
			return nil, fmt.Errorf("PGS: %d trailing byte(s) where a segment header needs 3", len(b))
		}
		typ := b[0]
		size := int(binary.BigEndian.Uint16(b[1:3]))
		if size > len(b)-3 {
			return nil, fmt.Errorf("PGS: a %#x segment declares %d bytes, %d remain in the block", typ, size, len(b)-3)
		}
		payload := b[3 : 3+size]
		b = b[3+size:]

		switch typ {
		case pgsSegPCS:
			// A PCS opens a display set: emit the one before it if the stream
			// left out its END segment (some muxers do).
			if comp != nil {
				ds, err := d.compose(comp)
				if err != nil {
					return nil, err
				}
				out = append(out, ds)
			}
			c, err := parsePGSComposition(payload)
			if err != nil {
				return nil, err
			}
			if c.state&pgsEpochStart != 0 {
				// An epoch start invalidates everything carried over. It comes
				// BEFORE the PDS/ODS of its own display set, so clearing here
				// discards the old epoch without touching the new one.
				d.palettes, d.objects, d.pending = nil, nil, nil
			}
			comp = c
		case pgsSegPDS:
			if err := d.parsePalette(payload); err != nil {
				return nil, err
			}
		case pgsSegODS:
			if err := d.parseObject(payload); err != nil {
				return nil, err
			}
		case pgsSegWDS:
			// Windows are the clip regions the composition draws into, but an
			// object's on-screen position comes from the PCS, not the window,
			// so nothing here is needed to place a picture. It is still parsed:
			// a truncated WDS means the block is not what it claims to be, and
			// that is worth failing on rather than reading past.
			if err := validatePGSWindows(payload); err != nil {
				return nil, err
			}
		case pgsSegEND:
			if comp == nil {
				continue // an END with no composition: nothing to emit
			}
			ds, err := d.compose(comp)
			if err != nil {
				return nil, err
			}
			out = append(out, ds)
			comp = nil
		default:
			// Unknown segment types are skipped by size, not guessed at: the
			// header is self-describing, so an extension cannot desynchronize
			// the parse.
		}
	}
	if comp != nil {
		ds, err := d.compose(comp)
		if err != nil {
			return nil, err
		}
		out = append(out, ds)
	}
	return out, nil
}

// pgsComposition is a parsed PCS: the screen it addresses and the objects it
// wants drawn, before those objects are resolved.
type pgsComposition struct {
	screenW, screenH int
	state            byte
	paletteID        uint8
	objects          []pgsCompositionObject
}

type pgsCompositionObject struct {
	objectID uint16
	x, y     int
	forced   bool
	cropped  bool
	cropX    int
	cropY    int
	cropW    int
	cropH    int
}

func parsePGSComposition(p []byte) (*pgsComposition, error) {
	const head = 11
	if len(p) < head {
		return nil, fmt.Errorf("PGS: a composition segment needs %d bytes, got %d", head, len(p))
	}
	c := &pgsComposition{
		screenW:   int(binary.BigEndian.Uint16(p[0:2])),
		screenH:   int(binary.BigEndian.Uint16(p[2:4])),
		state:     p[7],
		paletteID: p[9],
	}
	n := int(p[10])
	rest := p[head:]
	for i := 0; i < n; i++ {
		const objHead = 8
		if len(rest) < objHead {
			return nil, fmt.Errorf("PGS: composition declares %d objects, the segment holds %d", n, i)
		}
		o := pgsCompositionObject{
			objectID: binary.BigEndian.Uint16(rest[0:2]),
			forced:   rest[3]&pgsObjectForced != 0,
			cropped:  rest[3]&pgsObjectCropped != 0,
			x:        int(binary.BigEndian.Uint16(rest[4:6])),
			y:        int(binary.BigEndian.Uint16(rest[6:8])),
		}
		rest = rest[objHead:]
		if o.cropped {
			const cropLen = 8
			if len(rest) < cropLen {
				return nil, fmt.Errorf("PGS: composition object %d is flagged cropped but carries no crop rectangle", o.objectID)
			}
			o.cropX = int(binary.BigEndian.Uint16(rest[0:2]))
			o.cropY = int(binary.BigEndian.Uint16(rest[2:4]))
			o.cropW = int(binary.BigEndian.Uint16(rest[4:6]))
			o.cropH = int(binary.BigEndian.Uint16(rest[6:8]))
			rest = rest[cropLen:]
		}
		c.objects = append(c.objects, o)
	}
	return c, nil
}

// validatePGSWindows checks a window definition segment is complete. The
// windows themselves are not kept - see the WDS case in Decode.
func validatePGSWindows(p []byte) error {
	if len(p) < 1 {
		return fmt.Errorf("PGS: an empty window definition segment")
	}
	const winLen = 9 // ID, x, y, width, height
	if got, want := len(p)-1, int(p[0])*winLen; got != want {
		return fmt.Errorf("PGS: window definition declares %d window(s) (%d bytes), the segment holds %d", p[0], want, got)
	}
	return nil
}

// parsePalette applies a palette definition. Definitions are CUMULATIVE within
// an epoch: a PDS carries only the entries it changes, so an existing palette is
// updated in place rather than replaced.
func (d *PGSDecoder) parsePalette(p []byte) error {
	if len(p) < 2 {
		return fmt.Errorf("PGS: a palette segment needs 2 bytes of header, got %d", len(p))
	}
	id := p[0]
	entries := p[2:]
	const entryLen = 5 // index, Y, Cr, Cb, alpha
	if len(entries)%entryLen != 0 {
		return fmt.Errorf("PGS: palette %d holds %d bytes of entries, not a multiple of %d", id, len(entries), entryLen)
	}
	if d.palettes == nil {
		d.palettes = make(map[uint8]*pgsPalette, 1)
	}
	pal := d.palettes[id]
	if pal == nil {
		pal = &pgsPalette{}
		d.palettes[id] = pal
	}
	for off := 0; off < len(entries); off += entryLen {
		e := entries[off : off+entryLen]
		r, g, bl := ycrcbToRGB(e[1], e[2], e[3])
		pal[e[0]] = [4]uint8{r, g, bl, e[4]}
	}
	return nil
}

// parseObject accumulates an object definition. One object's RLE may be split
// over several ODS segments (a display set's segments are size-capped), which is
// why the decoder holds a pending object between them.
func (d *PGSDecoder) parseObject(p []byte) error {
	const head = 4 // object ID, version, sequence flags
	if len(p) < head {
		return fmt.Errorf("PGS: an object segment needs %d bytes of header, got %d", head, len(p))
	}
	id := binary.BigEndian.Uint16(p[0:2])
	seq := p[3]
	rest := p[head:]

	const (
		seqLast  = 0x40
		seqFirst = 0x80
	)
	if seq&seqFirst != 0 {
		const dims = 7 // 3-byte data length, then width and height
		if len(rest) < dims {
			return fmt.Errorf("PGS: object %d declares no dimensions", id)
		}
		// object_data_length counts the width and height fields too, so the RLE
		// is four bytes shorter than it says.
		dataLen := int(rest[0])<<16 | int(rest[1])<<8 | int(rest[2])
		w := int(binary.BigEndian.Uint16(rest[3:5]))
		h := int(binary.BigEndian.Uint16(rest[5:7]))
		if w <= 0 || h <= 0 || w > pgsMaxObjectDim || h > pgsMaxObjectDim {
			return fmt.Errorf("PGS: object %d is %dx%d, outside 1..%d", id, w, h, pgsMaxObjectDim)
		}
		if dataLen < 4 || dataLen-4 > pgsMaxObjectBytes {
			return fmt.Errorf("PGS: object %d declares %d bytes of data (limit %d)", id, dataLen, pgsMaxObjectBytes)
		}
		want := dataLen - 4
		d.pendID = id
		d.pending = &pgsObjectDef{w: w, h: h, want: want, rle: make([]byte, 0, want)}
		rest = rest[dims:]
	} else if d.pending == nil || d.pendID != id {
		return fmt.Errorf("PGS: object %d continues a definition that never started", id)
	}

	if len(d.pending.rle)+len(rest) > d.pending.want {
		return fmt.Errorf("PGS: object %d sends %d bytes of data, it declared %d",
			id, len(d.pending.rle)+len(rest), d.pending.want)
	}
	d.pending.rle = append(d.pending.rle, rest...)

	if seq&seqLast != 0 {
		if len(d.pending.rle) != d.pending.want {
			return fmt.Errorf("PGS: object %d ends with %d of the %d bytes it declared",
				id, len(d.pending.rle), d.pending.want)
		}
		if d.objects == nil {
			d.objects = make(map[uint16]*pgsObjectDef, 2)
		}
		d.objects[id] = d.pending
		d.pending = nil
	}
	return nil
}

// compose turns a parsed composition into a display set: every object it names
// is decoded against the palette it names, cropped if asked, and placed.
func (d *PGSDecoder) compose(c *pgsComposition) (PGSDisplaySet, error) {
	ds := PGSDisplaySet{ScreenW: c.screenW, ScreenH: c.screenH}
	if len(c.objects) == 0 {
		return ds, nil
	}
	pal := d.palettes[c.paletteID]
	if pal == nil {
		// A composition against a palette this epoch never defined paints
		// nothing at all. Treating it as a clear is the honest reading - there
		// is no colour to invent - and it keeps the timeline consistent.
		pal = &pgsPalette{}
	}
	for _, co := range c.objects {
		def := d.objects[co.objectID]
		if def == nil {
			// The object was defined in a part of the epoch the caller did not
			// read (a mid-file start). Skipping keeps the rest of the display
			// set; inventing a picture would be worse than a missing one.
			continue
		}
		img, err := decodePGSRLE(def, pal)
		if err != nil {
			return ds, fmt.Errorf("PGS: object %d: %w", co.objectID, err)
		}
		x, y := co.x, co.y
		if co.cropped {
			r := image.Rect(co.cropX, co.cropY, co.cropX+co.cropW, co.cropY+co.cropH).Intersect(img.Bounds())
			if r.Empty() {
				continue
			}
			// SubImage shares the backing array; re-origin it at 0,0 so the
			// caller gets a plain top-left-anchored picture and X/Y stay the
			// composition's own coordinates.
			sub := img.SubImage(r).(*image.NRGBA)
			cropped := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
			for row := 0; row < r.Dy(); row++ {
				copy(cropped.Pix[row*cropped.Stride:(row+1)*cropped.Stride],
					sub.Pix[row*sub.Stride:row*sub.Stride+cropped.Stride])
			}
			img = cropped
		}
		ds.Objects = append(ds.Objects, PGSObject{X: x, Y: y, Forced: co.forced, Image: img})
	}
	return ds, nil
}

// decodePGSRLE expands one object's run-length data straight into an NRGBA
// bitmap. The encoding is per line: a non-zero byte is one pixel of that palette
// index, a zero byte introduces a run whose length and colour are in the one to
// three bytes that follow, and a bare 0x00 0x00 ends the line.
//
// Pixels the data never writes stay transparent, which is why the bitmap is
// allocated zeroed and never filled with a background first.
//
// Two tolerances, both for data seen in the wild rather than for the spec: a run
// that overruns its line continues on the next one instead of being an error,
// and trailing bytes past the last line are ignored. A rip whose encoder forgot
// one end-of-line marker still yields the right picture; refusing it would cost
// the whole track for one byte.
func decodePGSRLE(def *pgsObjectDef, pal *pgsPalette) (*image.NRGBA, error) {
	img := image.NewNRGBA(image.Rect(0, 0, def.w, def.h))
	data := def.rle
	x, y := 0, 0

	paint := func(idx byte, run int) {
		c := pal[idx]
		for run > 0 && y < def.h {
			if x >= def.w {
				x, y = 0, y+1
				continue
			}
			n := def.w - x
			if n > run {
				n = run
			}
			// Fully transparent entries need no write at all: the bitmap is
			// already zeroed, and most of a subtitle picture is background.
			if c[3] != 0 {
				off := img.PixOffset(x, y)
				for i := 0; i < n; i++ {
					copy(img.Pix[off:off+4], c[:])
					off += 4
				}
			}
			x += n
			run -= n
		}
	}

	for i := 0; i < len(data) && y < def.h; {
		b0 := data[i]
		i++
		if b0 != 0 {
			paint(b0, 1)
			continue
		}
		if i >= len(data) {
			return nil, fmt.Errorf("the run-length data ends on an escape byte")
		}
		b1 := data[i]
		i++
		switch {
		case b1 == 0: // end of line
			x, y = 0, y+1
		case b1 < 0x40: // b1 pixels of colour 0
			paint(0, int(b1))
		case b1 < 0x80: // 14-bit run of colour 0
			if i >= len(data) {
				return nil, fmt.Errorf("a long transparent run is truncated")
			}
			run := int(b1&0x3f)<<8 | int(data[i])
			i++
			paint(0, run)
		case b1 < 0xc0: // 6-bit run of the colour that follows
			if i >= len(data) {
				return nil, fmt.Errorf("a coloured run is missing its colour")
			}
			run := int(b1 & 0x3f)
			idx := data[i]
			i++
			paint(idx, run)
		default: // 14-bit run of the colour that follows
			if i+1 >= len(data) {
				return nil, fmt.Errorf("a long coloured run is truncated")
			}
			run := int(b1&0x3f)<<8 | int(data[i])
			idx := data[i+1]
			i += 2
			paint(idx, run)
		}
	}
	return img, nil
}

// ycrcbToRGB converts one palette entry. PGS carries LIMITED-range BT.601 luma
// (16..235), so the conversion expands the range as well as rotating the axes -
// skipping the expansion is the classic washed-out-subtitle bug. Fixed-point
// coefficients, so the result does not depend on the FPU.
func ycrcbToRGB(y, cr, cb uint8) (uint8, uint8, uint8) {
	yy := 298 * (int(y) - 16)
	u := int(cb) - 128
	v := int(cr) - 128
	r := (yy + 409*v + 128) >> 8
	g := (yy - 100*u - 208*v + 128) >> 8
	b := (yy + 516*u + 128) >> 8
	return clampByte(r), clampByte(g), clampByte(b)
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// Paletted re-indexes a decoded subtitle picture against its own colours, or
// returns nil when it holds more than a palette can (which a PGS picture never
// does: the format's own palette has 256 entries, and one display set draws
// from one palette).
//
// It exists because the natural way to store these pictures is the expensive
// one. A PGS cue decodes to at most 256 distinct colours, so encoding it as
// truecolour RGBA spends four bytes a pixel to say what one byte can. Measured
// over two full tracks of a real disc, PNG-encoding the same cues: 9.4 MB and
// 13.4 MB as NRGBA, 6.3 MB and 8.8 MB indexed - and 21.0 MB and 28.6 MB for the
// undecoded PGS stream they came from, which is the thing worth knowing if you
// are choosing what a cache should hold.
func Paletted(img *image.NRGBA) *image.Paletted {
	if img == nil {
		return nil
	}
	b := img.Bounds()
	seen := make(map[color.NRGBA]uint8, 32)
	var pal color.Palette
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			o := img.PixOffset(x, y)
			c := color.NRGBA{R: img.Pix[o], G: img.Pix[o+1], B: img.Pix[o+2], A: img.Pix[o+3]}
			if _, ok := seen[c]; ok {
				continue
			}
			if len(pal) == 256 {
				return nil
			}
			seen[c] = uint8(len(pal))
			pal = append(pal, c)
		}
	}
	out := image.NewPaletted(b, pal)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			o := img.PixOffset(x, y)
			out.SetColorIndex(x, y, seen[color.NRGBA{R: img.Pix[o], G: img.Pix[o+1], B: img.Pix[o+2], A: img.Pix[o+3]}])
		}
	}
	return out
}
