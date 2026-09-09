package subtitle

import (
	"image"
	"strings"
	"testing"
)

// --- synthetic PGS builders ---------------------------------------------
//
// Everything below builds display sets byte by byte, so the tests pin the wire
// format rather than whatever the decoder happens to accept.

func pgsSegment(typ byte, payload []byte) []byte {
	out := []byte{typ, byte(len(payload) >> 8), byte(len(payload))}
	return append(out, payload...)
}

type synthObject struct {
	id     uint16
	x, y   int
	forced bool
	crop   *image.Rectangle
}

func pgsPCS(w, h int, state byte, paletteID uint8, objs ...synthObject) []byte {
	p := []byte{
		byte(w >> 8), byte(w), byte(h >> 8), byte(h),
		0x10,       // frame rate
		0x00, 0x00, // composition number
		state,
		0x00,      // palette update flag
		paletteID, // palette ID
		byte(len(objs)),
	}
	for _, o := range objs {
		flags := byte(0)
		if o.forced {
			flags |= pgsObjectForced
		}
		if o.crop != nil {
			flags |= pgsObjectCropped
		}
		p = append(p,
			byte(o.id>>8), byte(o.id),
			0x00, // window ID
			flags,
			byte(o.x>>8), byte(o.x), byte(o.y>>8), byte(o.y),
		)
		if o.crop != nil {
			c := *o.crop
			p = append(p,
				byte(c.Min.X>>8), byte(c.Min.X), byte(c.Min.Y>>8), byte(c.Min.Y),
				byte(c.Dx()>>8), byte(c.Dx()), byte(c.Dy()>>8), byte(c.Dy()),
			)
		}
	}
	return pgsSegment(pgsSegPCS, p)
}

func pgsWDS(x, y, w, h int) []byte {
	return pgsSegment(pgsSegWDS, []byte{
		0x01, 0x00,
		byte(x >> 8), byte(x), byte(y >> 8), byte(y),
		byte(w >> 8), byte(w), byte(h >> 8), byte(h),
	})
}

// pgsPDS builds a palette definition; entries are index, Y, Cr, Cb, alpha.
func pgsPDS(id uint8, entries ...[5]byte) []byte {
	p := []byte{id, 0x00}
	for _, e := range entries {
		p = append(p, e[:]...)
	}
	return pgsSegment(pgsSegPDS, p)
}

func pgsODS(id uint16, w, h int, rle []byte) []byte {
	dataLen := len(rle) + 4 // the length field counts width and height too
	p := []byte{
		byte(id >> 8), byte(id),
		0x00, // version
		0xc0, // first and last in sequence
		byte(dataLen >> 16), byte(dataLen >> 8), byte(dataLen),
		byte(w >> 8), byte(w), byte(h >> 8), byte(h),
	}
	return pgsSegment(pgsSegODS, append(p, rle...))
}

// pgsODSSplit sends one object over two segments, as a real stream does when the
// picture does not fit a single segment.
func pgsODSSplit(id uint16, w, h int, rle []byte, at int) [][]byte {
	dataLen := len(rle) + 4
	first := []byte{
		byte(id >> 8), byte(id), 0x00,
		0x80, // first, not last
		byte(dataLen >> 16), byte(dataLen >> 8), byte(dataLen),
		byte(w >> 8), byte(w), byte(h >> 8), byte(h),
	}
	rest := []byte{byte(id >> 8), byte(id), 0x00, 0x40} // last
	return [][]byte{
		pgsSegment(pgsSegODS, append(first, rle[:at]...)),
		pgsSegment(pgsSegODS, append(rest, rle[at:]...)),
	}
}

func pgsEND() []byte { return pgsSegment(pgsSegEND, nil) }

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// White and a saturated blue, as a disc encodes them: limited-range BT.601.
var (
	palWhite = [5]byte{1, 235, 128, 128, 255}
	palBlue  = [5]byte{2, 82, 90, 240, 255}
)

// rle4x2 paints a 4x2 object: the top line is all colour 1, the bottom line is
// two transparent pixels then two of colour 2.
var rle4x2 = []byte{
	0x00, 0x84, 0x01, // 4 pixels of colour 1
	0x00, 0x00, // end of line
	0x00, 0x02, // 2 pixels of colour 0 (transparent)
	0x00, 0x82, 0x02, // 2 pixels of colour 2
	0x00, 0x00, // end of line
}

func nrgbaAt(t *testing.T, img *image.NRGBA, x, y int) [4]uint8 {
	t.Helper()
	off := img.PixOffset(x, y)
	return [4]uint8{img.Pix[off], img.Pix[off+1], img.Pix[off+2], img.Pix[off+3]}
}

// --- tests ---------------------------------------------------------------

// The whole shape of the format in one stream: two shown display sets, each
// followed by the empty composition that clears it.
func TestPGSDecoder_TwoCues(t *testing.T) {
	d := NewPGSDecoder()

	show1 := concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 10, x: 100, y: 900}),
		pgsWDS(100, 900, 4, 2),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(10, 4, 2, rle4x2),
		pgsEND(),
	)
	clear := concat(pgsPCS(1920, 1080, 0, 0), pgsEND())
	show2 := concat(
		pgsPCS(1920, 1080, 0, 0, synthObject{id: 11, x: 200, y: 950, forced: true}),
		pgsODS(11, 4, 2, rle4x2),
		pgsEND(),
	)

	sets1, err := d.Decode(show1)
	if err != nil {
		t.Fatalf("Decode(show1): %v", err)
	}
	if len(sets1) != 1 {
		t.Fatalf("show1 decoded %d display sets, want 1", len(sets1))
	}
	ds := sets1[0]
	if ds.Clear() {
		t.Fatal("a display set with one composition object reports Clear")
	}
	if ds.ScreenW != 1920 || ds.ScreenH != 1080 {
		t.Errorf("screen = %dx%d, want 1920x1080", ds.ScreenW, ds.ScreenH)
	}
	if len(ds.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(ds.Objects))
	}
	o := ds.Objects[0]
	if o.X != 100 || o.Y != 900 {
		t.Errorf("object at (%d,%d), want (100,900)", o.X, o.Y)
	}
	if o.Forced {
		t.Error("object reports Forced, the composition did not set the flag")
	}
	if got := o.Image.Bounds(); got != image.Rect(0, 0, 4, 2) {
		t.Fatalf("image bounds %v, want 4x2 at the origin", got)
	}
	// Limited-range BT.601: a full-range conversion would give (29,109,255) for
	// the blue, so these exact values are what pins the range expansion.
	if got, want := nrgbaAt(t, o.Image, 0, 0), [4]uint8{255, 255, 255, 255}; got != want {
		t.Errorf("top-left pixel = %v, want opaque white %v", got, want)
	}
	if got, want := nrgbaAt(t, o.Image, 0, 1), [4]uint8{0, 0, 0, 0}; got != want {
		t.Errorf("an unpainted pixel = %v, want fully transparent %v", got, want)
	}
	if got, want := nrgbaAt(t, o.Image, 2, 1), [4]uint8{16, 64, 255, 255}; got != want {
		t.Errorf("bottom blue pixel = %v, want %v (limited-range BT.601)", got, want)
	}

	sets2, err := d.Decode(clear)
	if err != nil {
		t.Fatalf("Decode(clear): %v", err)
	}
	if len(sets2) != 1 || !sets2[0].Clear() {
		t.Fatalf("the empty composition did not decode to one clearing display set: %+v", sets2)
	}

	// show2 defines no palette: it must reuse the epoch's, which is exactly the
	// state the decoder exists to carry across blocks.
	sets3, err := d.Decode(show2)
	if err != nil {
		t.Fatalf("Decode(show2): %v", err)
	}
	if len(sets3) != 1 || len(sets3[0].Objects) != 1 {
		t.Fatalf("show2 decoded %+v", sets3)
	}
	o2 := sets3[0].Objects[0]
	if !o2.Forced {
		t.Error("the forced flag was not carried through")
	}
	if got, want := nrgbaAt(t, o2.Image, 0, 0), [4]uint8{255, 255, 255, 255}; got != want {
		t.Errorf("second cue top-left = %v, want %v - the epoch palette was lost", got, want)
	}
}

// An object defined once must stay usable by later display sets of the same
// epoch, and must be gone the moment a new epoch starts.
func TestPGSDecoder_EpochStartClearsState(t *testing.T) {
	d := NewPGSDecoder()
	if _, err := d.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 10}),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(10, 4, 2, rle4x2),
		pgsEND(),
	)); err != nil {
		t.Fatal(err)
	}
	// Same epoch, object not re-sent: it is still there.
	sets, err := d.Decode(concat(pgsPCS(1920, 1080, 0, 0, synthObject{id: 10, x: 5}), pgsEND()))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets[0].Objects) != 1 {
		t.Fatal("an object of the current epoch was dropped")
	}
	// New epoch, object not re-sent: it must NOT be reused, or a seek would
	// paint a subtitle from the wrong part of the film.
	sets, err = d.Decode(concat(pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 10, x: 5}), pgsEND()))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets[0].Objects) != 0 {
		t.Errorf("an epoch start kept %d object(s) from the previous epoch", len(sets[0].Objects))
	}
}

// Reset is what a caller starting mid-track uses; it must behave like an epoch
// start.
func TestPGSDecoder_Reset(t *testing.T) {
	d := NewPGSDecoder()
	if _, err := d.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 10}),
		pgsPDS(0, palWhite),
		pgsODS(10, 4, 2, rle4x2),
		pgsEND(),
	)); err != nil {
		t.Fatal(err)
	}
	d.Reset()
	sets, err := d.Decode(concat(pgsPCS(1920, 1080, 0, 0, synthObject{id: 10}), pgsEND()))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets[0].Objects) != 0 {
		t.Error("Reset left objects behind")
	}
}

// A picture too big for one segment arrives in several; the reassembled object
// must be identical to the single-segment one.
func TestPGSDecoder_ObjectSplitOverSegments(t *testing.T) {
	whole := NewPGSDecoder()
	one, err := whole.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 7}),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(7, 4, 2, rle4x2),
		pgsEND(),
	))
	if err != nil {
		t.Fatal(err)
	}

	split := NewPGSDecoder()
	parts := pgsODSSplit(7, 4, 2, rle4x2, 5)
	many, err := split.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 7}),
		pgsPDS(0, palWhite, palBlue),
		parts[0], parts[1],
		pgsEND(),
	))
	if err != nil {
		t.Fatalf("split object: %v", err)
	}
	a, b := one[0].Objects[0].Image, many[0].Objects[0].Image
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds differ: %v vs %v", a.Bounds(), b.Bounds())
	}
	if string(a.Pix) != string(b.Pix) {
		t.Error("an object split over two segments decodes to different pixels")
	}
}

func TestPGSDecoder_Crop(t *testing.T) {
	d := NewPGSDecoder()
	crop := image.Rect(2, 1, 4, 2) // the bottom-right 2x1 of the 4x2 object
	sets, err := d.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1, x: 30, y: 40, crop: &crop}),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(1, 4, 2, rle4x2),
		pgsEND(),
	))
	if err != nil {
		t.Fatal(err)
	}
	o := sets[0].Objects[0]
	if got := o.Image.Bounds(); got != image.Rect(0, 0, 2, 1) {
		t.Fatalf("cropped bounds %v, want 2x1 at the origin", got)
	}
	if o.X != 30 || o.Y != 40 {
		t.Errorf("cropping moved the object to (%d,%d), the composition said (30,40)", o.X, o.Y)
	}
	if got, want := nrgbaAt(t, o.Image, 0, 0), [4]uint8{16, 64, 255, 255}; got != want {
		t.Errorf("cropped pixel = %v, want the blue %v", got, want)
	}
}

// A crop rectangle that runs past the object is clipped to it rather than read
// out of bounds; one that lands entirely outside drops the object.
func TestPGSDecoder_CropOutOfBounds(t *testing.T) {
	build := func(crop image.Rectangle) []PGSDisplaySet {
		t.Helper()
		d := NewPGSDecoder()
		sets, err := d.Decode(concat(
			pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1, x: 7, y: 8, crop: &crop}),
			pgsPDS(0, palWhite, palBlue),
			pgsODS(1, 4, 2, rle4x2),
			pgsEND(),
		))
		if err != nil {
			t.Fatalf("crop %v: %v", crop, err)
		}
		return sets
	}

	sets := build(image.Rect(1, 0, 40, 20)) // overruns the 4x2 object
	if len(sets[0].Objects) != 1 {
		t.Fatalf("the object was dropped by an overrunning crop")
	}
	if got := sets[0].Objects[0].Image.Bounds(); got != image.Rect(0, 0, 3, 2) {
		t.Errorf("clipped crop bounds %v, want 3x2", got)
	}
	if got, want := nrgbaAt(t, sets[0].Objects[0].Image, 0, 0), [4]uint8{255, 255, 255, 255}; got != want {
		t.Errorf("clipped crop top-left = %v, want %v", got, want)
	}

	if sets := build(image.Rect(100, 100, 104, 102)); len(sets[0].Objects) != 0 {
		t.Errorf("a crop entirely outside the object kept %d object(s)", len(sets[0].Objects))
	}
}

// A composition may name an object that was defined before the caller started
// reading. Skipping it keeps the rest of the display set; failing would lose the
// whole track.
func TestPGSDecoder_UndefinedObjectIsSkipped(t *testing.T) {
	d := NewPGSDecoder()
	sets, err := d.Decode(concat(
		pgsPCS(1920, 1080, 0, 0, synthObject{id: 99, x: 1, y: 2}),
		pgsEND(),
	))
	if err != nil {
		t.Fatalf("an undefined object must not be an error: %v", err)
	}
	if len(sets) != 1 || len(sets[0].Objects) != 0 {
		t.Errorf("got %+v, want one display set with no object", sets)
	}
}

// Some muxers leave the END segment out; the display set still has to come out.
func TestPGSDecoder_MissingEndSegment(t *testing.T) {
	d := NewPGSDecoder()
	sets, err := d.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1}),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(1, 4, 2, rle4x2),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].Objects) != 1 {
		t.Fatalf("got %+v, want the display set flushed at the end of the block", sets)
	}
}

// A block holding several display sets must yield them all - dropping the extras
// would silently lose cues.
func TestPGSDecoder_SeveralDisplaySetsInOneBlock(t *testing.T) {
	d := NewPGSDecoder()
	sets, err := d.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1}),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(1, 4, 2, rle4x2),
		pgsEND(),
		pgsPCS(1920, 1080, 0, 0),
		pgsEND(),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 {
		t.Fatalf("got %d display sets, want 2", len(sets))
	}
	if sets[0].Clear() || !sets[1].Clear() {
		t.Errorf("got Clear() = %v, %v; want false, true", sets[0].Clear(), sets[1].Clear())
	}
}

// An unknown segment type is skipped by its declared size, so a future extension
// cannot desynchronize the parse.
func TestPGSDecoder_UnknownSegmentSkipped(t *testing.T) {
	d := NewPGSDecoder()
	sets, err := d.Decode(concat(
		pgsSegment(0x42, []byte{1, 2, 3, 4}),
		pgsPCS(1920, 1080, pgsEpochStart, 0),
		pgsEND(),
	))
	if err != nil {
		t.Fatalf("an unknown segment must be skipped, not fatal: %v", err)
	}
	if len(sets) != 1 {
		t.Fatalf("got %d display sets, want 1", len(sets))
	}
}

func TestPGSDecoder_Malformed(t *testing.T) {
	cases := []struct {
		name  string
		block []byte
		want  string
	}{
		{"truncated header", []byte{0x16, 0x00}, "segment header"},
		{"segment overruns block", []byte{0x16, 0xff, 0xff, 0x01}, "remain in the block"},
		{"short composition", pgsSegment(pgsSegPCS, []byte{0, 1, 2}), "composition segment needs"},
		{"composition object count lies", pgsSegment(pgsSegPCS,
			[]byte{0x07, 0x80, 0x04, 0x38, 0x10, 0, 0, 0x80, 0, 0, 0x02}), "declares 2 objects"},
		{"palette entries not a multiple of 5", pgsSegment(pgsSegPDS, []byte{0, 0, 1, 2, 3}), "not a multiple of 5"},
		{"window count lies", pgsSegment(pgsSegWDS, []byte{0x03, 0x00}), "window definition declares"},
		{"object continues nothing", pgsSegment(pgsSegODS, []byte{0, 5, 0, 0x40, 0xaa}), "never started"},
		{"object dimensions out of range", pgsSegment(pgsSegODS,
			[]byte{0, 1, 0, 0xc0, 0, 0, 8, 0xff, 0xff, 0x00, 0x10}), "outside 1..4096"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewPGSDecoder()
			_, err := d.Decode(tc.block)
			if err == nil {
				t.Fatalf("no error; want one mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A run that overflows its line continues on the next one rather than failing:
// the picture a decoder shows must not depend on an encoder remembering every
// end-of-line marker.
func TestPGSDecoder_RunWrapsAcrossLines(t *testing.T) {
	d := NewPGSDecoder()
	// Six pixels of colour 1 into a 4x2 object, with no end-of-line marker.
	sets, err := d.Decode(concat(
		pgsPCS(100, 100, pgsEpochStart, 0, synthObject{id: 1}),
		pgsPDS(0, palWhite),
		pgsODS(1, 4, 2, []byte{0x00, 0x86, 0x01}),
		pgsEND(),
	))
	if err != nil {
		t.Fatal(err)
	}
	img := sets[0].Objects[0].Image
	if got, want := nrgbaAt(t, img, 3, 0), [4]uint8{255, 255, 255, 255}; got != want {
		t.Errorf("end of the first line = %v, want %v", got, want)
	}
	if got, want := nrgbaAt(t, img, 1, 1), [4]uint8{255, 255, 255, 255}; got != want {
		t.Errorf("the run did not wrap: pixel (1,1) = %v, want %v", got, want)
	}
	if got, want := nrgbaAt(t, img, 2, 1), [4]uint8{0, 0, 0, 0}; got != want {
		t.Errorf("the run wrapped too far: pixel (2,1) = %v, want %v", got, want)
	}
}

// Every run form the encoding has, and every way one can be cut short. The long
// forms are what a wide subtitle line actually uses, so they must not be
// reachable only in theory.
func TestPGSDecoder_RunLengthForms(t *testing.T) {
	decode := func(t *testing.T, w, h int, rle []byte) (*image.NRGBA, error) {
		t.Helper()
		d := NewPGSDecoder()
		sets, err := d.Decode(concat(
			pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1}),
			pgsPDS(0, palWhite, palBlue),
			pgsODS(1, w, h, rle),
			pgsEND(),
		))
		if err != nil {
			return nil, err
		}
		return sets[0].Objects[0].Image, nil
	}

	t.Run("long coloured run", func(t *testing.T) {
		// 0xc0 | 0x00, then 0x14 -> a 20-pixel run of colour 1.
		img, err := decode(t, 20, 2, []byte{0x00, 0xc0, 0x14, 0x01, 0x00, 0x00, 0x00, 0x00})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range []int{0, 19} {
			if got, want := nrgbaAt(t, img, x, 0), [4]uint8{255, 255, 255, 255}; got != want {
				t.Errorf("pixel (%d,0) = %v, want %v", x, got, want)
			}
		}
		if got, want := nrgbaAt(t, img, 0, 1), [4]uint8{0, 0, 0, 0}; got != want {
			t.Errorf("the run leaked onto the second line: %v, want %v", got, want)
		}
	})

	t.Run("long transparent run", func(t *testing.T) {
		// 0x40 | 0x00, then 0x03 -> 3 transparent pixels, then one of colour 2.
		img, err := decode(t, 4, 1, []byte{0x00, 0x40, 0x03, 0x02, 0x00, 0x00})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := nrgbaAt(t, img, 2, 0), [4]uint8{0, 0, 0, 0}; got != want {
			t.Errorf("pixel (2,0) = %v, want %v", got, want)
		}
		if got, want := nrgbaAt(t, img, 3, 0), [4]uint8{16, 64, 255, 255}; got != want {
			t.Errorf("pixel (3,0) = %v, want the blue %v", got, want)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		cases := []struct {
			name string
			rle  []byte
			want string
		}{
			{"escape at the end", []byte{0x00}, "ends on an escape byte"},
			{"long transparent run", []byte{0x00, 0x40}, "long transparent run is truncated"},
			{"coloured run with no colour", []byte{0x00, 0x80}, "missing its colour"},
			{"long coloured run", []byte{0x00, 0xc0, 0x05}, "long coloured run is truncated"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := decode(t, 4, 2, tc.rle)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("got %v, want an error mentioning %q", err, tc.want)
				}
			})
		}
	})
}

// A palette entry the stream never defined paints nothing, rather than an
// arbitrary colour.
func TestPGSDecoder_UndefinedPaletteEntryIsTransparent(t *testing.T) {
	d := NewPGSDecoder()
	sets, err := d.Decode(concat(
		pgsPCS(100, 100, pgsEpochStart, 0, synthObject{id: 1}),
		pgsPDS(0, palWhite), // index 2 is never defined
		pgsODS(1, 4, 2, rle4x2),
		pgsEND(),
	))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nrgbaAt(t, sets[0].Objects[0].Image, 2, 1), [4]uint8{0, 0, 0, 0}; got != want {
		t.Errorf("an undefined palette index painted %v, want %v", got, want)
	}
}

// A PGS picture always fits a palette - that is the whole point of Paletted -
// and re-indexing must not change a single pixel.
func TestPaletted(t *testing.T) {
	d := NewPGSDecoder()
	sets, err := d.Decode(concat(
		pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1}),
		pgsPDS(0, palWhite, palBlue),
		pgsODS(1, 4, 2, rle4x2),
		pgsEND(),
	))
	if err != nil {
		t.Fatal(err)
	}
	src := sets[0].Objects[0].Image
	p := Paletted(src)
	if p == nil {
		t.Fatal("a decoded PGS picture did not fit a palette")
	}
	if p.Bounds() != src.Bounds() {
		t.Fatalf("bounds %v, want %v", p.Bounds(), src.Bounds())
	}
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			r1, g1, b1, a1 := src.At(x, y).RGBA()
			r2, g2, b2, a2 := p.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("pixel (%d,%d) changed: %v -> %v", x, y, src.At(x, y), p.At(x, y))
			}
		}
	}
	if n := len(p.Palette); n > 3 {
		t.Errorf("palette holds %d colours for a 3-colour picture", n)
	}
	if Paletted(nil) != nil {
		t.Error("Paletted(nil) must be nil")
	}
}

// More than 256 distinct colours cannot be indexed; the caller keeps the
// truecolour picture rather than getting a silently degraded one.
func TestPaletted_TooManyColours(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 1))
	for x := 0; x < 300; x++ {
		o := img.PixOffset(x, 0)
		img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = uint8(x), uint8(x>>1), uint8(x>>2), 255
	}
	if Paletted(img) != nil {
		t.Error("a 300-colour picture was indexed anyway")
	}
}
