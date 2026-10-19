package mp4

import (
	"bytes"
	"context"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

type nopCloseReader struct{ *bytes.Reader }

func (nopCloseReader) Close() error { return nil }

// TestMP4PlanReadsNoSubtitleText: an MP4 plan lists its subtitle samples from the sample table and reads none of them.
func TestMP4PlanReadsNoSubtitleText(t *testing.T) {
	plan, err := PlanHLS(context.Background(), pkgcovMP4SourceWithSubAndAC3(t), Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.mp4subs) != 1 || len(plan.mp4subs[0].state) != 2 {
		t.Fatalf("subtitle tables = %+v, want one track of 2 samples", plan.mp4subs)
	}
	for j, s := range plan.mp4subs[0].state {
		if s != subUnread {
			t.Errorf("sample %d read at plan time (state %d)", j, s)
		}
	}
	vtt, err := plan.subtitleSegment(context.Background(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "00:00:00.500 --> 00:00:01.500\ncue one\n\n00:00:01.500 --> 00:00:02.368\ncue two\n"; !bytes.Contains(vtt, []byte(want)) {
		t.Errorf("segment 2 = %q, want both cues (cue one spans its 1000 ms start, cue two ends with the film)", vtt)
	}
}

// TestMP4SubWindowMatchesWholeTrackDecode: every window equals decoding the whole track, SettleCueEnds and the overlap filter, the old plan-time path.
func TestMP4SubWindowMatchesWholeTrackDecode(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var file []byte
	type smp struct {
		off            int64
		size           uint32
		startMs, durMs int64
	}
	var all []smp
	at := int64(0)
	for i := 0; i < 300; i++ {
		file = append(file, make([]byte, rng.Intn(5000))...) // other tracks' media between the cues
		var data []byte
		switch rng.Intn(5) {
		case 0:
			data = []byte{0, 0} // tx3g clear sample
		case 1:
			data = []byte{0, 0, 0, 0, 0, 12, 's', 't', 'y', 'l', 0, 0} // no text, a style box: decodes to nothing after a read
		default:
			txt := []byte{byte('a' + i%26), byte('a' + i/26)}
			data = append([]byte{0, byte(len(txt))}, txt...)
		}
		at += int64(rng.Intn(3000))
		dur := int64(rng.Intn(2500))
		if rng.Intn(4) == 0 {
			dur = 0 // ended by the next cue
		}
		all = append(all, smp{off: int64(len(file)), size: uint32(len(data)), startMs: at, durMs: dur})
		file = append(file, data...)
	}

	var want []subtitle.Cue
	for _, s := range all {
		if text, ok := decodeSubtitleSample("tx3g", file[s.off:s.off+int64(s.size)]); ok {
			c := subtitle.Cue{StartMs: s.startMs, Text: string(text)}
			if s.durMs > 0 {
				c.EndMs = s.startMs + s.durMs
			}
			want = append(want, c)
		}
	}
	subtitle.SettleCueEnds(want, 0)

	table := func() *mp4SubTable {
		tb := &mp4SubTable{codec: "tx3g"}
		for _, s := range all {
			if s.size <= minSubSampleSize("tx3g") {
				continue
			}
			tb.samples = append(tb.samples, fragSample{size: s.size})
			tb.offs = append(tb.offs, s.off)
			tb.startMs = append(tb.startMs, s.startMs)
			tb.durMs = append(tb.durMs, s.durMs)
		}
		tb.state = make([]uint8, len(tb.samples))
		tb.text = make([]string, len(tb.samples))
		return tb
	}
	open := func(context.Context) (mkv.ReadSeekCloser, error) {
		return nopCloseReader{bytes.NewReader(file)}, nil
	}
	shared := table()
	for n := 0; n < 400; n++ {
		from := rng.Int63n(at + 5000)
		to := from + rng.Int63n(20000)
		if n%50 == 0 {
			from, to = math.MinInt64, math.MaxInt64
		}
		var exp []subtitle.Cue
		for _, c := range want {
			if c.EndMs > from && c.StartMs < to {
				exp = append(exp, c)
			}
		}
		for name, tb := range map[string]*mp4SubTable{"fresh": table(), "shared": shared} {
			got, err := tb.window(context.Background(), open, from, to)
			if err != nil {
				t.Fatal(err)
			}
			var in []subtitle.Cue
			for _, c := range got {
				if c.EndMs > from && c.StartMs < to {
					in = append(in, c)
				}
			}
			if !reflect.DeepEqual(in, exp) {
				t.Fatalf("%s window [%d, %d):\n got %+v\nwant %+v", name, from, to, in, exp)
			}
		}
	}
}
