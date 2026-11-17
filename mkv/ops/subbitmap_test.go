package ops

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// Real tracks: a DVD's VobSub and a broadcast's DVB subtitles, the samples of mkv/subtitle.
const (
	vobsubSampleMKV = "../../internal/testdata/vobsub.mkv"
	dvbsubSampleMKV = "../../internal/testdata/dvbsub.mkv"
)

type readBlock struct {
	timeMs, durMs int64
	data          []byte
	lost          bool
}

func collectBlocks(t *testing.T, read func(fn func(SubtitleBlock) error) error) []readBlock {
	t.Helper()
	var out []readBlock
	if err := read(func(b SubtitleBlock) error {
		out = append(out, readBlock{b.TimeMs, b.DurationMs, append([]byte(nil), b.Data...), b.Lost})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameBlocks(t *testing.T, got, want []readBlock, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d blocks, want %d", what, len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.timeMs != w.timeMs || g.durMs != w.durMs || g.lost != w.lost || !bytes.Equal(g.data, w.data) {
			t.Fatalf("%s: block %d = %d ms, %d ms long, %d bytes, lost=%v; want %d ms, %d ms, %d bytes, lost=%v",
				what, i, g.timeMs, g.durMs, len(g.data), g.lost, w.timeMs, w.durMs, len(w.data), w.lost)
		}
	}
}

// The blocks of a track come out the same walked and served from an index, whatever the codec.
func TestForEachSubtitleBlock_WalkAndIndexAgree(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		path     string
		blocks   int
		firstMs  int64
		firstLen int
		lastMs   int64
	}{
		{vobsubSampleMKV, 46, 132499, 2238, 359834},
		{dvbsubSampleMKV, 46, 0, 14, 31880},
	} {
		walked := collectBlocks(t, func(fn func(SubtitleBlock) error) error {
			return ForEachSubtitleBlock(ctx, tc.path, 1, fn)
		})
		if len(walked) != tc.blocks || walked[0].timeMs != tc.firstMs || len(walked[0].data) != tc.firstLen || walked[len(walked)-1].timeMs != tc.lastMs {
			t.Fatalf("%s: %d blocks, the first at %d ms of %d bytes, the last at %d ms; want %d, %d ms, %d bytes, %d ms",
				tc.path, len(walked), walked[0].timeMs, len(walked[0].data), walked[len(walked)-1].timeMs, tc.blocks, tc.firstMs, tc.firstLen, tc.lastMs)
		}
		ix, err := BuildSubtitleIndex(ctx, tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		indexed := collectBlocks(t, func(fn func(SubtitleBlock) error) error {
			return ForEachSubtitleBlockFrom(ctx, tc.path, 1, ix, fn)
		})
		sameBlocks(t, indexed, walked, tc.path+" from its index")
	}
}

// A track stored compressed is handed decoded, with the duration its blocks carry.
func TestForEachSubtitleBlock_DecodesACompressedTrack(t *testing.T) {
	ctx := context.Background()
	tr := mkv.Track{ID: 1, Type: mkv.SubtitleTrack, Codec: "pgs", Language: "fre", Compression: mkv.CompressionZlib}
	show, clear := pgsShow(10, 100, 900, true), pgsClear()
	path := buildCompressedSubMKV(t, t.TempDir(), "zlib.mkv", tr, [][]byte{show, clear})
	want := []readBlock{{0, 900, show, false}, {1000, 900, clear, false}}
	sameBlocks(t, collectBlocks(t, func(fn func(SubtitleBlock) error) error {
		return ForEachSubtitleBlock(ctx, path, 1, fn)
	}), want, "walk")
	ix, err := BuildSubtitleIndex(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	sameBlocks(t, collectBlocks(t, func(fn func(SubtitleBlock) error) error {
		return ForEachSubtitleBlockFrom(ctx, path, 1, ix, fn)
	}), want, "index")
}

func TestForEachSubtitleBlock_Refusals(t *testing.T) {
	ctx := context.Background()
	path := buildPGSFixture(t, t.TempDir())
	none := func(SubtitleBlock) error { return nil }
	all, err := BuildSubtitleIndex(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := BuildSubtitleIndex(ctx, path, []uint64{3})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := BuildSubtitleIndex(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale.fileSize++
	for name, tc := range map[string]struct {
		err  error
		is   error
		want string
	}{
		"walk: a video track":      {ForEachSubtitleBlock(ctx, path, 1, none), nil, "subtitle track 1 not found"},
		"walk: no such track":      {ForEachSubtitleBlock(ctx, path, 9, none), nil, "subtitle track 9 not found"},
		"index: a video track":     {ForEachSubtitleBlockFrom(ctx, path, 1, all, none), nil, "subtitle track 1 not found"},
		"index: nil":               {ForEachSubtitleBlockFrom(ctx, path, 2, nil, none), nil, "nil subtitle index"},
		"index: another file":      {ForEachSubtitleBlockFrom(ctx, path, 2, stale, none), ErrIndexStale, "built for"},
		"index: track not covered": {ForEachSubtitleBlockFrom(ctx, path, 2, text, none), ErrTrackNotIndexed, "track 2"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) || (tc.is != nil && !errors.Is(tc.err, tc.is)) {
			t.Errorf("%s: err = %v, want one naming %q", name, tc.err, tc.want)
		}
	}
	stop := errors.New("stop")
	calls := 0
	halt := func(SubtitleBlock) error { calls++; return stop }
	if err := ForEachSubtitleBlock(ctx, path, 2, halt); err != stop || calls != 1 {
		t.Errorf("walk: err = %v after %d calls, want the callback's own after one", err, calls)
	}
	calls = 0
	if err := ForEachSubtitleBlockFrom(ctx, path, 2, all, halt); err != stop || calls != 1 {
		t.Errorf("index: err = %v after %d calls, want the callback's own after one", err, calls)
	}
}

// A damaged region the read passes over comes as one Lost call, between the blocks on either side of it. A walk
// knows the region starts past the 3 s cluster; an index of the clean file seats on the 4 s cluster and finds it
// zeroed, then on the 5 s cluster's block, which lies past the zeroed bytes.
func TestForEachSubtitleBlock_LostRegion(t *testing.T) {
	ctx := context.Background()
	clean, bad, clusters := damagedSubtitleSource(t)
	ix, err := BuildSubtitleIndex(ctx, clean, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		read           func(fn func(SubtitleBlock) error, o mkv.Options) error
		lostMs, nextMs int64
	}{
		{"walk", func(fn func(SubtitleBlock) error, o mkv.Options) error {
			return ForEachSubtitleBlock(ctx, bad, 3, fn, o)
		}, 3000, 6200},
		{"index", func(fn func(SubtitleBlock) error, o mkv.Options) error {
			return ForEachSubtitleBlockFrom(ctx, bad, 3, ix, fn, o)
		}, 4000, 5200},
	} {
		var skips []mkv.DamagedRange
		var times []int64
		lostAt, lost := -1, 0
		err := tc.read(func(b SubtitleBlock) error {
			if !b.Lost {
				times = append(times, b.TimeMs)
				return nil
			}
			lost++
			lostAt = len(times)
			if b.Data != nil || b.TimeMs != tc.lostMs {
				t.Errorf("%s: the Lost call carries %d bytes at %d ms, want none at %d ms", tc.name, len(b.Data), b.TimeMs, tc.lostMs)
			}
			return nil
		}, onSkipInto(&skips))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(skips) != 1 || skips[0].EndOffset != clusters[6] || skips[0].ApproxStartMs != tc.lostMs {
			t.Errorf("%s: OnSkip got %+v, want once a region from %d ms ending at byte %d", tc.name, skips, tc.lostMs, clusters[6])
		}
		if lost != 1 || lostAt != 4 || times[3] != 3200 || times[4] != tc.nextMs {
			t.Errorf("%s: %d Lost call(s), after %d blocks, blocks at %v; want one after the block at 3200 ms, the next at %d ms", tc.name, lost, lostAt, times, tc.nextMs)
		}
	}
}

type cueShape struct {
	startMs, endMs int64
	x, y, w, h     int
}

func shapeOf(c BitmapCue) cueShape {
	return cueShape{c.StartMs, c.EndMs, c.X, c.Y, c.Image.Bounds().Dx(), c.Image.Bounds().Dy()}
}

func collectCues(t *testing.T, read func(fn func(BitmapCue) error) error) []BitmapCue {
	t.Helper()
	var out []BitmapCue
	if err := read(func(c BitmapCue) error { out = append(out, c); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameCues(t *testing.T, got, want []BitmapCue, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d cues, want %d", what, len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if shapeOf(g) != shapeOf(w) || g.ScreenW != w.ScreenW || g.ScreenH != w.ScreenH || g.Forced != w.Forced || !bytes.Equal(g.Image.Pix, w.Image.Pix) {
			t.Fatalf("%s: cue %d = %+v, want %+v, or its pixels differ", what, i, shapeOf(g), shapeOf(w))
		}
	}
}

// A DVD's VobSub track as cues: each dated by the block that starts its unit, ended by its own stop command.
func TestForEachSubtitleBitmap_VobSub(t *testing.T) {
	ctx := context.Background()
	cues := collectCues(t, func(fn func(BitmapCue) error) error { return ForEachSubtitleBitmap(ctx, vobsubSampleMKV, 1, fn) })
	if len(cues) != 43 {
		t.Fatalf("%d cues, want 43", len(cues))
	}
	for i, want := range map[int]cueShape{
		0:  {132499, 132499 + 4960, 259, 379, 200, 24},
		4:  {185919, 185919 + 2696, 234, 340, 250, 63},
		27: {302018, 302018 + 1524, 236, 379, 247, 30}, // a unit over two blocks, the second at 302035 ms
		42: {359834, 359834 + 1262, 232, 379, 254, 24},
	} {
		if got := shapeOf(cues[i]); got != want {
			t.Errorf("cue %d = %+v, want %+v", i, got, want)
		}
	}
	for i, c := range cues {
		if c.ScreenW != 720 || c.ScreenH != 480 || c.Forced || c.EndMs <= c.StartMs {
			t.Errorf("cue %d: plane %dx%d, forced=%v, %d..%d ms", i, c.ScreenW, c.ScreenH, c.Forced, c.StartMs, c.EndMs)
		}
	}
	ix, err := BuildSubtitleIndex(ctx, vobsubSampleMKV, nil)
	if err != nil {
		t.Fatal(err)
	}
	sameCues(t, collectCues(t, func(fn func(BitmapCue) error) error {
		return ForEachSubtitleBitmapFrom(ctx, vobsubSampleMKV, 1, ix, fn)
	}), cues, "from the index")
}

// A broadcast's DVB track as cues: each display set ends where the next begins, two regions are one picture.
func TestForEachSubtitleBitmap_DVB(t *testing.T) {
	ctx := context.Background()
	cues := collectCues(t, func(fn func(BitmapCue) error) error { return ForEachSubtitleBitmap(ctx, dvbsubSampleMKV, 1, fn) })
	if len(cues) != 36 {
		t.Fatalf("%d cues, want 36", len(cues))
	}
	for i, want := range map[int]cueShape{
		0:  {15600, 15760, 78, 416, 88, 36},
		6:  {17760, 17980, 78, 380, 406, 72}, // two regions, one above the other
		35: {29360, 31040, 78, 380, 460, 72}, // ended by the empty display set that follows
	} {
		if got := shapeOf(cues[i]); got != want {
			t.Errorf("cue %d = %+v, want %+v", i, got, want)
		}
	}
	for i, c := range cues {
		if c.ScreenW != 720 || c.ScreenH != 576 || c.Forced {
			t.Errorf("cue %d: plane %dx%d, forced=%v", i, c.ScreenW, c.ScreenH, c.Forced)
		}
		if i > 0 && c.StartMs != cues[i-1].EndMs {
			t.Errorf("cue %d starts at %d ms, the one before ends at %d", i, c.StartMs, cues[i-1].EndMs)
		}
	}
	ix, err := BuildSubtitleIndex(ctx, dvbsubSampleMKV, nil)
	if err != nil {
		t.Fatal(err)
	}
	sameCues(t, collectCues(t, func(fn func(BitmapCue) error) error {
		return ForEachSubtitleBitmapFrom(ctx, dvbsubSampleMKV, 1, ix, fn)
	}), cues, "from the index")
}

// The codec-neutral read gives a PGS track the cues the PGS read gives it.
func TestForEachSubtitleBitmap_PGS(t *testing.T) {
	ctx := context.Background()
	path := buildPGSFixture(t, t.TempDir())
	checkPGSCues(t, collectCues(t, func(fn func(BitmapCue) error) error { return ForEachSubtitleBitmap(ctx, path, 2, fn) }), "walk")
	ix, err := BuildSubtitleIndex(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkPGSCues(t, collectCues(t, func(fn func(BitmapCue) error) error {
		return ForEachSubtitleBitmapFrom(ctx, path, 2, ix, fn)
	}), "index")
}

func TestForEachSubtitleBitmap_Refusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := buildPGSFixture(t, dir)
	other := buildMinimalMKV(t, dir, "other.mkv", []mkv.Track{subtitleTrack(1, "S_KATE"), subtitleTrack(2, "vobsub")},
		[]mkv.Block{{TrackNumber: 1, Timecode: 0, Data: []byte("x")}}, 1000)
	none := func(BitmapCue) error { return nil }
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"a text track":              {ForEachSubtitleBitmap(ctx, path, 3, none), "codec \"srt\" is text, not bitmaps - extract it as WebVTT instead"},
		"a codec without decoder":   {ForEachSubtitleBitmap(ctx, other, 1, none), "is not a bitmap subtitle this reads (PGS, VobSub, DVB)"},
		"VobSub without its header": {ForEachSubtitleBitmap(ctx, other, 2, none), "lacks the size: or palette: line"},
		"a video track":             {ForEachSubtitleBitmap(ctx, path, 1, none), "subtitle track 1 not found"},
		"the PGS read on VobSub":    {ForEachSubtitlePGS(ctx, vobsubSampleMKV, 1, none), "codec \"vobsub\" is not PGS (S_HDMV/PGS)"},
		"index: nil":                {ForEachSubtitleBitmapFrom(ctx, path, 2, nil, none), "nil subtitle index"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, tc.err, tc.want)
		}
	}
}

// scriptedDecoder hands the assembler the display sets a test wrote for each block, by the block's first byte.
type scriptedDecoder struct {
	sets      map[byte][]subtitle.BitmapDisplaySet
	continues map[byte]bool
	resets    int
}

func (d *scriptedDecoder) Decode(block []byte) ([]subtitle.BitmapDisplaySet, error) {
	return d.sets[block[0]], nil
}
func (d *scriptedDecoder) Reset()                      { d.resets++ }
func (d *scriptedDecoder) Continues(block []byte) bool { return d.continues[block[0]] }

func shownSet(x int, more subtitle.BitmapDisplaySet) subtitle.BitmapDisplaySet {
	more.ScreenW, more.ScreenH = 720, 576
	more.Objects = []subtitle.BitmapObject{{X: x, Y: 10, Image: image.NewNRGBA(image.Rect(0, 0, 2, 2))}}
	return more
}

// The rules that end a cue, on display sets written by hand: the stream's own duration, then the block's, then the
// next display set, no later than the page's time-out.
func TestBitmapAssembler_CueEnds(t *testing.T) {
	clear := subtitle.BitmapDisplaySet{ScreenW: 720, ScreenH: 576}
	dec := &scriptedDecoder{
		sets: map[byte][]subtitle.BitmapDisplaySet{
			'a': {shownSet(1, subtitle.BitmapDisplaySet{Delay: 100 * time.Millisecond, Duration: 2 * time.Second})},
			'b': {shownSet(2, subtitle.BitmapDisplaySet{})},
			'c': {shownSet(3, subtitle.BitmapDisplaySet{Timeout: 5 * time.Second})},
			'd': {clear},
			'e': nil, // the first part of a unit
			'f': {shownSet(4, subtitle.BitmapDisplaySet{})},
			'g': {shownSet(5, subtitle.BitmapDisplaySet{Timeout: 2 * time.Second})},
		},
		continues: map[byte]bool{'f': true},
	}
	var got []cueShape
	a := &bitmapAssembler{dec: dec, name: "test", track: &mkv.Track{}, emit: func(c BitmapCue) error {
		got = append(got, cueShape{startMs: c.StartMs, endMs: c.EndMs, x: c.X})
		return nil
	}}
	for _, b := range []struct {
		timeMs, durMs int64
		data          string
	}{
		{1000, 9000, "a"}, // its own duration wins over the block's: 1100..3100
		{4000, 700, "b"},  // the block's duration: 4000..4700
		{5000, 0, "b"},    // open, ended by the next display set: 5000..6000
		{6000, 0, "c"},    // open, the next set comes after its time-out: 6000..11000
		{20000, 0, "d"},   // a clear
		{30000, 500, "e"}, // starts a unit
		{30040, 0, "f"},   // completes it: dated 30000, ended by the next set at 31000
		{31000, 0, "g"},   // never closed: its time-out, shorter than the default: 31000..33000
	} {
		if err := a.block(mkv.Block{Timecode: b.timeMs, Duration: b.durMs, Data: []byte(b.data)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.finish(); err != nil {
		t.Fatal(err)
	}
	want := []cueShape{
		{startMs: 1100, endMs: 3100, x: 1},
		{startMs: 4000, endMs: 4700, x: 2},
		{startMs: 5000, endMs: 6000, x: 2},
		{startMs: 6000, endMs: 11000, x: 3},
		{startMs: 30000, endMs: 31000, x: 4},
		{startMs: 31000, endMs: 33000, x: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("%d cues %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cue %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A region the read passed over ends the cue on screen where it starts and resets the decoder.
func TestBitmapAssembler_LostRegion(t *testing.T) {
	dec := &scriptedDecoder{sets: map[byte][]subtitle.BitmapDisplaySet{'b': {shownSet(2, subtitle.BitmapDisplaySet{})}}}
	var got []cueShape
	a := &bitmapAssembler{dec: dec, name: "test", track: &mkv.Track{}, emit: func(c BitmapCue) error {
		got = append(got, cueShape{startMs: c.StartMs, endMs: c.EndMs})
		return nil
	}}
	if err := a.block(mkv.Block{Timecode: 1000, Data: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if err := a.lost(1800); err != nil {
		t.Fatal(err)
	}
	if err := a.block(mkv.Block{Timecode: 9000, Data: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if err := a.finish(); err != nil {
		t.Fatal(err)
	}
	want := []cueShape{{startMs: 1000, endMs: 1800}, {startMs: 9000, endMs: 9000 + defaultSubDurationMs}}
	if dec.resets != 1 || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("%d reset(s), cues %+v; want one reset and %+v", dec.resets, got, want)
	}
}

// zeroedCopy copies a sample with n bytes zeroed from its middle.
func zeroedCopy(t *testing.T, src string, n int) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	clear(data[len(data)/2 : len(data)/2+n])
	path := filepath.Join(t.TempDir(), "zeroed.mkv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A block that reads but does not decode is lost alone: a VobSub track with 4096 bytes zeroed in the middle of the
// file gives every cue but the two the zeros hit, where it gave none.
func TestForEachSubtitleBitmap_UndecodableBlockIsLostAlone(t *testing.T) {
	ctx := context.Background()
	bad := zeroedCopy(t, vobsubSampleMKV, 4096)
	whole := collectCues(t, func(fn func(BitmapCue) error) error { return ForEachSubtitleBitmap(ctx, vobsubSampleMKV, 1, fn) })
	ix, err := BuildSubtitleIndex(ctx, vobsubSampleMKV, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(fn func(BitmapCue) error, o mkv.Options) error{
		"walk": func(fn func(BitmapCue) error, o mkv.Options) error { return ForEachSubtitleBitmap(ctx, bad, 1, fn, o) },
		"index": func(fn func(BitmapCue) error, o mkv.Options) error {
			return ForEachSubtitleBitmapFrom(ctx, bad, 1, ix, fn, o)
		},
	} {
		var skips []mkv.DamagedRange
		var cues []BitmapCue
		if err := read(func(c BitmapCue) error { cues = append(cues, c); return nil }, onSkipInto(&skips)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(cues) != 41 || len(skips) == 0 {
			t.Fatalf("%s: %d cues of %d and %d region(s) reported; want 41 and at least one", name, len(cues), len(whole), len(skips))
		}
		// Every cue that came out is one of the whole track's, pixels included.
		byStart := map[int64]BitmapCue{}
		for _, c := range whole {
			byStart[c.StartMs] = c
		}
		for _, c := range cues {
			w, ok := byStart[c.StartMs]
			if !ok || shapeOf(c) != shapeOf(w) || !bytes.Equal(c.Image.Pix, w.Image.Pix) {
				t.Errorf("%s: the cue at %d ms is not the whole track's", name, c.StartMs)
			}
		}
		// A read that refuses any damage still fails, on the block and by its time.
		err := read(func(BitmapCue) error { return nil }, mkv.Options{DamageBytes: -1})
		if err == nil {
			t.Errorf("%s: a read refusing any damage passed over the zeroed bytes", name)
		}
	}
}

// pgsDamagedFixture is buildPGSFixture's track with a block that reads but is no display set between its cues.
func pgsDamagedFixture(t *testing.T) string {
	t.Helper()
	tracks := []mkv.Track{subtitleTrack(1, "pgs")}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Data: pgsShow(10, 100, 900, true)}, // open: ended by the block that does not decode
		{TrackNumber: 1, Timecode: 700, Data: []byte{0x16, 0xff, 0xff, 0x00}},
		{TrackNumber: 1, Timecode: 1000, Duration: 400, Data: pgsShow(11, 200, 950, true)},
		{TrackNumber: 1, Timecode: 2000, Data: pgsShow(12, 300, 960, false)},
	}
	return buildMinimalMKV(t, t.TempDir(), "pgsbad.mkv", tracks, blocks, 3000)
}

func TestForEachSubtitleBitmap_UndecodablePGSBlock(t *testing.T) {
	ctx := context.Background()
	path := pgsDamagedFixture(t)
	want := []cueShape{{0, 700, 100, 900, 4, 2}, {1000, 1400, 200, 950, 4, 2}, {2000, 2000 + defaultSubDurationMs, 300, 960, 4, 2}}
	ix, err := BuildSubtitleIndex(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(fn func(PGSCue) error, o mkv.Options) error{
		"bitmap walk": func(fn func(PGSCue) error, o mkv.Options) error { return ForEachSubtitleBitmap(ctx, path, 1, fn, o) },
		"pgs walk":    func(fn func(PGSCue) error, o mkv.Options) error { return ForEachSubtitlePGS(ctx, path, 1, fn, o) },
		"bitmap index": func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitleBitmapFrom(ctx, path, 1, ix, fn, o)
		},
		"pgs index": func(fn func(PGSCue) error, o mkv.Options) error {
			return ForEachSubtitlePGSFrom(ctx, path, 1, ix, fn, o)
		},
	} {
		var skips []mkv.DamagedRange
		var got []cueShape
		if err := read(func(c PGSCue) error { got = append(got, shapeOf(c)); return nil }, onSkipInto(&skips)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("%s: cues %+v, want %+v", name, got, want)
		}
		if len(skips) != 1 || skips[0].ApproxStartMs != 700 || skips[0].ApproxEndMs != 700 || skips[0].EndOffset-skips[0].StartOffset != 4 || skips[0].StartOffset <= 0 {
			t.Errorf("%s: OnSkip got %+v, want once the 4 bytes of the block at 700 ms", name, skips)
		}
		err := read(func(PGSCue) error { return nil }, mkv.Options{DamageBytes: -1})
		if err == nil || !strings.Contains(err.Error(), "subtitle block at 700 ms: PGS: a 0x16 segment declares 65535 bytes") {
			t.Errorf("%s: refusing any damage, err = %v; want the block's own", name, err)
		}
		// What the caller's function returns stays fatal, also from the cue the lost block closes.
		stop := errors.New("stop")
		if err := read(func(PGSCue) error { return stop }, mkv.Options{}); err != stop {
			t.Errorf("%s: err = %v, want the callback's own", name, err)
		}
	}
	// So does a cancelled context.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := ForEachSubtitleBitmap(cancelled, path, 1, func(PGSCue) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled read: err = %v, want context.Canceled", err)
	}
	if err := ForEachSubtitleBitmapFrom(cancelled, path, 1, ix, func(PGSCue) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled indexed read: err = %v, want context.Canceled", err)
	}
}

// failingDecoder fails on blocks starting with '!' after handing back the sets it had, as the DVB decoder does.
type failingDecoder struct {
	scriptedDecoder
}

func (d *failingDecoder) Decode(block []byte) ([]subtitle.BitmapDisplaySet, error) {
	if block[0] == '!' {
		return d.sets['b'], errors.New("a segment that cannot be read")
	}
	return d.scriptedDecoder.Decode(block)
}

// The sets a decoder returns with its error are dropped with the block, the decoder is reset and the cue on screen
// ends at the block's time; a display set that cannot be composited is lost the same way.
func TestBitmapAssembler_UndecodableBlock(t *testing.T) {
	far := subtitle.BitmapDisplaySet{ScreenW: 720, ScreenH: 576, Objects: []subtitle.BitmapObject{
		{X: 0, Y: 0, Image: image.NewNRGBA(image.Rect(0, 0, 1, 1))},
		{X: bitmapMaxPlane + 1, Y: 0, Image: image.NewNRGBA(image.Rect(0, 0, 1, 1))},
	}}
	dec := &failingDecoder{scriptedDecoder{sets: map[byte][]subtitle.BitmapDisplaySet{
		'b': {shownSet(2, subtitle.BitmapDisplaySet{})},
		'f': {shownSet(3, subtitle.BitmapDisplaySet{}), far},
	}}}
	var got []cueShape
	var skips []mkv.DamagedRange
	a := &bitmapAssembler{dec: dec, name: "test", track: &mkv.Track{}, onSkip: func(r mkv.DamagedRange) { skips = append(skips, r) },
		emit: func(c BitmapCue) error {
			got = append(got, cueShape{startMs: c.StartMs, endMs: c.EndMs, x: c.X})
			return nil
		}}
	for _, b := range []mkv.Block{
		{Timecode: 1000, Data: []byte("b"), DataOffset: 100},
		{Timecode: 1500, Data: []byte("!x"), DataOffset: 200}, // fails: its sets dropped, the cue of 1000 ends here
		{Timecode: 2000, Data: []byte("b"), DataOffset: 300},
		{Timecode: 2600, Data: []byte("fyz"), DataOffset: 400}, // its second set cannot be composited: the whole block is lost
		{Timecode: 3000, Data: []byte("b"), DataOffset: 500},
	} {
		if err := a.block(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.finish(); err != nil {
		t.Fatal(err)
	}
	want := []cueShape{{startMs: 1000, endMs: 1500, x: 2}, {startMs: 2000, endMs: 2600, x: 2}, {startMs: 3000, endMs: 3000 + defaultSubDurationMs, x: 2}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("cues %+v, want %+v", got, want)
	}
	wantSkips := []mkv.DamagedRange{{StartOffset: 200, EndOffset: 202, ApproxStartMs: 1500, ApproxEndMs: 1500}, {StartOffset: 400, EndOffset: 403, ApproxStartMs: 2600, ApproxEndMs: 2600}}
	if dec.resets != 2 || len(skips) != 2 || skips[0] != wantSkips[0] || skips[1] != wantSkips[1] {
		t.Errorf("%d reset(s), OnSkip %+v; want 2 and %+v", dec.resets, skips, wantSkips)
	}

	a.strict = true
	if err := a.block(mkv.Block{Timecode: 4000, Data: []byte("!")}); err == nil || !strings.Contains(err.Error(), "subtitle block at 4000 ms: a segment that cannot be read") {
		t.Errorf("refusing any damage, err = %v; want the block's own", err)
	}
}
