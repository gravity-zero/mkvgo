package subtitle

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- synthetic DVB builders: segments byte by byte, so the tests pin the wire format ---

const dvbTestPage, dvbTestAncillary = 0x0001, 0x0152

var dvbTestPrivate = []byte{0x00, 0x01, 0x01, 0x52, 0x10}

func dvbSeg(typ byte, page uint16, p []byte) []byte {
	return append([]byte{dvbSync, typ, byte(page >> 8), byte(page), byte(len(p) >> 8), byte(len(p))}, p...)
}

// dvbPCS is a page composition: state 0 normal case, 1 acquisition point, 2 mode change.
func dvbPCS(state byte, at ...dvbPlacement) []byte {
	p := []byte{15, state << 2}
	for _, a := range at {
		p = append(p, a.region, 0, byte(a.x>>8), byte(a.x), byte(a.y>>8), byte(a.y))
	}
	return dvbSeg(dvbSegPage, dvbTestPage, p)
}

// dvbRCS is a region composition; depthCode is 1, 2 or 3 for 2, 4 or 8 bits.
func dvbRCS(id byte, fill bool, w, h int, depthCode, clut, background byte, refs ...dvbObjectRef) []byte {
	flags := byte(0)
	if fill {
		flags = 0x08
	}
	p := []byte{id, flags, byte(w >> 8), byte(w), byte(h >> 8), byte(h), depthCode<<5 | depthCode<<2, clut, background, background<<4 | background&3<<2}
	for _, r := range refs {
		p = append(p, byte(r.id>>8), byte(r.id), byte(r.x>>8)&0xf, byte(r.x), byte(r.y>>8)&0xf, byte(r.y))
	}
	return dvbSeg(dvbSegRegion, dvbTestPage, p)
}

// dvbCDS is a CLUT definition; an entry is its ID, its flags, then Y, Cr, Cb, T in full range.
func dvbCDS(id byte, entries ...[6]byte) []byte {
	p := []byte{id, 0}
	for _, e := range entries {
		p = append(p, e[:]...)
	}
	return dvbSeg(dvbSegCLUT, dvbTestPage, p)
}

func dvbODS(id uint16, nonModifying bool, top, bottom []byte) []byte {
	flags := byte(0)
	if nonModifying {
		flags = 2
	}
	p := []byte{byte(id >> 8), byte(id), flags, byte(len(top) >> 8), byte(len(top)), byte(len(bottom) >> 8), byte(len(bottom))}
	return dvbSeg(dvbSegObject, dvbTestPage, append(append(p, top...), bottom...))
}

// dvbGrow appends bytes to a segment's payload and declares them in its length.
func dvbGrow(seg []byte, more ...byte) []byte {
	out := append(append([]byte(nil), seg...), more...)
	out[4], out[5] = byte((len(out)-6)>>8), byte(len(out)-6)
	return out
}

func dvbEDS() []byte { return dvbSeg(dvbSegEnd, dvbTestPage, nil) }

// dvbBitString packs a string of 0s and 1s, spaces ignored, into bytes, zero-padded.
func dvbBitString(s string) []byte {
	s = strings.ReplaceAll(s, " ", "")
	out := make([]byte, (len(s)+7)/8)
	for i, c := range s {
		if c == '1' {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

// 4-bit entries of CLUT 0: 1 white, 2 blue, 3 white at half transparency.
var dvbTestCLUT = dvbCDS(0, [6]byte{1, 0x41, 235, 128, 128, 0}, [6]byte{2, 0x41, 82, 90, 240, 0}, [6]byte{3, 0x41, 235, 128, 128, 0x80})

// A 4x2 object in 4-bit strings: codes 1, 1, 2, 3 on the top line, a run of four 2s on the bottom one.
var (
	dvbTop4x2    = []byte{dvbData4Bit, 0x11, 0x23, 0x00, dvbDataEndRow}
	dvbBottom4x2 = []byte{dvbData4Bit, 0x08, 0x20, 0x00, dvbDataEndRow}
)

func dvbShow4x2() []byte {
	return concat(
		dvbPCS(2, dvbPlacement{region: 0, x: 100, y: 500}),
		dvbRCS(0, true, 4, 2, 2, 0, 0, dvbObjectRef{id: 7}),
		dvbTestCLUT,
		dvbODS(7, false, dvbTop4x2, dvbBottom4x2),
		dvbEDS(),
	)
}

func newDVBSub(t *testing.T) *DVBSubDecoder {
	t.Helper()
	return NewDVBSubDecoder(dvbTestPrivate)
}

func decodeOneDVB(t *testing.T, d *DVBSubDecoder, block []byte) BitmapDisplaySet {
	t.Helper()
	sets, err := d.Decode(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("got %d display sets, want 1", len(sets))
	}
	return sets[0]
}

// --- tests ---

func TestDVBSubDecoder_ShowThenClear(t *testing.T) {
	d := newDVBSub(t)
	ds := decodeOneDVB(t, d, dvbShow4x2())
	if ds.ScreenW != 720 || ds.ScreenH != 576 {
		t.Errorf("plane %dx%d, want the 720x576 of a stream without display definition", ds.ScreenW, ds.ScreenH)
	}
	if len(ds.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(ds.Objects))
	}
	o := ds.Objects[0]
	if o.X != 100 || o.Y != 500 || o.Forced {
		t.Errorf("object at %d,%d forced=%v, want 100,500 not forced", o.X, o.Y, o.Forced)
	}
	if b := o.Image.Bounds(); b.Dx() != 4 || b.Dy() != 2 {
		t.Fatalf("image %dx%d, want 4x2", b.Dx(), b.Dy())
	}
	br, bg, bb := ycrcbToRGB(82, 90, 240)
	white, blue, half := [4]uint8{255, 255, 255, 255}, [4]uint8{br, bg, bb, 255}, [4]uint8{255, 255, 255, 127}
	want := [2][4][4]uint8{{white, white, blue, half}, {blue, blue, blue, blue}}
	for y := range want {
		for x := range want[y] {
			if got := nrgbaAt(t, o.Image, x, y); got != want[y][x] {
				t.Errorf("pixel %d,%d = %v, want %v", x, y, got, want[y][x])
			}
		}
	}
	if ds := decodeOneDVB(t, d, concat(dvbPCS(0), dvbEDS())); !ds.Clear() {
		t.Errorf("a page with no region gave %d objects, want a clear", len(ds.Objects))
	}
}

// Within an epoch a display set sends only what changed: the region keeps the pixels of the blocks before.
func TestDVBSubDecoder_NormalCaseKeepsTheRegion(t *testing.T) {
	d := newDVBSub(t)
	at := dvbPlacement{region: 0, x: 10, y: 20}
	decodeOneDVB(t, d, concat(dvbPCS(2, at), dvbRCS(0, true, 12, 2, 2, 0, 0, dvbObjectRef{id: 7}), dvbTestCLUT,
		dvbODS(7, false, dvbTop4x2, dvbBottom4x2), dvbEDS()))
	ds := decodeOneDVB(t, d, concat(dvbPCS(0, at), dvbRCS(0, false, 12, 2, 2, 0, 0, dvbObjectRef{id: 8, x: 8}),
		dvbODS(8, false, dvbTop4x2, dvbBottom4x2), dvbEDS()))
	if len(ds.Objects) != 1 || ds.Objects[0].Image.Bounds().Dx() != 12 {
		t.Fatalf("got %d objects, the first %v wide; want one of 12, both words", len(ds.Objects), ds.Objects[0].Image.Bounds().Dx())
	}
	img := ds.Objects[0].Image
	if nrgbaAt(t, img, 0, 0)[3] == 0 || nrgbaAt(t, img, 8, 0)[3] == 0 || nrgbaAt(t, img, 5, 0)[3] != 0 {
		t.Errorf("the region lost the first object, or the second, or the gap between them is painted")
	}
	// A page that only lists the region again shows it as it is.
	if ds := decodeOneDVB(t, d, concat(dvbPCS(0, at), dvbEDS())); len(ds.Objects) != 1 {
		t.Errorf("a page listing a kept region gave %d objects, want 1", len(ds.Objects))
	}
}

func TestDVBSubDecoder_ModeChangeAndResetStartAnEpoch(t *testing.T) {
	at := dvbPlacement{region: 0, x: 10, y: 20}
	for name, forget := range map[string]func(d *DVBSubDecoder) []byte{
		"mode change":       func(*DVBSubDecoder) []byte { return dvbPCS(2, at) },
		"acquisition point": func(*DVBSubDecoder) []byte { return dvbPCS(1, at) },
		"Reset":             func(d *DVBSubDecoder) []byte { d.Reset(); return dvbPCS(0, at) },
	} {
		d := newDVBSub(t)
		decodeOneDVB(t, d, dvbShow4x2())
		if ds := decodeOneDVB(t, d, concat(forget(d), dvbEDS())); !ds.Clear() {
			t.Errorf("%s: a region of the epoch before is still shown", name)
		}
		if d.held != 0 {
			t.Errorf("%s: %d bytes still counted as held", name, d.held)
		}
	}
}

func TestDVBSubDecoder_BottomFieldAbsentRepeatsTheTop(t *testing.T) {
	ds := decodeOneDVB(t, newDVBSub(t), concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, 4, 2, 2, 0, 0, dvbObjectRef{id: 7}),
		dvbTestCLUT, dvbODS(7, false, dvbTop4x2, nil), dvbEDS()))
	img := ds.Objects[0].Image
	for x := 0; x < 4; x++ {
		if nrgbaAt(t, img, x, 0) != nrgbaAt(t, img, x, 1) {
			t.Errorf("column %d: the bottom line is not the top one", x)
		}
	}
}

func TestDVBSubDecoder_InvisibleRegionIsNoObject(t *testing.T) {
	ds := decodeOneDVB(t, newDVBSub(t), concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, 64, 8, 2, 0, 0), dvbEDS()))
	if !ds.Clear() {
		t.Errorf("a region filled with the transparent entry gave %d objects, want a clear", len(ds.Objects))
	}
}

func TestDVBSubDecoder_RegionTrimmedToItsVisiblePixels(t *testing.T) {
	ds := decodeOneDVB(t, newDVBSub(t), concat(dvbPCS(2, dvbPlacement{x: 50, y: 60}),
		dvbRCS(0, true, 40, 10, 2, 0, 0, dvbObjectRef{id: 7, x: 20, y: 4}), dvbTestCLUT, dvbODS(7, false, dvbTop4x2, dvbBottom4x2), dvbEDS()))
	o := ds.Objects[0]
	if o.X != 70 || o.Y != 64 || o.Image.Bounds().Dx() != 4 || o.Image.Bounds().Dy() != 2 {
		t.Errorf("object at %d,%d of %v, want 70,64 of 4x2", o.X, o.Y, o.Image.Bounds().Size())
	}
}

func TestDVBSubDecoder_UndefinedRegionIsSkipped(t *testing.T) {
	ds := decodeOneDVB(t, newDVBSub(t), concat(dvbPCS(0, dvbPlacement{region: 9}), dvbEDS()))
	if !ds.Clear() {
		t.Errorf("a region never defined gave %d objects", len(ds.Objects))
	}
}

func TestDVBSubDecoder_DisplayDefinition(t *testing.T) {
	dds := func(p ...byte) []byte { return dvbSeg(dvbSegDisplay, dvbTestPage, p) }
	body := concat(dvbPCS(2, dvbPlacement{x: 100, y: 500}), dvbRCS(0, true, 4, 2, 2, 0, 0, dvbObjectRef{id: 7}), dvbTestCLUT,
		dvbODS(7, false, dvbTop4x2, dvbBottom4x2), dvbEDS())
	d := newDVBSub(t)
	ds := decodeOneDVB(t, d, concat(dds(0x00, 0x07, 0x7f, 0x04, 0x37), body))
	if ds.ScreenW != 1920 || ds.ScreenH != 1080 || ds.Objects[0].X != 100 {
		t.Errorf("plane %dx%d, object at x %d; want 1920x1080 and 100", ds.ScreenW, ds.ScreenH, ds.Objects[0].X)
	}
	// The definition holds for the display sets that follow, and a window shifts what the page places.
	if ds := decodeOneDVB(t, d, body); ds.ScreenW != 1920 {
		t.Errorf("plane width %d on the next display set, want 1920", ds.ScreenW)
	}
	ds = decodeOneDVB(t, d, concat(dds(0x08, 0x07, 0x7f, 0x04, 0x37, 0x00, 0x40, 0x07, 0x3f, 0x00, 0x20, 0x04, 0x17), body))
	if o := ds.Objects[0]; o.X != 164 || o.Y != 532 {
		t.Errorf("object at %d,%d inside a window at 64,32; want 164,532", o.X, o.Y)
	}
}

func TestDVBSubDecoder_OtherPagesAreAnotherService(t *testing.T) {
	foreign := dvbSeg(dvbSegPage, 0x0099, []byte{15, 2 << 2})
	d := newDVBSub(t)
	if sets, err := d.Decode(concat(foreign, dvbSeg(dvbSegEnd, 0x0099, nil))); err != nil || len(sets) != 0 {
		t.Errorf("another page's display set gave %d sets, err %v", len(sets), err)
	}
	// The ancillary page's CLUT and objects serve the composition page.
	clut := append([]byte(nil), dvbTestCLUT...)
	clut[2], clut[3] = byte(dvbTestAncillary>>8), byte(dvbTestAncillary&0xff)
	ds := decodeOneDVB(t, d, concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, 4, 2, 2, 0, 0, dvbObjectRef{id: 7}), clut,
		dvbODS(7, false, dvbTop4x2, dvbBottom4x2), dvbEDS()))
	if got := nrgbaAt(t, ds.Objects[0].Image, 0, 0); got != [4]uint8{255, 255, 255, 255} {
		t.Errorf("pixel = %v, want the white of the ancillary page's CLUT", got)
	}
	all := NewDVBSubDecoder(nil)
	if sets, _ := all.Decode(concat(foreign, dvbSeg(dvbSegEnd, 0x0099, nil))); len(sets) != 1 {
		t.Errorf("without CodecPrivate a page gave %d sets, want 1", len(sets))
	}
}

func TestDVBSubDecoder_DisplaySetBoundaries(t *testing.T) {
	show := dvbShow4x2()
	noEnd := show[:len(show)-len(dvbEDS())]
	for name, tc := range map[string]struct {
		block []byte
		sets  int
	}{
		"no end segment":            {noEnd, 1},
		"two sets":                  {concat(show, dvbPCS(0), dvbEDS()), 2},
		"two sets, no end segments": {concat(noEnd, dvbPCS(0)), 2},
		"PES header and end marker": {concat([]byte{0x20, 0x00}, show, []byte{0xff}), 1},
		"end without composition":   {dvbEDS(), 0},
		"segments of unknown types": {concat(dvbSeg(0x15, dvbTestPage, []byte{1, 2, 3}), show), 1},
	} {
		sets, err := newDVBSub(t).Decode(tc.block)
		if err != nil || len(sets) != tc.sets {
			t.Errorf("%s: %d sets, err %v; want %d", name, len(sets), err, tc.sets)
		}
	}
}

// dvbDraw draws one field into the top line of a fresh region and returns that line's CLUT entries.
func dvbDraw(t *testing.T, depth, w int, nonModifying bool, fill byte, data []byte) ([]byte, error) {
	t.Helper()
	r := &dvbRegion{w: w, h: 2, depth: depth, pix: bytes.Repeat([]byte{fill}, 2*w)}
	pen := dvbPen{d: &DVBSubDecoder{budget: 1 << 20}, r: r, skipOne: nonModifying}
	err := pen.field(data, 0, 0)
	return r.pix[:w], err
}

func dvbRuns(pairs ...int) []byte {
	var out []byte
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, bytes.Repeat([]byte{byte(pairs[i])}, pairs[i+1])...)
	}
	return out
}

func TestDVBSub_PixelCodeStrings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
		data  []byte
		want  []byte
	}{
		{"2-bit", 2,
			concat([]byte{dvbData2Bit}, dvbBitString("01 10 11  0001  000001  00 1 010 11  0000 10 0011 10  0000 11 00000001 01  000000")),
			dvbRuns(1, 1, 2, 1, 3, 1, 0, 1, 0, 2, 3, 5, 2, 15, 1, 30)},
		{"4-bit", 4,
			concat([]byte{dvbData4Bit}, dvbBitString("0001 1111  0000 1100  0000 1101  0000 0 011  0000 10 01 0111  0000 1110 0010 0101  0000 1111 00000011 1001  0000 0000")),
			dvbRuns(1, 1, 15, 1, 0, 1, 0, 2, 0, 5, 7, 5, 5, 11, 9, 28)},
		{"8-bit", 8,
			[]byte{dvbData8Bit, 0x41, 0x00, 0x05, 0x00, 0x83, 0x99, 0x00, 0x00},
			dvbRuns(0x41, 1, 0, 5, 0x99, 3)},
		{"two strings on one line", 4,
			[]byte{dvbData4Bit, 0x12, 0x00, dvbData4Bit, 0x34, 0x00},
			[]byte{1, 2, 3, 4}},
		{"2-bit codes in a 4-bit region", 4,
			concat([]byte{dvbData2Bit}, dvbBitString("01 10 11 000000")),
			[]byte{0x7, 0x8, 0xf}},
		{"2-bit codes through a sent 2-to-4 table", 4,
			concat([]byte{dvbMap2To4, 0x12, 0x34, dvbData2Bit}, dvbBitString("01 10 11 000000")),
			[]byte{2, 3, 4}},
		{"2-bit codes in an 8-bit region", 8,
			concat([]byte{dvbData2Bit}, dvbBitString("01 10 11 000000")),
			[]byte{0x77, 0x88, 0xff}},
		{"2-bit codes through a sent 2-to-8 table", 8,
			concat([]byte{dvbMap2To8, 9, 10, 11, 12, dvbData2Bit}, dvbBitString("01 10 11 000000")),
			[]byte{10, 11, 12}},
		{"4-bit codes in an 8-bit region", 8,
			[]byte{dvbData4Bit, 0x1f, 0x00},
			[]byte{0x11, 0xff}},
		{"4-bit codes through a sent 4-to-8 table", 8,
			concat([]byte{dvbMap4To8}, bytes.Repeat([]byte{0x42}, 16), []byte{dvbData4Bit, 0x1f, 0x00}),
			[]byte{0x42, 0x42}},
		{"a string cut short ends there", 4,
			[]byte{dvbData4Bit, 0x12},
			[]byte{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dvbDraw(t, tc.depth, len(tc.want), false, 0, tc.data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("line  %v\nwant  %v", got, tc.want)
			}
		})
	}
}

func TestDVBSub_NonModifyingColourAndClipping(t *testing.T) {
	got, err := dvbDraw(t, 4, 3, true, 9, []byte{dvbData4Bit, 0x12, 0x10, 0x00})
	if err != nil || !bytes.Equal(got, []byte{9, 2, 9}) {
		t.Errorf("line %v, err %v; want code 1 to leave the region's 9", got, err)
	}
	// A run past the region's edge stops at it, and lines below the region are not drawn.
	data := concat([]byte{dvbData4Bit}, dvbBitString("0000 1111 11111111 0011 0000 0000"), []byte{dvbDataEndRow, dvbData4Bit, 0x55, 0x00, dvbDataEndRow, dvbData4Bit, 0x66, 0x00})
	got, err = dvbDraw(t, 4, 5, false, 0, data)
	if err != nil || !bytes.Equal(got, []byte{3, 3, 3, 3, 3}) {
		t.Errorf("line %v, err %v; want the run clipped to the 5 pixels of the region", got, err)
	}
}

func TestDVBSub_PixelDataRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		depth int
		data  []byte
		want  string
	}{
		"4-bit string in a 2-bit region": {2, []byte{dvbData4Bit, 0x10, 0x00}, "a 4-bit pixel string in a 2-bit region"},
		"8-bit string in a 4-bit region": {4, []byte{dvbData8Bit, 0x10, 0x00, 0x00}, "an 8-bit pixel string in a 4-bit region"},
		"map table cut short":            {8, []byte{dvbMap4To8, 1, 2, 3}, "a map table of 16 bytes, 3 remain"},
	} {
		if _, err := dvbDraw(t, tc.depth, 4, false, 0, tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

func TestDVBSub_CLUT(t *testing.T) {
	d := newDVBSub(t)
	// Entry 5 of the 16-entry table in reduced range, entry 6 of all three tables, entry 7 with Y 0.
	reduced := dvbSeg(dvbSegCLUT, dvbTestPage, []byte{3, 0, 5, 0x40, 0xeb<<2&0xfc | 0x02, 0x00<<6 | 0x08<<2 | 0x01})
	full := dvbCDS(3, [6]byte{6, 0xe1, 235, 128, 128, 0x40}, [6]byte{7, 0x41, 0, 128, 128, 0})
	if _, err := d.Decode(concat(reduced, full)); err != nil {
		t.Fatal(err)
	}
	c := d.cluts[3]
	r, g, b := ycrcbToRGB(0xeb<<2&0xfc, 0x80, 0x80)
	if c.c16[5] != [4]uint8{r, g, b, 255 - 0x40} {
		t.Errorf("reduced-range entry = %v, want %v", c.c16[5], [4]uint8{r, g, b, 255 - 0x40})
	}
	if want := [4]uint8{255, 255, 255, 255 - 0x40}; c.c16[6] != want || c.c256[6] != want {
		t.Errorf("entry 6 = %v and %v, want %v in both tables", c.c16[6], c.c256[6], want)
	}
	if c.c4[2] != dvbDefaultCLUT.c4[2] {
		t.Errorf("entry 6 of a 4-entry table changed entry %v", c.c4)
	}
	if c.c16[7] != [4]uint8{} {
		t.Errorf("an entry of Y 0 = %v, want fully transparent", c.c16[7])
	}
	if c.c16[1] != dvbDefaultCLUT.c16[1] {
		t.Errorf("an entry the definition did not carry lost its default")
	}
}

// The defaults of clause 10, one entry of each of its cases.
func TestDVBSub_DefaultCLUT(t *testing.T) {
	c := dvbDefaultCLUT
	for name, tc := range map[string]struct{ got, want [4]uint8 }{
		"4: transparent":           {c.c4[0], [4]uint8{}},
		"4: white":                 {c.c4[1], [4]uint8{255, 255, 255, 255}},
		"4: black":                 {c.c4[2], [4]uint8{0, 0, 0, 255}},
		"4: grey":                  {c.c4[3], [4]uint8{127, 127, 127, 255}},
		"16: transparent":          {c.c16[0], [4]uint8{}},
		"16: red":                  {c.c16[1], [4]uint8{255, 0, 0, 255}},
		"16: white":                {c.c16[7], [4]uint8{255, 255, 255, 255}},
		"16: black":                {c.c16[8], [4]uint8{0, 0, 0, 255}},
		"16: half blue":            {c.c16[12], [4]uint8{0, 0, 127, 255}},
		"256: transparent":         {c.c256[0], [4]uint8{}},
		"256: red at 75 %":         {c.c256[1], [4]uint8{255, 0, 0, 63}},
		"256: full red":            {c.c256[0x11], [4]uint8{255, 0, 0, 255}},
		"256: two thirds of green": {c.c256[0x20], [4]uint8{0, 170, 0, 255}},
		"256: half transparent":    {c.c256[0x4c], [4]uint8{0, 0, 255, 127}},
		"256: light":               {c.c256[0x91], [4]uint8{255, 127, 127, 255}},
		"256: dark":                {c.c256[0x99], [4]uint8{128, 0, 0, 255}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", name, tc.got, tc.want)
		}
	}
}

func TestDVBSubDecoder_Malformed(t *testing.T) {
	seg := func(typ byte, p ...byte) []byte { return dvbSeg(typ, dvbTestPage, p) }
	for name, tc := range map[string]struct {
		block []byte
		want  string
	}{
		"short header":            {[]byte{dvbSync, 0x10, 0}, "a segment header needs 6"},
		"segment past the block":  {[]byte{dvbSync, 0x10, 0, 1, 0, 9, 0}, "declares 9 bytes"},
		"page cut short":          {seg(dvbSegPage, 15), "page composition segment holds 1 bytes"},
		"page with half a region": {seg(dvbSegPage, 15, 0, 1, 0, 0), "page composition segment holds 5 bytes"},
		"region cut short":        {seg(dvbSegRegion, 0, 0, 0, 4), "region composition segment needs 10"},
		"region of no width":      {dvbRCS(0, true, 0, 2, 2, 0, 0), "outside 1..4096"},
		"region too tall":         {dvbRCS(0, true, 4, 5000, 2, 0, 0), "outside 1..4096"},
		"region of no depth":      {dvbRCS(0, true, 4, 2, 0, 0, 0), "pixel depth code 0"},
		"half an object ref":      {dvbGrow(dvbRCS(0, true, 4, 2, 2, 0, 0), 1, 2, 3), "of an object reference of 6"},
		"CLUT cut short":          {seg(dvbSegCLUT, 0), "CLUT definition segment needs 2"},
		"CLUT entry cut short":    {seg(dvbSegCLUT, 0, 0, 1, 0x41, 235, 128), "of an entry of 6"},
		"object cut short":        {seg(dvbSegObject, 0, 7), "object data segment needs 3"},
		"object without lengths":  {seg(dvbSegObject, 0, 7, 0, 0, 5), "declares no field lengths"},
		"object fields too long":  {seg(dvbSegObject, 0, 7, 0, 0, 5, 0, 5, 1, 2, 3), "declares 10 bytes of fields"},
		"display cut short":       {seg(dvbSegDisplay, 0, 7, 0x7f), "display definition segment needs 5"},
		"display window missing":  {seg(dvbSegDisplay, 8, 7, 0x7f, 4, 0x37, 0, 0), "declares a window"},
		"bad object data": {concat(dvbPCS(2), dvbRCS(0, true, 4, 2, 1, 0, 0, dvbObjectRef{id: 7}), dvbODS(7, false, dvbTop4x2, nil)),
			"object 7: a 4-bit pixel string in a 2-bit region"},
	} {
		sets, err := newDVBSub(t).Decode(tc.block)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %d sets, err = %v, want one naming %q", name, len(sets), err, tc.want)
		}
	}
}

// Objects that are not pictures (characters, a coding the format added later) are passed over.
func TestDVBSubDecoder_NonPixelObjectsIgnored(t *testing.T) {
	chars := dvbSeg(dvbSegObject, dvbTestPage, []byte{0, 7, 1 << 2, 1, 0, 0x41})
	region := dvbGrow(dvbRCS(0, true, 4, 2, 2, 0, 0), 0, 7, 0x40, 0, 0, 0, 1, 2) // a character object reference, two bytes longer
	ds := decodeOneDVB(t, newDVBSub(t), concat(dvbPCS(2, dvbPlacement{}), region, chars, dvbEDS()))
	if !ds.Clear() {
		t.Errorf("a character object drew %d objects", len(ds.Objects))
	}
}

// A few bytes naming a full-size region to fill again and again are stopped by the block's budget.
func TestDVBSubBlockPixelsBounded(t *testing.T) {
	fill := dvbRCS(0, true, dvbMaxDim, dvbMaxDim, 3, 0, 1)
	d := newDVBSub(t)
	if _, err := d.Decode(concat(fill, fill)); err != nil {
		t.Fatalf("two fills of a full-size region: %v", err)
	}
	_, err := d.Decode(concat(fill, fill, fill))
	if err == nil || !strings.Contains(err.Error(), "fills and draws more than") {
		t.Fatalf("err = %v, want the block's pixel bound", err)
	}
	if _, err := d.Decode(fill); err != nil {
		t.Errorf("the block after a refused one: %v", err)
	}
}

func TestDVBSubEpochRegionsBounded(t *testing.T) {
	d := newDVBSub(t)
	if _, err := d.Decode(dvbRCS(0, false, dvbMaxDim, dvbMaxDim, 3, 0, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := d.Decode(dvbRCS(1, false, 64, 64, 3, 0, 0))
	if err == nil || !strings.Contains(err.Error(), "brings the regions held since the last mode change") {
		t.Fatalf("err = %v, want the epoch bound", err)
	}
	// Defining the first region again, smaller, gives its bytes back.
	if _, err := d.Decode(concat(dvbRCS(0, false, 64, 64, 3, 0, 0), dvbRCS(1, false, 64, 64, 3, 0, 0))); err != nil {
		t.Fatalf("after shrinking region 0: %v", err)
	}
	if d.held != 2*64*64 {
		t.Errorf("held = %d, want %d", d.held, 2*64*64)
	}
}

// An object a region lists thousands of times costs its data each time, and the block's budget counts it.
func TestDVBSubObjectListedManyTimesBounded(t *testing.T) {
	refs := make([]dvbObjectRef, 10000)
	for i := range refs {
		refs[i].id = 7
	}
	long := concat([]byte{dvbData4Bit}, dvbBitString("0000 1111 11111111 0001 0000 0000"), []byte{dvbDataEndRow})
	top := bytes.Repeat(long, 200)
	block := concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, 64, 64, 2, 0, 0, refs...))
	for i := 0; i < 8; i++ {
		block = append(block, dvbODS(7, false, top, nil)...)
	}
	_, err := newDVBSub(t).Decode(block)
	if err == nil || !strings.Contains(err.Error(), "fills and draws more than") {
		t.Fatalf("err = %v, want the block's pixel bound", err)
	}
}

// A page may place one region many times: it is read through its CLUT once per display set, the placements sharing
// the picture. 32 placements of a 1024x1024 region each made a picture of 4 MiB.
func TestDVBSubRegionRenderedOncePerDisplaySet(t *testing.T) {
	at := make([]dvbPlacement, 32)
	for i := range at {
		at[i] = dvbPlacement{region: 0, x: i, y: 2 * i}
	}
	block := concat(dvbPCS(2, at...), dvbRCS(0, true, 1024, 1024, 3, 0, 1), dvbEDS())
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	sets, err := newDVBSub(t).Decode(block)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Objects) != 32 {
		t.Fatalf("%d display sets, want one placing the region 32 times", len(sets))
	}
	for i, o := range sets[0].Objects {
		if o.X != i || o.Y != 2*i || o.Image != sets[0].Objects[0].Image {
			t.Fatalf("placement %d at %d,%d with its own picture; want %d,%d sharing the first", i, o.X, o.Y, i, 2*i)
		}
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 12<<20 {
		t.Errorf("32 placements of one 1024x1024 region allocated %d MiB, want the region and one picture, 5 MiB", alloc>>20)
	}
}

// The budget counts a region once per display set too: 8 placements of a full-size region are one picture.
func TestDVBSubPlacementsOfOneRegionCostOnePicture(t *testing.T) {
	at := make([]dvbPlacement, 8)
	block := concat(dvbPCS(2, at...), dvbRCS(0, true, dvbMaxDim, dvbMaxDim, 3, 0, 1), dvbEDS())
	if _, err := newDVBSub(t).Decode(block); err != nil {
		t.Fatalf("8 placements of one region: %v", err)
	}
}

func TestNewDVBSubDecoder_ShortCodecPrivateDecodesEveryPage(t *testing.T) {
	for n := 1; n <= 3; n++ {
		d := NewDVBSubDecoder(make([]byte, n))
		if sets, err := d.Decode(concat(dvbSeg(dvbSegPage, 0x0099, []byte{15, 2 << 2}), dvbSeg(dvbSegEnd, 0x0099, nil))); err != nil || len(sets) != 1 {
			t.Errorf("%d byte(s): a page gave %d sets, err %v; want it decoded", n, len(sets), err)
		}
	}
}

// Bytes that are no segment end the block: the display set composed ahead of them is given, without an error.
func TestDVBSubDecoder_JunkAfterTheSegments(t *testing.T) {
	show := dvbShow4x2()
	noEnd := show[:len(show)-len(dvbEDS())]
	for name, block := range map[string][]byte{
		"after the end segment": concat(show, []byte{0x47, 0x1f, 0xff, 0x10}),
		"in place of it":        concat(noEnd, []byte{0x00, 0x00}),
	} {
		sets, err := newDVBSub(t).Decode(block)
		if err != nil || len(sets) != 1 || len(sets[0].Objects) != 1 {
			t.Errorf("%s: %d sets, err %v; want the display set", name, len(sets), err)
		}
	}
}

// A pixel-data block of a type the format does not define ends its field: what was drawn ahead of it stays.
func TestDVBSubDecoder_UnknownPixelDataEndsTheField(t *testing.T) {
	top := concat(dvbTop4x2, []byte{0x33, 0x01, 0x02})
	ds := decodeOneDVB(t, newDVBSub(t), concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, 4, 2, 2, 0, 0, dvbObjectRef{id: 7}),
		dvbTestCLUT, dvbODS(7, false, top, dvbBottom4x2), dvbEDS()))
	if len(ds.Objects) != 1 || ds.Objects[0].Image.Bounds().Dy() != 2 {
		t.Fatalf("%d objects; want the picture, both lines drawn", len(ds.Objects))
	}
}

// A block refused part-way gives back, with the error, the display sets it had completed and the one it was composing.
func TestDVBSubDecoder_ErrorKeepsTheSetsAheadOfIt(t *testing.T) {
	bad := dvbSeg(dvbSegRegion, dvbTestPage, []byte{0, 0, 0, 4})
	d := newDVBSub(t)
	sets, err := d.Decode(concat(dvbShow4x2(), dvbPCS(0, dvbPlacement{x: 100, y: 500}), bad, dvbEDS()))
	if err == nil || !strings.Contains(err.Error(), "region composition segment needs") {
		t.Fatalf("err = %v, want the region's", err)
	}
	if len(sets) != 2 || len(sets[0].Objects) != 1 || len(sets[1].Objects) != 1 {
		t.Fatalf("%d sets with the error; want the completed one and the one being composed", len(sets))
	}
	// The next block starts from the state the error left, which is the one the sets showed.
	if ds := decodeOneDVB(t, d, concat(dvbPCS(0, dvbPlacement{x: 100, y: 500}), dvbEDS())); len(ds.Objects) != 1 {
		t.Errorf("the block after the error shows %d objects, want the region still held", len(ds.Objects))
	}
}

// The page's time-out is given as it is, a bound on how long the set stays without a successor, not its duration.
func TestDVBSubDecoder_Timeout(t *testing.T) {
	d := newDVBSub(t)
	ds := decodeOneDVB(t, d, dvbShow4x2())
	if ds.Timeout != 15*time.Second || ds.Duration != 0 || ds.Delay != 0 {
		t.Errorf("timeout %v duration %v delay %v; want the page's 15 s and no duration", ds.Timeout, ds.Duration, ds.Delay)
	}
	short := dvbSeg(dvbSegPage, dvbTestPage, []byte{3, 0})
	if ds := decodeOneDVB(t, d, concat(short, dvbEDS())); ds.Timeout != 3*time.Second {
		t.Errorf("timeout %v after a page of 3 s", ds.Timeout)
	}
}
