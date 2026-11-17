package subtitle

import (
	"image"
	"runtime"
	"testing"
)

// allocated runs fn and returns the bytes it allocated.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A display set that only names a region again costs nothing of the region: 50 page-only blocks of 14 bytes each
// made a 2048x2048 region be read through its CLUT 50 times, 16 MiB a time.
func TestDVBSubUnchangedRegionIsNotRenderedAgain(t *testing.T) {
	d := newDVBSub(t)
	at := dvbPlacement{region: 0, x: 10, y: 20}
	first := decodeOneDVB(t, d, concat(dvbPCS(2, at), dvbRCS(0, true, 2048, 2048, 3, 0, 1), dvbEDS()))
	if len(first.Objects) != 1 {
		t.Fatalf("%d objects, want the region", len(first.Objects))
	}
	page := dvbPCS(0, at) // a page composition and nothing else: the display set ends with the block
	if len(page) != 14 {
		t.Fatalf("the page-only block is %d bytes, want 14", len(page))
	}
	var last BitmapDisplaySet
	alloc := allocated(func() {
		for i := 0; i < 50; i++ {
			last = decodeOneDVB(t, d, page)
		}
	})
	if len(last.Objects) != 1 || last.Objects[0].Image != first.Objects[0].Image {
		t.Fatalf("a display set naming the unchanged region again does not hand back its picture")
	}
	if alloc > 1<<20 {
		t.Errorf("50 page-only blocks allocated %d KiB, want next to nothing: the region did not change", alloc>>10)
	}
}

// The picture kept for a region is dropped as soon as what it shows changes: its pixels, or its colours.
func TestDVBSubRegionPictureFollowsItsPixelsAndColours(t *testing.T) {
	at := dvbPlacement{region: 0}
	show := concat(dvbPCS(2, at), dvbRCS(0, true, 4, 2, 2, 0, 0, dvbObjectRef{id: 7}), dvbTestCLUT, dvbODS(7, false, dvbTop4x2, dvbBottom4x2), dvbEDS())
	page := concat(dvbPCS(0, at), dvbEDS())
	white, red := [4]uint8{255, 255, 255, 255}, [4]uint8{}
	red[0], red[1], red[2] = ycrcbToRGB(81, 240, 90)
	red[3] = 255
	for name, tc := range map[string]struct {
		change []byte
		want   [4]uint8 // the top-left pixel afterwards
	}{
		"a CLUT entry redefined": {dvbCDS(0, [6]byte{1, 0x41, 81, 240, 90, 0}), red},
		"an object drawn over":   {dvbODS(7, false, []byte{dvbData4Bit, 0x22, 0x22, 0x00, dvbDataEndRow}, nil), [4]uint8{}},
		"the region filled":      {dvbRCS(0, true, 4, 2, 2, 0, 1), white},
		"another CLUT":           {concat(dvbCDS(5, [6]byte{1, 0x41, 81, 240, 90, 0}), dvbRCS(0, false, 4, 2, 2, 5, 0, dvbObjectRef{id: 7})), red},
	} {
		d := newDVBSub(t)
		before := decodeOneDVB(t, d, show)
		if got := nrgbaAt(t, before.Objects[0].Image, 0, 0); got != white {
			t.Fatalf("%s: the top-left pixel starts as %v, want white", name, got)
		}
		after := decodeOneDVB(t, d, concat(tc.change, page))
		if len(after.Objects) != 1 || after.Objects[0].Image == before.Objects[0].Image {
			t.Errorf("%s: the display set after the change shows the picture from before it", name)
			continue
		}
		want := tc.want
		if want == ([4]uint8{}) { // the object drawn over paints entry 2: the test CLUT's blue
			want[0], want[1], want[2] = ycrcbToRGB(82, 90, 240)
			want[3] = 255
		}
		if got := nrgbaAt(t, after.Objects[0].Image, 0, 0); got != want {
			t.Errorf("%s: the top-left pixel is %v, want %v", name, got, want)
		}
		if got := nrgbaAt(t, before.Objects[0].Image, 0, 0); got != white {
			t.Errorf("%s: the picture handed back before the change was written over: %v", name, got)
		}
	}
}

// A composition that names an object again costs nothing of the object: 50 blocks of a composition and an end
// segment made a 1024x1024 object be expanded 50 times, 4 MiB a time.
func TestPGSUnchangedObjectIsNotDecodedAgain(t *testing.T) {
	d := NewPGSDecoder()
	show := concat(pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1, x: 10, y: 20}), pgsPDS(0, palWhite),
		pgsODS(1, 1024, 1024, []byte{0x00, 0xc4, 0x00, 0x01, 0x00, 0x00}), pgsEND())
	again := concat(pgsPCS(1920, 1080, 0, 0, synthObject{id: 1, x: 30, y: 40}), pgsEND())
	first, err := d.Decode(show)
	if err != nil || len(first) != 1 || len(first[0].Objects) != 1 {
		t.Fatalf("%d sets, err %v", len(first), err)
	}
	var last []PGSDisplaySet
	alloc := allocated(func() {
		for i := 0; i < 50; i++ {
			if last, err = d.Decode(again); err != nil {
				t.Fatal(err)
			}
		}
	})
	if len(last) != 1 || len(last[0].Objects) != 1 || last[0].Objects[0].Image != first[0].Objects[0].Image || last[0].Objects[0].X != 30 {
		t.Fatalf("a composition naming the unchanged object again does not hand back its picture, placed anew")
	}
	if alloc > 1<<20 {
		t.Errorf("50 compositions of an unchanged object allocated %d KiB, want next to nothing", alloc>>10)
	}
}

// The picture kept for an object is dropped when the object, its palette or the crop asked of it changes.
func TestPGSObjectPictureFollowsItsDataAndPalette(t *testing.T) {
	show := concat(pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1}), pgsPDS(0, palWhite, palBlue), pgsODS(1, 4, 2, rle4x2), pgsEND())
	again := concat(pgsPCS(1920, 1080, 0, 0, synthObject{id: 1}), pgsEND())
	white := [4]uint8{255, 255, 255, 255}
	var blue [4]uint8
	blue[0], blue[1], blue[2] = ycrcbToRGB(82, 90, 240)
	blue[3] = 255
	crop := image.Rect(2, 0, 4, 2)
	for name, tc := range map[string]struct {
		change []byte
		then   []byte
		want   [4]uint8 // the top-left pixel afterwards
	}{
		"a palette entry redefined": {pgsPDS(0, [5]byte{1, 82, 90, 240, 255}), again, blue},
		"the object redefined":      {pgsODS(1, 4, 2, []byte{0x00, 0x84, 0x02, 0x00, 0x00, 0x00, 0x84, 0x02, 0x00, 0x00}), again, blue},
		"another palette":           {pgsPDS(3, [5]byte{1, 82, 90, 240, 255}), concat(pgsPCS(1920, 1080, 0, 3, synthObject{id: 1}), pgsEND()), blue},
		"a crop of it":              {nil, concat(pgsPCS(1920, 1080, 0, 0, synthObject{id: 1, crop: &crop}), pgsEND()), white},
	} {
		d := NewPGSDecoder()
		before, err := d.Decode(show)
		if err != nil || len(before) != 1 {
			t.Fatal(err)
		}
		after, err := d.Decode(concat(tc.change, tc.then))
		if err != nil || len(after) != 1 || len(after[0].Objects) != 1 {
			t.Fatalf("%s: %d sets, err %v", name, len(after), err)
		}
		if after[0].Objects[0].Image == before[0].Objects[0].Image {
			t.Errorf("%s: the display set after the change shows the picture from before it", name)
		}
		if got := nrgbaAt(t, after[0].Objects[0].Image, 0, 0); got != tc.want {
			t.Errorf("%s: the top-left pixel is %v, want %v", name, got, tc.want)
		}
		if got := nrgbaAt(t, before[0].Objects[0].Image, 0, 0); got != white {
			t.Errorf("%s: the picture handed back before the change was written over: %v", name, got)
		}
	}
}

// What the decoder keeps of pictures is bounded: objects that together decode past one full plane are not all kept.
func TestPGSKeptPicturesBounded(t *testing.T) {
	d := NewPGSDecoder()
	parts := [][]byte{pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1}, synthObject{id: 2}, synthObject{id: 3}), pgsPDS(0, palWhite)}
	for id := uint16(1); id <= 3; id++ {
		parts = append(parts, pgsODS(id, 4096, 2048, []byte{0, 0}))
	}
	if _, err := d.Decode(concat(append(parts, pgsEND())...)); err != nil {
		t.Fatal(err)
	}
	if d.keptPixels > pgsMaxKeptPixels {
		t.Errorf("the decoder keeps %d pixels of pictures, past its bound of %d", d.keptPixels, pgsMaxKeptPixels)
	}
}
