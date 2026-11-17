package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

const (
	vobsubSampleIdx = "../../internal/testdata/vobsub.idx"
	vobsubSampleSub = "../../internal/testdata/vobsub.sub"
	pgsSampleSup    = "../../internal/testdata/pgs_sub.sup"
)

// copyPair copies the sample .idx/.sub pair into dir under name, the .sub through edit when it is set.
func copyPair(t *testing.T, dir, name string, edit func(sub []byte)) string {
	t.Helper()
	for _, f := range []struct{ from, ext string }{{vobsubSampleIdx, ".idx"}, {vobsubSampleSub, ".sub"}} {
		data, err := os.ReadFile(f.from)
		if err != nil {
			t.Fatal(err)
		}
		if f.ext == ".sub" && edit != nil {
			edit(data)
		}
		if err := os.WriteFile(filepath.Join(dir, name+f.ext), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, name+".idx")
}

// The pair the VobSub sample was muxed from gives the cues its Matroska track gives, pixels included, named by its
// .idx or by its .sub.
func TestForEachSubtitleBitmapFile_VobSubPair(t *testing.T) {
	ctx := context.Background()
	want := collectCues(t, func(fn func(BitmapCue) error) error { return ForEachSubtitleBitmap(ctx, vobsubSampleMKV, 1, fn) })
	dir := t.TempDir()
	idx := copyPair(t, dir, "film", nil)
	for _, path := range []string{idx, strings.TrimSuffix(idx, ".idx") + ".sub"} {
		got := collectCues(t, func(fn func(BitmapCue) error) error { return ForEachSubtitleBitmapFile(ctx, path, 0, fn) })
		sameCues(t, got, want, filepath.Base(path))
	}
}

// A .sup: two display sets of two objects each, the second cut short by the end of the file without its end
// segment, which is given as it stands and closed by the default duration.
func TestForEachSubtitleBitmapFile_Sup(t *testing.T) {
	got := collectCues(t, func(fn func(BitmapCue) error) error {
		return ForEachSubtitleBitmapFile(context.Background(), pgsSampleSup, 0, fn)
	})
	want := []cueShape{
		{67, 401, 100, 956, 1439, 67}, // two objects, composited on the rectangle that holds them
		{401, 401 + defaultSubDurationMs, 100, 956, 1439, 67},
	}
	if len(got) != len(want) {
		t.Fatalf("%d cues, want %d", len(got), len(want))
	}
	for i := range want {
		if s := shapeOf(got[i]); s != want[i] || got[i].ScreenW != 1920 || got[i].ScreenH != 1080 {
			t.Errorf("cue %d = %+v on %dx%d, want %+v on 1920x1080", i, s, got[i].ScreenW, got[i].ScreenH, want[i])
		}
	}
}

// unitStart is the offset in a .sub of the subpicture unit the pack at pos starts: past the pack header, the
// private stream packet's header and its substream byte.
func unitStart(t *testing.T, sub []byte, pos int64) int {
	t.Helper()
	p := int(pos)
	if !bytes.Equal(sub[p:p+4], []byte{0, 0, 1, 0xba}) {
		t.Fatalf("no pack at %d", p)
	}
	p += 14 + int(sub[p+13]&7)
	if !bytes.Equal(sub[p:p+4], []byte{0, 0, 1, 0xbd}) {
		t.Fatalf("no private stream packet at %d", p)
	}
	return p + 6 + 3 + int(sub[p+8]) + 1
}

// A unit of the pair that does not decode is lost alone, reported with its time and error and no byte range; a read
// that refuses any damage fails on it.
func TestForEachSubtitleBitmapFile_UndecodableUnit(t *testing.T) {
	ctx := context.Background()
	idxData, err := os.ReadFile(vobsubSampleIdx)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := subtitle.ParseVobSubIndex(bytes.NewReader(idxData))
	if err != nil {
		t.Fatal(err)
	}
	e := ix.Streams[0].Entries[10]
	idx := copyPair(t, t.TempDir(), "bad", func(sub []byte) {
		u := unitStart(t, sub, e.Pos)
		binary.BigEndian.PutUint16(sub[u+2:], 2) // control sequences at byte 2: inside the header
	})
	var skips []mkv.DamagedRange
	var got []BitmapCue
	err = ForEachSubtitleBitmapFile(ctx, idx, 0, func(c BitmapCue) error { got = append(got, c); return nil }, onSkipInto(&skips))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 42 {
		t.Errorf("%d cues, want the 42 of the 43 that decode", len(got))
	}
	if len(skips) != 1 || skips[0].ApproxStartMs != e.TimeMs || skips[0].StartOffset != -1 || skips[0].EndOffset != -1 ||
		skips[0].Err == nil || !strings.Contains(skips[0].Err.Error(), "places its control sequences at byte 2") {
		t.Errorf("OnSkip got %+v, want once the unit at %d ms, its error, no byte range", skips, e.TimeMs)
	}
	err = ForEachSubtitleBitmapFile(ctx, idx, 0, func(BitmapCue) error { return nil }, mkv.Options{DamageBytes: -1})
	if err == nil || !strings.Contains(err.Error(), "places its control sequences at byte 2") {
		t.Errorf("refusing any damage: err = %v, want the unit's own", err)
	}
}

// A file the reader cannot go through gives the cues ahead of the fault, then its error; the caller's own error
// stops the read at once.
func TestForEachSubtitleBitmapFile_FaultsAndStops(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile(pgsSampleSup)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.sup")
	second := bytes.Index(data[13:], []byte("PG")) + 13 // the second segment of the file, inside the first display set
	for end := len(data) - 1; end > second; end-- {     // cut inside the last segment of the second display set
		if !bytes.Equal(data[end-1:end+1], []byte("PG")) {
			continue
		}
		if err := os.WriteFile(cut, data[:end+20], 0o644); err != nil {
			t.Fatal(err)
		}
		break
	}
	var got []BitmapCue
	err = ForEachSubtitleBitmapFile(ctx, cut, 0, func(c BitmapCue) error { got = append(got, c); return nil })
	if err == nil || !strings.Contains(err.Error(), "cut short") || len(got) != 1 || got[0].StartMs != 67 || got[0].EndMs != 67+defaultSubDurationMs {
		t.Errorf("a .sup cut short: %d cue(s) %+v, err %v; want the first display set, then the error", len(got), got, err)
	}

	stop := errors.New("stop")
	calls := 0
	halt := func(BitmapCue) error { calls++; return stop }
	if err := ForEachSubtitleBitmapFile(ctx, pgsSampleSup, 0, halt); err != stop || calls != 1 {
		t.Errorf(".sup: err = %v after %d call(s), want the callback's own after one", err, calls)
	}
	calls = 0
	idx := copyPair(t, t.TempDir(), "film", nil)
	if err := ForEachSubtitleBitmapFile(ctx, idx, 0, halt); err != stop || calls != 1 {
		t.Errorf("pair: err = %v after %d call(s), want the callback's own after one", err, calls)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := ForEachSubtitleBitmapFile(cancelled, idx, 0, func(BitmapCue) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled read: err = %v", err)
	}
}

func TestForEachSubtitleBitmapFile_Refusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := copyPair(t, dir, "film", nil)
	if err := os.WriteFile(filepath.Join(dir, "alone.idx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	none := func(BitmapCue) error { return nil }
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"no such stream":      {ForEachSubtitleBitmapFile(ctx, idx, 1, none), "holds 1 subtitle stream(s), from 0: no stream 1"},
		"a .sup's stream 1":   {ForEachSubtitleBitmapFile(ctx, pgsSampleSup, 1, none), "holds one subtitle stream, 0, not 1"},
		"another file":        {ForEachSubtitleBitmapFile(ctx, vobsubSampleMKV, 0, none), "is not a bitmap subtitle file this reads"},
		"a .sub without .idx": {ForEachSubtitleBitmapFile(ctx, filepath.Join(dir, "lost.sub"), 0, none), "read with the .idx beside it"},
		"a .idx without .sub": {ForEachSubtitleBitmapFile(ctx, filepath.Join(dir, "alone.idx"), 0, none), "names no stream"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, tc.err, tc.want)
		}
	}
}
