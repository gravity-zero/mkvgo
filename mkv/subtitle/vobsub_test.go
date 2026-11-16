package subtitle

import (
	"strings"
	"testing"
	"time"
)

const vobsubIdx = "# VobSub index file, v7\nsize: 720x576\npalette: 000000, ff0000, 00ff00, 0000ff, ffffff, 101010, 202020, 303030, 404040, 505050, 606060, 707070, 808080, 909090, a0a0a0, b0b0b0\nforced subs: OFF\n"

type spuSeq struct {
	date int
	cmds []byte
}

// vobsubSPU assembles a subpicture unit: the two fields' data at bytes 4 and 4+len(top), then the chained sequences.
func vobsubSPU(top, bottom []byte, seqs ...spuSeq) []byte {
	ctrl := 4 + len(top) + len(bottom)
	out := []byte{0, 0, byte(ctrl >> 8), byte(ctrl)}
	out = append(append(out, top...), bottom...)
	for i, s := range seqs {
		next := len(out) // the last sequence names itself
		if i < len(seqs)-1 {
			next += 4 + len(s.cmds) + 1
		}
		out = append(out, byte(s.date>>8), byte(s.date), byte(next>>8), byte(next))
		out = append(append(out, s.cmds...), spuEnd)
	}
	out[0], out[1] = byte(len(out)>>8), byte(len(out))
	return out
}

func spuArea4x2(x, y int) []byte {
	x2, y2 := x+3, y+1
	return []byte{spuArea, byte(x >> 4), byte(x<<4) | byte(x2>>8), byte(x2), byte(y >> 4), byte(y<<4) | byte(y2>>8), byte(y2)}
}

// Top line: two pixels of value 1, one of 2, one of 3. Bottom line: value 2 to the end of the line.
var (
	spuTop4x2    = []byte{0x96, 0x70}
	spuBottom4x2 = []byte{0x00, 0x02}
)

// spuShow4x2 is the usual first sequence: values 0..3 take palette entries 0, 1, 2, 4, value 0 transparent.
func spuShow4x2(start byte) []byte {
	cmds := []byte{start, spuColours, 0x42, 0x10, spuContrast, 0xff, 0xf0}
	cmds = append(cmds, spuArea4x2(100, 400)...)
	return append(cmds, spuOffsets, 0, 4, 0, 6)
}

func newVobSub(t *testing.T) *VobSubDecoder {
	t.Helper()
	d, err := NewVobSubDecoder([]byte(vobsubIdx))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func decodeOneVobSub(t *testing.T, block []byte) BitmapDisplaySet {
	t.Helper()
	sets, err := newVobSub(t).Decode(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("got %d display sets, want 1", len(sets))
	}
	return sets[0]
}

func TestVobSubDecoder_Picture(t *testing.T) {
	block := vobsubSPU(spuTop4x2, spuBottom4x2,
		spuSeq{date: 0, cmds: spuShow4x2(spuStart)},
		spuSeq{date: 300, cmds: []byte{spuStop}})
	ds := decodeOneVobSub(t, block)
	if ds.ScreenW != 720 || ds.ScreenH != 576 {
		t.Errorf("plane %dx%d, want 720x576", ds.ScreenW, ds.ScreenH)
	}
	if ds.Delay != 0 || ds.Duration != 300*1024*time.Second/90000 {
		t.Errorf("delay %v duration %v, want 0 and %v", ds.Delay, ds.Duration, 300*1024*time.Second/90000)
	}
	if len(ds.Objects) != 1 {
		t.Fatalf("got %d objects, want 1", len(ds.Objects))
	}
	o := ds.Objects[0]
	if o.X != 100 || o.Y != 400 || o.Forced {
		t.Errorf("object at %d,%d forced=%v, want 100,400 not forced", o.X, o.Y, o.Forced)
	}
	if b := o.Image.Bounds(); b.Dx() != 4 || b.Dy() != 2 {
		t.Fatalf("image %dx%d, want 4x2", b.Dx(), b.Dy())
	}
	red, green, white := [4]uint8{255, 0, 0, 255}, [4]uint8{0, 255, 0, 255}, [4]uint8{255, 255, 255, 255}
	want := [2][4][4]uint8{{red, red, green, white}, {green, green, green, green}}
	for y := range want {
		for x := range want[y] {
			if got := nrgbaAt(t, o.Image, x, y); got != want[y][x] {
				t.Errorf("pixel %d,%d = %v, want %v", x, y, got, want[y][x])
			}
		}
	}
}

func TestVobSubDecoder_TransparentAndPartialAlpha(t *testing.T) {
	cmds := []byte{spuStart, spuColours, 0x42, 0x13, spuContrast, 0x08, 0xf0}
	cmds = append(cmds, spuArea4x2(0, 0)...)
	cmds = append(cmds, spuOffsets, 0, 4, 0, 6)
	// Top line: value 0 twice, value 1, value 2.
	ds := decodeOneVobSub(t, vobsubSPU([]byte{0x85, 0x60}, spuBottom4x2, spuSeq{cmds: cmds}))
	img := ds.Objects[0].Image
	if got := nrgbaAt(t, img, 0, 0); got != [4]uint8{} {
		t.Errorf("a pixel of contrast 0 = %v, want untouched", got)
	}
	if got := nrgbaAt(t, img, 2, 0); got != [4]uint8{255, 0, 0, 255} {
		t.Errorf("pixel value 1 = %v, want opaque red", got)
	}
	if got := nrgbaAt(t, img, 3, 0); got != [4]uint8{0, 255, 0, 8 * 17} {
		t.Errorf("pixel value 2 = %v, want green at alpha %d", got, 8*17)
	}
}

func TestVobSubDecoder_ForcedStartAndDelay(t *testing.T) {
	ds := decodeOneVobSub(t, vobsubSPU(spuTop4x2, spuBottom4x2,
		spuSeq{date: 90, cmds: spuShow4x2(spuForcedStart)},
		spuSeq{date: 180, cmds: []byte{spuStop}}))
	if !ds.Objects[0].Forced {
		t.Error("a forced start display gave a picture that is not Forced")
	}
	if want := 90 * 1024 * time.Second / 90000; ds.Delay != want || ds.Duration != want {
		t.Errorf("delay %v duration %v, want %v each", ds.Delay, ds.Duration, want)
	}
}

func TestVobSubDecoder_NoStopLeavesTheDurationOpen(t *testing.T) {
	ds := decodeOneVobSub(t, vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)}))
	if ds.Duration != 0 || ds.Clear() {
		t.Errorf("duration %v clear=%v, want an open-ended picture", ds.Duration, ds.Clear())
	}
}

func TestVobSubDecoder_StopOnlyClears(t *testing.T) {
	ds := decodeOneVobSub(t, vobsubSPU(nil, nil, spuSeq{date: 45, cmds: []byte{spuStop}}))
	if !ds.Clear() || ds.Delay != 45*1024*time.Second/90000 {
		t.Errorf("clear=%v delay=%v, want an empty set delayed by the stop date", ds.Clear(), ds.Delay)
	}
	if ds.ScreenW != 720 || ds.ScreenH != 576 {
		t.Errorf("plane %dx%d, want 720x576", ds.ScreenW, ds.ScreenH)
	}
}

func TestVobSubDecoder_NothingToShowOrRemove(t *testing.T) {
	sets, err := newVobSub(t).Decode(vobsubSPU(nil, nil, spuSeq{cmds: []byte{spuColours, 0x42, 0x10}}))
	if err != nil || len(sets) != 0 {
		t.Errorf("got %d sets, err %v; want none", len(sets), err)
	}
}

// A picture that starts fully transparent takes the contrast a later sequence gives it; one that is visible keeps its own.
func TestVobSubDecoder_FadeIn(t *testing.T) {
	first := spuShow4x2(spuStart)
	first[5], first[6] = 0, 0
	ds := decodeOneVobSub(t, vobsubSPU(spuTop4x2, spuBottom4x2,
		spuSeq{cmds: first},
		spuSeq{date: 10, cmds: []byte{spuContrast, 0xff, 0xf0}},
		spuSeq{date: 20, cmds: []byte{spuContrast, 0x11, 0x10}}))
	if got := nrgbaAt(t, ds.Objects[0].Image, 0, 0); got != [4]uint8{255, 0, 0, 255} {
		t.Errorf("pixel = %v, want the first visible contrast, opaque red", got)
	}
}

func TestVobSubDecoder_RunLengthForms(t *testing.T) {
	area := func(w int) []byte {
		x2 := w - 1
		return []byte{spuArea, 0, byte(x2 >> 8), byte(x2), 0, 0, 0}
	}
	for _, tc := range []struct {
		name string
		w    int
		data []byte
		runs []int // lengths of the alternating runs of values 1 and 2
	}{
		{"one nibble", 4, []byte{0xd6}, []int{3, 1}},
		{"two nibbles", 16, []byte{0x3d, 0x60}, []int{15, 1}},
		{"three nibbles", 64, []byte{0x0f, 0xd6}, []int{63, 1}},
		{"four nibbles", 256, []byte{0x03, 0xfd, 0x60}, []int{255, 1}},
		{"to the end of the line", 300, []byte{0x96, 0x00, 0x01}, []int{2, 1, 297}},
		{"a run past the line stops at its end", 3, []byte{0x3d}, []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmds := append([]byte{spuStart, spuColours, 0x42, 0x10}, area(tc.w)...)
			cmds = append(cmds, spuOffsets, 0, 4, 0, 4)
			ds := decodeOneVobSub(t, vobsubSPU(tc.data, nil, spuSeq{cmds: cmds}))
			img, x := ds.Objects[0].Image, 0
			for i, n := range tc.runs {
				want := [4]uint8{255, 0, 0, 255}
				if tc.name != "to the end of the line" && i%2 == 1 || tc.name == "to the end of the line" && i == 1 {
					want = [4]uint8{0, 255, 0, 255}
				}
				for ; n > 0; n, x = n-1, x+1 {
					if got := nrgbaAt(t, img, x, 0); got != want {
						t.Fatalf("pixel %d = %v, want %v", x, got, want)
					}
				}
			}
			if x != tc.w {
				t.Fatalf("the runs cover %d pixels of %d", x, tc.w)
			}
		})
	}
}

func TestNewVobSubDecoder_Refusals(t *testing.T) {
	for name, tc := range map[string]struct{ idx, want string }{
		"empty":           {"", "lacks the size: or palette: line"},
		"no palette":      {"size: 720x576\n", "lacks the size: or palette: line"},
		"no size":         {"palette: 0,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f\n", "lacks the size: or palette: line"},
		"short palette":   {"size: 720x576\npalette: 000000, ffffff\n", "holds 2 colours"},
		"bad colour":      {"size: 720x576\npalette: zz,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f\n", "not an RRGGBB colour"},
		"bad size":        {"size: 720\npalette: 0,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f\n", "not a width x height"},
		"oversized plane": {"size: 70000x576\npalette: 0,1,2,3,4,5,6,7,8,9,a,b,c,d,e,f\n", "not a width x height"},
	} {
		if _, err := NewVobSubDecoder([]byte(tc.idx)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

func TestNewVobSubDecoder_ToleratesTheHeaderAsMuxersWriteIt(t *testing.T) {
	d, err := NewVobSubDecoder([]byte("\r\n# comment: x\r\nSize: 720 x 480\r\nPalette: 0d00ee,ee450d,101010,ebebeb,0ce60b,ec14ed,ebff0b,0d637e,a1a1a1,c5c5c5,0e640c,89db89,0e0089,a2bdd4,ebcf0b,7e127e\r\n\x00\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if d.screenW != 720 || d.screenH != 480 || d.palette[1] != [3]uint8{0xee, 0x45, 0x0d} {
		t.Errorf("read %dx%d, palette[1] %v", d.screenW, d.screenH, d.palette[1])
	}
}

func TestVobSubDecoder_Malformed(t *testing.T) {
	good := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)})
	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	show := func(cmds ...byte) []byte { return vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: cmds}) }
	for name, tc := range map[string]struct {
		block []byte
		want  string
	}{
		"short header":        {[]byte{0, 4, 0}, "needs 4 bytes of header"},
		"control before data": {mut(func(b []byte) []byte { b[2], b[3] = 0, 2; return b }), "places its control sequences"},
		"control at the end":  {mut(func(b []byte) []byte { b[2], b[3] = b[0], b[1]; return b }), "places its control sequences"},
		"backward chain":      {mut(func(b []byte) []byte { b[10], b[11] = 0, 4; return b }), "is not after it"},
		"truncated command":   {show(spuStart, spuArea, 0, 0), "needs 6 more byte(s)"},
		"no area":             {show(spuStart, spuOffsets, 0, 4, 0, 6), "without naming its area"},
		"no offsets":          {show(append([]byte{spuStart}, spuArea4x2(0, 0)...)...), "without naming its area"},
		"inverted area":       {show(spuStart, spuArea, 0x00, 0x40, 0x00, 0, 0, 1, spuOffsets, 0, 4, 0, 6), "holds no pixel"},
		"field outside":       {show(append(append([]byte{spuStart}, spuArea4x2(0, 0)...), spuOffsets, 0, 4, 0, 200)...), "outside the"},
		"bad change length":   {show(spuChange, 0, 1), "colour change command"},
		"data ends early": {vobsubSPU([]byte{0x96, 0x70}, nil, spuSeq{cmds: append(append([]byte{spuStart}, spuArea4x2(0, 0)...), spuOffsets, 0, 4, 0, 5)}),
			"data ends on line"},
	} {
		sets, err := newVobSub(t).Decode(tc.block)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %d sets, err = %v, want one naming %q", name, len(sets), err, tc.want)
		}
	}
}

func TestVobSubDecoder_SkipsWhatItDoesNotApply(t *testing.T) {
	cmds := append([]byte{spuChange, 0, 4, 0xaa, 0xbb}, spuShow4x2(spuStart)...)
	ds := decodeOneVobSub(t, append(vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: cmds}), 0, 0, 0))
	if len(ds.Objects) != 1 {
		t.Fatalf("a colour change command or bytes after the unit lost the picture")
	}
}

// A unit a muxer spread over two blocks comes out with the second; Reset forgets the first.
func TestVobSubDecoder_UnitOverTwoBlocks(t *testing.T) {
	block := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)})
	d := newVobSub(t)
	if sets, err := d.Decode(block[:7]); err != nil || len(sets) != 0 {
		t.Fatalf("the first part gave %d sets, err %v; want nothing yet", len(sets), err)
	}
	sets, err := d.Decode(block[7:])
	if err != nil || len(sets) != 1 || len(sets[0].Objects) != 1 {
		t.Fatalf("the second part gave %d sets, err %v; want the picture", len(sets), err)
	}
	if _, err := d.Decode(block[:7]); err != nil {
		t.Fatal(err)
	}
	d.Reset()
	if sets, err := d.Decode(block); err != nil || len(sets) != 1 {
		t.Fatalf("after Reset a whole unit gave %d sets, err %v; want its picture", len(sets), err)
	}
}

// A block that declares more than it holds is not completed by a block that is a whole unit of its own.
func TestVobSubDecoder_WholeUnitAfterAnIncompleteOne(t *testing.T) {
	unit := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)}, spuSeq{date: 300, cmds: []byte{spuStop}})
	damaged := append([]byte(nil), unit...)
	damaged[0] = 0x40 // declares 16 KiB and more
	d := newVobSub(t)
	if sets, err := d.Decode(damaged); err != nil || len(sets) != 0 {
		t.Fatalf("the damaged block gave %d sets, err %v; want nothing yet", len(sets), err)
	}
	for i := 0; i < 3; i++ {
		sets, err := d.Decode(unit)
		if err != nil || len(sets) != 1 || len(sets[0].Objects) != 1 {
			t.Fatalf("whole unit %d after the damaged block: %d sets, err %v; want its picture", i, len(sets), err)
		}
	}
	if d.Pending() {
		t.Error("the damaged block's bytes are still held")
	}
}

// Pending tells a caller the block it just fed started a unit, whose time is that block's.
func TestVobSubDecoder_Pending(t *testing.T) {
	unit := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)})
	d := newVobSub(t)
	if d.Pending() {
		t.Fatal("a new decoder holds a unit")
	}
	if _, err := d.Decode(unit[:7]); err != nil || !d.Pending() {
		t.Fatalf("after the first part: pending=%v, err %v", d.Pending(), err)
	}
	if sets, err := d.Decode(unit[7:]); err != nil || len(sets) != 1 || d.Pending() {
		t.Fatalf("after the second part: %d sets, pending=%v, err %v", len(sets), d.Pending(), err)
	}
}

// A stop at the date of the start shows nothing: an empty set, not a picture without an end.
func TestVobSubDecoder_StopAtTheStartShowsNothing(t *testing.T) {
	ds := decodeOneVobSub(t, vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{date: 20, cmds: append(spuShow4x2(spuStart), spuStop)}))
	if !ds.Clear() || ds.Delay != 20*1024*time.Second/90000 || ds.Duration != 0 {
		t.Errorf("clear=%v delay=%v duration=%v; want an empty set at the unit's date", ds.Clear(), ds.Delay, ds.Duration)
	}
}
