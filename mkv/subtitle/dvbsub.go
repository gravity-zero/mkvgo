package subtitle

// dvbsub.go - a decoder for DVB subtitles (S_DVBSUB, ETSI EN 300 743), the bitmap subtitles of broadcast captures.
// A Matroska block holds the segments of one display set: a page composition listing the regions on screen, and the
// region compositions, colour tables (CLUT) and objects that changed. An object is a run-length picture drawn into
// every region that lists it; a region is a rectangle of 2, 4 or 8-bit pixels read through its CLUT.
//
// State spans blocks: within an epoch a display set carries only what changed, so regions, their pixels and the
// CLUTs are kept until a page composition declares a mode change or an acquisition point. Blocks are fed in file
// order; a region the read never saw defined is left out, not invented.
//
// Written from the specification, not ported: mkvgo is MIT and links no third-party subtitle decoder.

import (
	"encoding/binary"
	"fmt"
	"image"
	"time"
)

const dvbSync = 0x0f // the first byte of every segment

// Segment types.
const (
	dvbSegPage    = 0x10
	dvbSegRegion  = 0x11
	dvbSegCLUT    = 0x12
	dvbSegObject  = 0x13
	dvbSegDisplay = 0x14
	dvbSegEnd     = 0x80
)

// Pixel-data sub-block types of an object.
const (
	dvbData2Bit   = 0x10
	dvbData4Bit   = 0x11
	dvbData8Bit   = 0x12
	dvbMap2To4    = 0x20
	dvbMap2To8    = 0x21
	dvbMap4To8    = 0x22
	dvbDataEndRow = 0xf0
)

const (
	// dvbMaxDim is the widest and tallest display the format defines, and so the bound on a region.
	dvbMaxDim = 4096
	// dvbMaxEpochBytes bounds what an epoch's regions hold together: they live until the next mode change.
	dvbMaxEpochBytes = 16 << 20
	// dvbMaxBlockPixels bounds the work of one block, counted in pixels filled, drawn and handed back: a
	// full-size region and one picture of it, and the small change of placements and object references.
	dvbMaxBlockPixels = 2*dvbMaxDim*dvbMaxDim + 1<<20
	// dvbObjectRefBytes is what one object reference of a region counts for in dvbMaxEpochBytes.
	dvbObjectRefBytes = 24
)

// dvbCLUT is one CLUT family: the colours of 2, 4 and 8-bit pixels, straight RGBA.
type dvbCLUT struct {
	c4   [4][4]uint8
	c16  [16][4]uint8
	c256 [256][4]uint8
	gen  uint64 // moves each time a definition changes an entry: a picture read through it is then out of date
}

var dvbDefaultCLUT = newDVBDefaultCLUT()

// newDVBDefaultCLUT builds the contents every CLUT has before a definition changes them (clause 10).
func newDVBDefaultCLUT() *dvbCLUT {
	bit := func(i, mask, v int) uint8 {
		if i&mask != 0 {
			return uint8(v)
		}
		return 0
	}
	c := &dvbCLUT{}
	c.c4 = [4][4]uint8{{}, {255, 255, 255, 255}, {0, 0, 0, 255}, {127, 127, 127, 255}}
	for i := 1; i < 16; i++ {
		v := 255
		if i&8 != 0 {
			v = 127
		}
		c.c16[i] = [4]uint8{bit(i, 1, v), bit(i, 2, v), bit(i, 4, v), 255}
	}
	for i := 1; i < 256; i++ {
		rgb := func(low, high, base int) [3]uint8 {
			return [3]uint8{
				uint8(base) + bit(i, 0x01, low) + bit(i, 0x10, high),
				uint8(base) + bit(i, 0x02, low) + bit(i, 0x20, high),
				uint8(base) + bit(i, 0x04, low) + bit(i, 0x40, high),
			}
		}
		var p [3]uint8
		a := uint8(255)
		switch {
		case i&0x88 == 0 && i&0x70 == 0:
			p, a = rgb(255, 0, 0), 63
		case i&0x88 == 0:
			p = rgb(85, 170, 0)
		case i&0x88 == 0x08:
			p, a = rgb(85, 170, 0), 127
		case i&0x88 == 0x80:
			p = rgb(43, 85, 127)
		default:
			p = rgb(43, 85, 0)
		}
		c.c256[i] = [4]uint8{p[0], p[1], p[2], a}
	}
	return c
}

type dvbObjectRef struct {
	id   uint16
	x, y int
}

// dvbRegion is a region's pixels, as CLUT entries, and the objects that draw into it.
type dvbRegion struct {
	w, h, depth int
	clut        uint8
	pix         []byte
	objects     []dvbObjectRef
	// shown is the picture last read from the region, handed back until its pixels or its colours change; nil
	// once a pixel is written. It holds up to four bytes for each of the region's.
	shown *dvbPicture
}

// dvbPicture is a region as a display set shows it, and the CLUT it was read through.
type dvbPicture struct {
	img  *image.NRGBA // nil when the region shows nothing
	at   image.Point  // where the picture sits in the region
	clut *dvbCLUT
	gen  uint64
}

type dvbPlacement struct {
	region uint8
	x, y   int
}

// DVBSubDecoder decodes a DVB subtitle track block by block, carrying epoch state forward.
type DVBSubDecoder struct {
	pages            [2]uint16 // composition and ancillary page
	anyPage          bool
	screenW, screenH int // 0 until a display definition says otherwise
	winX, winY       int
	regions          map[uint8]*dvbRegion
	cluts            map[uint8]*dvbCLUT
	page             []dvbPlacement
	timeout          time.Duration // the page's time-out
	held             int           // what the epoch's regions hold (dvbMaxEpochBytes)
	budget           int           // what the block being decoded may still fill and draw (dvbMaxBlockPixels)
}

// NewDVBSubDecoder reads the composition and ancillary page IDs from the track's CodecPrivate, the segments of
// other pages being another service's; with a CodecPrivate too short to hold them every page is decoded.
func NewDVBSubDecoder(codecPrivate []byte) *DVBSubDecoder {
	if len(codecPrivate) < 4 {
		return &DVBSubDecoder{anyPage: true}
	}
	return &DVBSubDecoder{pages: [2]uint16{binary.BigEndian.Uint16(codecPrivate), binary.BigEndian.Uint16(codecPrivate[2:])}}
}

// Reset drops the epoch: call it when the read skips blocks, so a display set cannot draw on another part's regions.
func (d *DVBSubDecoder) Reset() {
	d.regions, d.cluts, d.page, d.held, d.timeout = nil, nil, nil, 0, 0
	d.screenW, d.screenH, d.winX, d.winY = 0, 0, 0, 0
}

// Decode decodes one Matroska block into the display sets it completes, almost always one: a set's objects are the
// regions on screen, each cut down to its visible pixels, and a set with none clears the screen; its Timeout is the
// page's, the end of a set being the next one. An image is shared by every placement, and by every display set,
// that shows the same picture: a region is read once and its image handed back for as long as neither its pixels
// nor its colours change. Treat the images as read-only. Bytes that are no
// segment end the block. A segment that cannot be read is an error, returned with the sets completed ahead of it
// and the one being composed, which show the state the decoder is left in.
//
// A block of a track stored compressed (mkv.Track.Compression) is decoded first: mkv.Track.DecodePayload.
func (d *DVBSubDecoder) Decode(block []byte) ([]BitmapDisplaySet, error) {
	var out []BitmapDisplaySet
	d.budget = dvbMaxBlockPixels
	composed := false // a page composition is waiting for the end of its display set
	emit := func() error {
		composed = false
		ds, err := d.compose()
		if err == nil {
			out = append(out, ds)
		}
		return err
	}
	fail := func(err error) ([]BitmapDisplaySet, error) {
		if composed {
			_ = emit()
		}
		return out, err
	}

	b := block
	if len(b) >= 2 && b[0] == 0x20 && b[1] == 0x00 { // the PES data field's own header, when a muxer kept it
		b = b[2:]
	}
	for len(b) > 0 && b[0] == dvbSync {
		if len(b) < 6 {
			return fail(fmt.Errorf("DVB subtitle: %d trailing byte(s) where a segment header needs 6", len(b)))
		}
		typ, page, size := b[1], binary.BigEndian.Uint16(b[2:]), int(binary.BigEndian.Uint16(b[4:]))
		if size > len(b)-6 {
			return fail(fmt.Errorf("DVB subtitle: a %#02x segment declares %d bytes, %d remain in the block", typ, size, len(b)-6))
		}
		p := b[6 : 6+size]
		b = b[6+size:]
		if !d.anyPage && page != d.pages[0] && page != d.pages[1] {
			continue
		}
		var err error
		switch typ {
		case dvbSegPage:
			if composed { // the display set before this one left out its end segment
				err = emit()
			}
			if err == nil {
				err = d.parsePage(p)
				composed = err == nil
			}
		case dvbSegRegion:
			err = d.parseRegion(p)
		case dvbSegCLUT:
			err = d.parseCLUT(p)
		case dvbSegObject:
			err = d.parseObject(p)
		case dvbSegDisplay:
			err = d.parseDisplay(p)
		case dvbSegEnd:
			if composed {
				err = emit()
			}
		}
		if err != nil {
			return fail(err)
		}
	}
	if composed {
		if err := emit(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// spend takes n pixels of work from the block's budget.
func (d *DVBSubDecoder) spend(n int) error {
	if d.budget -= n; d.budget < 0 {
		return fmt.Errorf("DVB subtitle: the block fills and draws more than %d pixels", dvbMaxBlockPixels)
	}
	return nil
}

func (d *DVBSubDecoder) parseDisplay(p []byte) error {
	if len(p) < 5 {
		return fmt.Errorf("DVB subtitle: a display definition segment needs 5 bytes, got %d", len(p))
	}
	d.screenW = int(binary.BigEndian.Uint16(p[1:])) + 1
	d.screenH = int(binary.BigEndian.Uint16(p[3:])) + 1
	d.winX, d.winY = 0, 0
	if p[0]&0x08 != 0 {
		if len(p) < 13 {
			return fmt.Errorf("DVB subtitle: a display definition segment declares a window and holds %d of its 13 bytes", len(p))
		}
		d.winX, d.winY = int(binary.BigEndian.Uint16(p[5:])), int(binary.BigEndian.Uint16(p[9:]))
	}
	return nil
}

// parsePage replaces the list of regions on screen; a mode change or an acquisition point starts a new epoch.
func (d *DVBSubDecoder) parsePage(p []byte) error {
	const head, entry = 2, 6
	if len(p) < head || (len(p)-head)%entry != 0 {
		return fmt.Errorf("DVB subtitle: a page composition segment holds %d bytes, not %d plus %d a region", len(p), head, entry)
	}
	if state := p[1] >> 2 & 3; state == 1 || state == 2 {
		d.regions, d.cluts, d.held = nil, nil, 0
	}
	d.page, d.timeout = d.page[:0], time.Duration(p[0])*time.Second
	for e := p[head:]; len(e) > 0; e = e[entry:] {
		d.page = append(d.page, dvbPlacement{region: e[0], x: int(binary.BigEndian.Uint16(e[2:])), y: int(binary.BigEndian.Uint16(e[4:]))})
	}
	return nil
}

// parseRegion defines or updates a region: its size and depth, its fill, and the objects that draw into it.
func (d *DVBSubDecoder) parseRegion(p []byte) error {
	const head = 10
	if len(p) < head {
		return fmt.Errorf("DVB subtitle: a region composition segment needs %d bytes, got %d", head, len(p))
	}
	id, fill := p[0], p[1]&0x08 != 0
	w, h := int(binary.BigEndian.Uint16(p[2:])), int(binary.BigEndian.Uint16(p[4:]))
	if w < 1 || h < 1 || w > dvbMaxDim || h > dvbMaxDim {
		return fmt.Errorf("DVB subtitle: region %d is %dx%d, outside 1..%d", id, w, h, dvbMaxDim)
	}
	var depth int
	var background byte
	switch p[6] >> 2 & 7 {
	case 1:
		depth, background = 2, p[9]>>2&3
	case 2:
		depth, background = 4, p[9]>>4
	case 3:
		depth, background = 8, p[8]
	default:
		return fmt.Errorf("DVB subtitle: region %d declares pixel depth code %d, not 2, 4 or 8 bits", id, p[6]>>2&7)
	}
	var refs []dvbObjectRef
	for e := p[head:]; len(e) > 0; {
		if len(e) < 6 {
			return fmt.Errorf("DVB subtitle: region %d ends on %d byte(s) of an object reference of 6", id, len(e))
		}
		kind := e[2] >> 6
		ref := dvbObjectRef{id: binary.BigEndian.Uint16(e), x: int(binary.BigEndian.Uint16(e[2:]) & 0xfff), y: int(binary.BigEndian.Uint16(e[4:]) & 0xfff)}
		e = e[6:]
		if kind == 1 || kind == 2 { // a character object carries its two colours and no picture
			if len(e) < 2 {
				return fmt.Errorf("DVB subtitle: region %d ends inside a character object reference", id)
			}
			e = e[2:]
			continue
		}
		refs = append(refs, ref)
	}

	r := d.regions[id]
	held := d.held + dvbObjectRefBytes*len(refs)
	if r != nil {
		held -= dvbObjectRefBytes * len(r.objects)
	}
	fresh := r == nil || r.w != w || r.h != h || r.depth != depth
	if fresh {
		held += w * h
		if r != nil {
			held -= len(r.pix)
		}
	}
	if held > dvbMaxEpochBytes {
		return fmt.Errorf("DVB subtitle: region %d brings the regions held since the last mode change to %d bytes (limit %d)", id, held, dvbMaxEpochBytes)
	}
	if fresh || fill {
		if err := d.spend(w * h); err != nil {
			return err
		}
	}
	if fresh {
		r = &dvbRegion{w: w, h: h, depth: depth, pix: make([]byte, w*h)}
		if d.regions == nil {
			d.regions = make(map[uint8]*dvbRegion, 2)
		}
		d.regions[id] = r
	}
	d.held = held
	r.clut, r.objects = p[7], refs
	if fill {
		r.shown = nil
	}
	if fill && (!fresh || background != 0) {
		for i := range r.pix {
			r.pix[i] = background
		}
	}
	return nil
}

// parseCLUT updates the entries a CLUT definition carries; the others keep their contents.
func (d *DVBSubDecoder) parseCLUT(p []byte) error {
	if len(p) < 2 {
		return fmt.Errorf("DVB subtitle: a CLUT definition segment needs 2 bytes, got %d", len(p))
	}
	c := d.cluts[p[0]]
	if c == nil {
		c = new(dvbCLUT)
		*c = *dvbDefaultCLUT
		if d.cluts == nil {
			d.cluts = make(map[uint8]*dvbCLUT, 1)
		}
		d.cluts[p[0]] = c
	}
	for e := p[2:]; len(e) > 0; {
		n := 4
		if len(e) >= 2 && e[1]&1 != 0 {
			n = 6
		}
		if len(e) < n {
			return fmt.Errorf("DVB subtitle: CLUT %d ends on %d byte(s) of an entry of %d", p[0], len(e), n)
		}
		entry, flags := int(e[0]), e[1]
		v := binary.BigEndian.Uint16(e[2:]) // 6 bits of Y, 4 of Cr, 4 of Cb, 2 of T: the most significant ones
		y, cr, cb, t := uint8(v>>10)<<2, uint8(v>>6&0xf)<<4, uint8(v>>2&0xf)<<4, uint8(v&3)<<6
		if n == 6 {
			y, cr, cb, t = e[2], e[3], e[4], e[5]
		}
		e = e[n:]
		var col [4]uint8 // Y 0 is the format's fully transparent colour
		if y != 0 {
			r, g, b := ycrcbToRGB(y, cr, cb)
			col = [4]uint8{r, g, b, 255 - t}
		}
		if flags&0x80 != 0 && entry < len(c.c4) {
			c.c4[entry] = col
		}
		if flags&0x40 != 0 && entry < len(c.c16) {
			c.c16[entry] = col
		}
		if flags&0x20 != 0 {
			c.c256[entry] = col
		}
		c.gen++
	}
	return nil
}

// parseObject draws an object's two fields into every region that lists it. Only pixel objects are drawn.
func (d *DVBSubDecoder) parseObject(p []byte) error {
	if len(p) < 3 {
		return fmt.Errorf("DVB subtitle: an object data segment needs 3 bytes, got %d", len(p))
	}
	id := binary.BigEndian.Uint16(p)
	if p[2]>>2&3 != 0 {
		return nil
	}
	if len(p) < 7 {
		return fmt.Errorf("DVB subtitle: object %d declares no field lengths", id)
	}
	topLen, bottomLen := int(binary.BigEndian.Uint16(p[3:])), int(binary.BigEndian.Uint16(p[5:]))
	if topLen+bottomLen > len(p)-7 {
		return fmt.Errorf("DVB subtitle: object %d declares %d bytes of fields, its segment holds %d", id, topLen+bottomLen, len(p)-7)
	}
	top, bottom := p[7:7+topLen], p[7+topLen:7+topLen+bottomLen]
	if bottomLen == 0 {
		bottom = top
	}
	for _, r := range d.regions {
		if err := d.spend(len(r.objects)); err != nil {
			return err
		}
		for _, ref := range r.objects {
			if ref.id != id {
				continue
			}
			r.shown = nil
			pen := dvbPen{d: d, r: r, skipOne: p[2]&2 != 0}
			if err := pen.field(top, ref.x, ref.y); err != nil {
				return fmt.Errorf("DVB subtitle: object %d: %w", id, err)
			}
			if err := pen.field(bottom, ref.x, ref.y+1); err != nil {
				return fmt.Errorf("DVB subtitle: object %d: %w", id, err)
			}
		}
	}
	return nil
}

// dvbPen draws one object into one region.
type dvbPen struct {
	d       *DVBSubDecoder
	r       *dvbRegion
	skipOne bool // pixel code 1 leaves the region's pixel as it is
	x, y    int
	table   []byte // what the codes of the string being read map to, nil for the codes themselves
	err     error
}

// run writes n pixels of one code at the pen, clipped to the region, and moves the pen.
func (p *dvbPen) run(code byte, n int) {
	if p.err != nil {
		return
	}
	if p.err = p.d.spend(n); p.err != nil {
		return
	}
	x0, x1 := p.x, min(p.x+n, p.r.w)
	p.x += n
	if p.skipOne && code == 1 || p.y >= p.r.h || x0 >= x1 {
		return
	}
	if p.table != nil {
		code = p.table[code]
	}
	row := p.r.pix[p.y*p.r.w+x0 : p.y*p.r.w+x1]
	for i := range row {
		row[i] = code
	}
}

// field draws one field's pixel-data sub-blocks: every other line of the object, from x, y.
func (p *dvbPen) field(data []byte, x, y int) error {
	map2to4 := [4]byte{0x0, 0x7, 0x8, 0xf}
	map2to8 := [4]byte{0x00, 0x77, 0x88, 0xff}
	var map4to8 [16]byte
	for i := range map4to8 {
		map4to8[i] = byte(i) * 0x11
	}
	if p.err = p.d.spend(8 * len(data)); p.err != nil { // reading a bit counts as drawing a pixel
		return p.err
	}
	p.x, p.y = x, y
	depth := p.r.depth
	for len(data) > 0 && p.err == nil {
		typ := data[0]
		data = data[1:]
		tableLen := 0
		switch typ {
		case dvbData2Bit:
			switch depth {
			case 8:
				p.table = map2to8[:]
			case 4:
				p.table = map2to4[:]
			default:
				p.table = nil
			}
			data = data[p.string2(data):]
		case dvbData4Bit:
			if depth < 4 {
				return fmt.Errorf("a 4-bit pixel string in a %d-bit region", depth)
			}
			p.table = nil
			if depth == 8 {
				p.table = map4to8[:]
			}
			data = data[p.string4(data):]
		case dvbData8Bit:
			if depth < 8 {
				return fmt.Errorf("an 8-bit pixel string in a %d-bit region", depth)
			}
			p.table = nil
			data = data[p.string8(data):]
		case dvbMap2To4:
			tableLen = 2
		case dvbMap2To8:
			tableLen = 4
		case dvbMap4To8:
			tableLen = 16
		case dvbDataEndRow:
			p.x, p.y = x, p.y+2
		default: // a type the format does not define: its length is not known, the field ends here
			return p.err
		}
		if tableLen == 0 {
			continue
		}
		if len(data) < tableLen {
			return fmt.Errorf("a map table of %d bytes, %d remain", tableLen, len(data))
		}
		switch typ {
		case dvbMap2To4:
			map2to4 = [4]byte{data[0] >> 4, data[0] & 0xf, data[1] >> 4, data[1] & 0xf}
		case dvbMap2To8:
			copy(map2to8[:], data)
		case dvbMap4To8:
			copy(map4to8[:], data)
		}
		data = data[tableLen:]
	}
	return p.err
}

// dvbBits reads a pixel code string bit by bit; past its end it reads zeros, which every string ends on.
type dvbBits struct {
	data []byte
	pos  int
}

func (b *dvbBits) take(n int) int {
	v := 0
	for ; n > 0; n-- {
		v <<= 1
		if b.pos>>3 < len(b.data) {
			v |= int(b.data[b.pos>>3]>>(7-b.pos&7)) & 1
		}
		b.pos++
	}
	return v
}

// used is how many whole bytes the string took, the stuffing bits that end it included.
func (b *dvbBits) used() int { return min((b.pos+7)>>3, len(b.data)) }

// string2 draws a 2-bit/pixel code string and returns the bytes it took (clause 7.2.5.2).
func (p *dvbPen) string2(data []byte) int {
	b := dvbBits{data: data}
	for p.err == nil {
		if v := b.take(2); v != 0 {
			p.run(byte(v), 1)
			continue
		}
		if b.take(1) == 1 {
			n := b.take(3) + 3
			p.run(byte(b.take(2)), n)
			continue
		}
		if b.take(1) == 1 {
			p.run(0, 1)
			continue
		}
		switch b.take(2) {
		case 0:
			return b.used()
		case 1:
			p.run(0, 2)
		case 2:
			n := b.take(4) + 12
			p.run(byte(b.take(2)), n)
		case 3:
			n := b.take(8) + 29
			p.run(byte(b.take(2)), n)
		}
	}
	return b.used()
}

// string4 draws a 4-bit/pixel code string and returns the bytes it took.
func (p *dvbPen) string4(data []byte) int {
	b := dvbBits{data: data}
	for p.err == nil {
		if v := b.take(4); v != 0 {
			p.run(byte(v), 1)
			continue
		}
		if b.take(1) == 0 {
			n := b.take(3)
			if n == 0 {
				return b.used()
			}
			p.run(0, n+2)
			continue
		}
		if b.take(1) == 0 {
			n := b.take(2) + 4
			p.run(byte(b.take(4)), n)
			continue
		}
		switch b.take(2) {
		case 0:
			p.run(0, 1)
		case 1:
			p.run(0, 2)
		case 2:
			n := b.take(4) + 9
			p.run(byte(b.take(4)), n)
		case 3:
			n := b.take(8) + 25
			p.run(byte(b.take(4)), n)
		}
	}
	return b.used()
}

// string8 draws an 8-bit/pixel code string and returns the bytes it took.
func (p *dvbPen) string8(data []byte) int {
	b := dvbBits{data: data}
	for p.err == nil {
		if v := b.take(8); v != 0 {
			p.run(byte(v), 1)
			continue
		}
		long, n := b.take(1), b.take(7)
		if long == 1 {
			p.run(byte(b.take(8)), n)
		} else if n == 0 {
			return b.used()
		} else {
			p.run(0, n)
		}
	}
	return b.used()
}

// compose turns the page into a display set: each region on screen, read through its CLUT, where the page puts it.
// A region is read once for as long as it shows the same thing: the placements of one display set share its
// picture, and so do the display sets that follow until its pixels or its colours change.
func (d *DVBSubDecoder) compose() (BitmapDisplaySet, error) {
	ds := BitmapDisplaySet{ScreenW: d.screenW, ScreenH: d.screenH, Timeout: d.timeout}
	if ds.ScreenW == 0 { // no display definition: the format's standard-definition plane
		ds.ScreenW, ds.ScreenH = 720, 576
	}
	for _, pl := range d.page {
		r := d.regions[pl.region]
		if r == nil {
			if err := d.spend(1); err != nil {
				return ds, err
			}
			continue
		}
		clut := d.cluts[r.clut]
		if clut == nil {
			clut = dvbDefaultCLUT
		}
		pic := r.shown
		if pic == nil || pic.clut != clut || pic.gen != clut.gen {
			if err := d.spend(r.w * r.h); err != nil {
				return ds, err
			}
			pic = &dvbPicture{clut: clut, gen: clut.gen}
			pic.img, pic.at = trimTransparent(renderDVBRegion(r, clut))
			r.shown = pic
		} else if err := d.spend(1); err != nil {
			return ds, err
		}
		if pic.img != nil {
			ds.Objects = append(ds.Objects, BitmapObject{X: d.winX + pl.x + pic.at.X, Y: d.winY + pl.y + pic.at.Y, Image: pic.img})
		}
	}
	return ds, nil
}

// renderDVBRegion reads a region's pixels through a CLUT.
func renderDVBRegion(r *dvbRegion, clut *dvbCLUT) *image.NRGBA {
	colours := clut.c256[:]
	switch r.depth {
	case 4:
		colours = clut.c16[:]
	case 2:
		colours = clut.c4[:]
	}
	img := image.NewNRGBA(image.Rect(0, 0, r.w, r.h))
	for i, v := range r.pix {
		if c := colours[int(v)&(len(colours)-1)]; c[3] != 0 {
			copy(img.Pix[4*i:4*i+4], c[:])
		}
	}
	return img
}
