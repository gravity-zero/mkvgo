package subtitle_test

// Real tracks: a DVD's VobSub and a broadcast's DVB subtitles, each remuxed to Matroska with its packets untouched.
// The golden pictures are the mainstream decoder's rendering of one display set on the whole subtitle plane.

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

const (
	vobsubSample = "../../internal/testdata/vobsub.mkv"
	dvbsubSample = "../../internal/testdata/dvbsub.mkv"
)

// sampleTrack reads the one track of a sample and all its blocks.
func sampleTrack(tb testing.TB, path string) (*mkv.Track, []mkv.Block) {
	tb.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		tb.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		tb.Fatal(err)
	}
	var blocks []mkv.Block
	for {
		b, err := br.Next()
		if err == io.EOF {
			return &c.Tracks[0], blocks
		}
		if err != nil {
			tb.Fatal(err)
		}
		b.Data = append([]byte(nil), b.Data...)
		blocks = append(blocks, b)
	}
}

// planeDiff counts the pixels of the subtitle plane where a display set and the golden rendering differ; two fully
// transparent pixels are the same whatever colour they carry.
func planeDiff(tb testing.TB, ds subtitle.BitmapDisplaySet, golden string) int {
	tb.Helper()
	f, err := os.Open(golden)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	decoded, err := png.Decode(f)
	if err != nil {
		tb.Fatal(err)
	}
	ref, ok := decoded.(*image.NRGBA)
	if !ok || ref.Bounds().Dx() != ds.ScreenW || ref.Bounds().Dy() != ds.ScreenH {
		tb.Fatalf("%s is a %T of %v, want an NRGBA of the %dx%d plane", golden, decoded, decoded.Bounds().Size(), ds.ScreenW, ds.ScreenH)
	}
	plane := image.NewNRGBA(ref.Bounds())
	for _, o := range ds.Objects {
		b := o.Image.Bounds()
		for y := 0; y < b.Dy(); y++ {
			src := o.Image.Pix[y*o.Image.Stride : y*o.Image.Stride+4*b.Dx()]
			copy(plane.Pix[plane.PixOffset(o.X, o.Y+y):], src)
		}
	}
	diff := 0
	for i := 0; i < len(ref.Pix); i += 4 {
		if ref.Pix[i+3] == 0 && plane.Pix[i+3] == 0 {
			continue
		}
		if [4]byte(ref.Pix[i:i+4]) != [4]byte(plane.Pix[i:i+4]) {
			diff++
		}
	}
	return diff
}

type sampleCue struct {
	timeMs, durMs int64
	x, y, w, h    int
}

func TestVobSubDecoder_RealTrack(t *testing.T) {
	track, blocks := sampleTrack(t, vobsubSample)
	if track.Codec != "vobsub" || len(blocks) != 46 {
		t.Fatalf("sample holds a %q track of %d blocks, want vobsub and 46", track.Codec, len(blocks))
	}
	d, err := subtitle.NewVobSubDecoder(track.CodecPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var cues []sampleCue
	var first subtitle.BitmapDisplaySet
	for i, b := range blocks {
		sets, err := d.Decode(b.Data)
		if err != nil {
			t.Fatalf("block %d at %d ms: %v", i, b.Timecode, err)
		}
		for _, ds := range sets {
			if ds.ScreenW != 720 || ds.ScreenH != 480 || len(ds.Objects) != 1 || ds.Objects[0].Forced || ds.Delay != 0 {
				t.Fatalf("block %d: plane %dx%d, %d objects, delay %v; want one unforced picture on 720x480, undelayed", i, ds.ScreenW, ds.ScreenH, len(ds.Objects), ds.Delay)
			}
			if len(cues) == 0 {
				first = ds
			}
			o := ds.Objects[0]
			cues = append(cues, sampleCue{b.Timecode, ds.Duration.Milliseconds(), o.X, o.Y, o.Image.Bounds().Dx(), o.Image.Bounds().Dy()})
		}
	}
	// Three of the 46 blocks hold the first part of a unit the muxer cut in two.
	if len(cues) != 43 {
		t.Fatalf("got %d cues, want 43", len(cues))
	}
	for i, want := range map[int]sampleCue{
		0:  {132499, 4960, 259, 379, 200, 24},
		1:  {147355, 4733, 259, 76, 203, 63},
		4:  {185919, 2696, 234, 340, 250, 63}, // a unit over two blocks
		42: {359834, 1262, 232, 379, 254, 24},
	} {
		if cues[i] != want {
			t.Errorf("cue %d = %+v, want %+v", i, cues[i], want)
		}
	}
	if n := planeDiff(t, first, "../../internal/testdata/vobsub_cue0.png"); n != 0 {
		t.Errorf("the first cue differs from its golden rendering on %d pixels", n)
	}
}

func TestDVBSubDecoder_RealTrack(t *testing.T) {
	track, blocks := sampleTrack(t, dvbsubSample)
	if track.Codec != "dvbsub" || len(blocks) != 46 {
		t.Fatalf("sample holds a %q track of %d blocks, want dvbsub and 46", track.Codec, len(blocks))
	}
	d, err := subtitle.NewDVBSubDecoder(track.CodecPrivate)
	if err != nil {
		t.Fatal(err)
	}
	type shown struct {
		timeMs  int64
		objects []sampleCue
	}
	var sets []shown
	var golden subtitle.BitmapDisplaySet
	for i, b := range blocks {
		got, err := d.Decode(b.Data)
		if err != nil {
			t.Fatalf("block %d at %d ms: %v", i, b.Timecode, err)
		}
		if len(got) != 1 || got[0].ScreenW != 720 || got[0].ScreenH != 576 || got[0].Delay != 0 || got[0].Duration != 0 {
			t.Fatalf("block %d: %d sets; want one, untimed, on 720x576", i, len(got))
		}
		s := shown{timeMs: b.Timecode}
		for _, o := range got[0].Objects {
			s.objects = append(s.objects, sampleCue{x: o.X, y: o.Y, w: o.Image.Bounds().Dx(), h: o.Image.Bounds().Dy()})
		}
		sets = append(sets, s)
		if i == 13 {
			golden = got[0]
		}
	}
	// The screen is empty for the first seven display sets and the last three; the 36 between each show a picture.
	for i, s := range sets {
		if clear := i < 7 || i > 42; clear != (len(s.objects) == 0) {
			t.Errorf("set %d at %d ms shows %d objects", i, s.timeMs, len(s.objects))
		}
	}
	for i, want := range map[int]shown{
		7:  {15600, []sampleCue{{x: 78, y: 416, w: 88, h: 36}}},
		12: {17400, []sampleCue{{x: 78, y: 416, w: 406, h: 36}}}, // the line, word after word
		13: {17760, []sampleCue{{x: 78, y: 380, w: 406, h: 36}, {x: 78, y: 416, w: 148, h: 36}}},
		42: {29360, []sampleCue{{x: 78, y: 380, w: 460, h: 36}, {x: 78, y: 416, w: 178, h: 36}}},
		43: {31040, nil},
	} {
		got := sets[i]
		if got.timeMs != want.timeMs || len(got.objects) != len(want.objects) {
			t.Errorf("set %d = %+v, want %+v", i, got, want)
			continue
		}
		for k := range want.objects {
			if got.objects[k] != want.objects[k] {
				t.Errorf("set %d object %d = %+v, want %+v", i, k, got.objects[k], want.objects[k])
			}
		}
	}
	if n := planeDiff(t, golden, "../../internal/testdata/dvbsub_set13.png"); n != 0 {
		t.Errorf("display set 13 differs from its golden rendering on %d pixels", n)
	}
}

// The .idx/.sub pair the VobSub sample was muxed from gives the same units at the same times as the Matroska.
func TestVobSubIndex_RealPair(t *testing.T) {
	_, blocks := sampleTrack(t, vobsubSample)
	type unit struct {
		timeMs int64
		data   []byte
	}
	var want []unit // a unit the muxer cut in two is the two blocks, at the time of the first
	for _, b := range blocks {
		if n := len(want); n > 0 && len(want[n-1].data) < int(want[n-1].data[0])<<8|int(want[n-1].data[1]) {
			want[n-1].data = append(want[n-1].data, b.Data...)
			continue
		}
		want = append(want, unit{b.Timecode, b.Data})
	}

	idx, err := os.Open("../../internal/testdata/vobsub.idx")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ix, err := subtitle.ParseVobSubIndex(idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Streams) != 1 || ix.Streams[0].Language != "en" || ix.Streams[0].Index != 0 || len(ix.Streams[0].Entries) != 47 {
		t.Fatalf("index = %d streams, the first %q with %d entries; want one, en, 47", len(ix.Streams), ix.Streams[0].Language, len(ix.Streams[0].Entries))
	}
	sub, err := os.Open("../../internal/testdata/vobsub.sub")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	d, err := subtitle.NewVobSubDecoder(ix.Header)
	if err != nil {
		t.Fatal(err)
	}
	var got []unit
	var first subtitle.BitmapDisplaySet
	err = ix.ForEachUnit(sub, 0, func(timeMs int64, data []byte) error {
		sets, err := d.Decode(data)
		if err != nil || len(sets) != 1 {
			t.Errorf("unit at %d ms: %d sets, err %v", timeMs, len(sets), err)
		} else if len(got) == 0 {
			first = sets[0]
		}
		got = append(got, unit{timeMs, append([]byte(nil), data...)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 47 entries: three are the second packet of a unit, the last sits at the end of the .sub.
	if len(got) != 43 || len(want) != 43 {
		t.Fatalf("the pair gives %d units, the Matroska %d; want 43 each", len(got), len(want))
	}
	for i := range want {
		if got[i].timeMs != want[i].timeMs || !bytes.Equal(got[i].data, want[i].data) {
			t.Errorf("unit %d: %d ms and %d bytes from the pair, %d ms and %d bytes from the Matroska", i, got[i].timeMs, len(got[i].data), want[i].timeMs, len(want[i].data))
		}
	}
	if n := planeDiff(t, first, "../../internal/testdata/vobsub_cue0.png"); n != 0 {
		t.Errorf("the first unit differs from its golden rendering on %d pixels", n)
	}
}

// A real .sup: one whole display set, then one the file stops in the middle of.
func TestForEachSUPDisplaySet_RealFile(t *testing.T) {
	f, err := os.Open("../../internal/testdata/pgs_sub.sup")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := subtitle.NewPGSDecoder()
	var times []int64
	var first subtitle.BitmapDisplaySet
	err = subtitle.ForEachSUPDisplaySet(f, func(timeMs int64, set []byte) error {
		sets, err := d.Decode(set)
		if err != nil || len(sets) != 1 {
			t.Errorf("display set at %d ms: %d sets, err %v", timeMs, len(sets), err)
		} else if len(times) == 0 {
			first = sets[0]
		}
		times = append(times, timeMs)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 || times[0] != 67 || times[1] != 401 {
		t.Fatalf("display sets at %v ms, want 67 and 401", times)
	}
	if first.ScreenW != 1920 || first.ScreenH != 1080 || len(first.Objects) != 2 {
		t.Fatalf("first set: plane %dx%d, %d objects; want 1920x1080 and 2", first.ScreenW, first.ScreenH, len(first.Objects))
	}
	for i, want := range []sampleCue{{x: 383, y: 956, w: 1156, h: 67}, {x: 100, y: 958, w: 123, h: 63}} {
		o := first.Objects[i]
		if got := (sampleCue{x: o.X, y: o.Y, w: o.Image.Bounds().Dx(), h: o.Image.Bounds().Dy()}); got != want {
			t.Errorf("object %d = %+v, want %+v", i, got, want)
		}
	}
	if n := planeDiff(t, first, "../../internal/testdata/pgs_sub_set0.png"); n != 0 {
		t.Errorf("the first display set differs from its golden rendering on %d pixels", n)
	}
}

// decodeTrack is what a benchmark iteration does: a fresh decoder over every block of the track.
func decodeTrack(b *testing.B, blocks []mkv.Block, decode func([]byte) ([]subtitle.BitmapDisplaySet, error)) {
	for _, blk := range blocks {
		if _, err := decode(blk.Data); err != nil {
			b.Fatal(err)
		}
	}
}

func trackBytes(blocks []mkv.Block) (n int64) {
	for _, b := range blocks {
		n += int64(len(b.Data))
	}
	return n
}

func BenchmarkVobSubTrack(b *testing.B) {
	track, blocks := sampleTrack(b, vobsubSample)
	b.SetBytes(trackBytes(blocks))
	b.ReportAllocs()
	for b.Loop() {
		d, err := subtitle.NewVobSubDecoder(track.CodecPrivate)
		if err != nil {
			b.Fatal(err)
		}
		decodeTrack(b, blocks, d.Decode)
	}
}

func BenchmarkDVBSubTrack(b *testing.B) {
	track, blocks := sampleTrack(b, dvbsubSample)
	b.SetBytes(trackBytes(blocks))
	b.ReportAllocs()
	for b.Loop() {
		d, err := subtitle.NewDVBSubDecoder(track.CodecPrivate)
		if err != nil {
			b.Fatal(err)
		}
		decodeTrack(b, blocks, d.Decode)
	}
}
