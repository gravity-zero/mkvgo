package subtitle

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

const vobsubIdxFile = vobsubIdx + `langidx: 0
time offset: 100

id: en, index: 0
alt: English
timestamp: 00:00:01:000, filepos: 000000000
timestamp: 00:00:01:010, filepos: 000000800
delay: 00:00:10:000
timestamp: 01:02:03:004, filepos: 00000a000

id: fr, index: 1
timestamp: 00:00:05:500, filepos: 000001000
delay: -00:00:00:500
timestamp: 00:00:07:000, filepos: 000002000
`

func TestParseVobSubIndex(t *testing.T) {
	ix, err := ParseVobSubIndex(strings.NewReader(vobsubIdxFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewVobSubDecoder(ix.Header); err != nil {
		t.Errorf("the header does not make a decoder: %v", err)
	}
	if strings.Contains(string(ix.Header), "timestamp") || !strings.Contains(string(ix.Header), "langidx: 0") {
		t.Errorf("header = %q, want every line ahead of the first stream and none after", ix.Header)
	}
	want := []VobSubStream{
		{Language: "en", Name: "English", Index: 0, Entries: []VobSubEntry{{1100, 0}, {1110, 0x800}, {3723104 + 10000, 0xa000}}},
		{Language: "fr", Index: 1, Entries: []VobSubEntry{{5600, 0x1000}, {6600, 0x2000}}},
	}
	if len(ix.Streams) != len(want) {
		t.Fatalf("got %d streams, want %d", len(ix.Streams), len(want))
	}
	for i, w := range want {
		g := ix.Streams[i]
		if g.Language != w.Language || g.Name != w.Name || g.Index != w.Index || len(g.Entries) != len(w.Entries) {
			t.Errorf("stream %d = %+v, want %+v", i, g, w)
			continue
		}
		for k := range w.Entries {
			if g.Entries[k] != w.Entries[k] {
				t.Errorf("stream %d entry %d = %+v, want %+v", i, k, g.Entries[k], w.Entries[k])
			}
		}
	}
}

func TestParseVobSubIndex_Refusals(t *testing.T) {
	for name, tc := range map[string]struct{ idx, want string }{
		"no stream":        {vobsubIdx, "names no stream"},
		"bad stream":       {"id: en, index: x\n", "does not name a stream"},
		"stream past 31":   {"id: en, index: 32\n", "does not name a stream"},
		"orphan timestamp": {"timestamp: 00:00:01:000, filepos: 0\n", "ahead of any"},
		"bad time":         {"id: en, index: 0\ntimestamp: 00:00:01, filepos: 0\n", "is not \"timestamp:"},
		"bad position":     {"id: en, index: 0\ntimestamp: 00:00:01:000, filepos: zz\n", "is not \"timestamp:"},
		"bad delay":        {"id: en, index: 0\ndelay: soon\n", "is not HH:MM:SS:mmm"},
		"endless line":     {"id: en, index: 0\n" + strings.Repeat("x", vobsubMaxIndexLine+1), "token too long"},
	} {
		if _, err := ParseVobSubIndex(strings.NewReader(tc.idx)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

// subPack is one pack of a .sub: a pack header, then a private-stream packet carrying payload for a substream.
func subPack(substream byte, pts bool, payload []byte) []byte {
	head := []byte{0x80, 0x00, 0x00}
	if pts {
		head = []byte{0x80, 0x80, 0x05, 0x21, 0x00, 0x01, 0x00, 0x01}
	}
	pes := append(append(head, substream), payload...)
	out := []byte{0, 0, 1, 0xba, 0x44, 0, 4, 0, 4, 1, 1, 0x89, 0xc3, 0xf8}
	return append(append(out, 0, 0, 1, 0xbd, byte(len(pes)>>8), byte(len(pes))), pes...)
}

func subPadding(n int) []byte {
	return append([]byte{0, 0, 1, 0xbe, byte(n >> 8), byte(n)}, bytes.Repeat([]byte{0xff}, n)...)
}

// A unit over two packs, the other language's pack and padding between them; the index also lists the second pack.
func TestVobSubIndex_ForEachUnit(t *testing.T) {
	en := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)}, spuSeq{date: 300, cmds: []byte{spuStop}})
	fr := vobsubSPU(nil, nil, spuSeq{date: 45, cmds: []byte{spuStop}})
	first := subPack(0x20, true, en[:9])
	other := subPack(0x21, true, fr)
	pad := subPadding(5)
	second := subPack(0x20, false, en[9:])
	third := subPack(0x20, true, en)
	sub := concat(first, other, pad, second, third)
	at := func(parts ...[]byte) int64 { return int64(len(concat(parts...))) }
	ix := &VobSubIndex{Streams: []VobSubStream{
		{Language: "en", Index: 0, Entries: []VobSubEntry{{1000, 0}, {1000, at(first, other, pad)}, {5000, at(first, other, pad, second)}}},
		{Language: "fr", Index: 1, Entries: []VobSubEntry{{2000, at(first)}}},
	}}
	type got struct {
		timeMs int64
		unit   []byte
	}
	read := func(stream int) (out []got) {
		t.Helper()
		err := ix.ForEachUnit(bytes.NewReader(sub), stream, func(timeMs int64, unit []byte) error {
			out = append(out, got{timeMs, unit})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	units := read(0)
	if len(units) != 2 || units[0].timeMs != 1000 || units[1].timeMs != 5000 || !bytes.Equal(units[0].unit, en) || !bytes.Equal(units[1].unit, en) {
		t.Fatalf("stream 0 gave %d units (%+v), want the unit at 1000 ms and the one at 5000 ms", len(units), units)
	}
	if units := read(1); len(units) != 1 || units[0].timeMs != 2000 || !bytes.Equal(units[0].unit, fr) {
		t.Fatalf("stream 1 gave %+v, want its one unit", units)
	}
	sets, err := newVobSub(t).Decode(units[0].unit)
	if err != nil || len(sets) != 1 || len(sets[0].Objects) != 1 {
		t.Errorf("the unit read from the .sub decodes to %d sets, err %v", len(sets), err)
	}
	stop := errors.New("stop")
	if err := ix.ForEachUnit(bytes.NewReader(sub), 0, func(int64, []byte) error { return stop }); err != stop {
		t.Errorf("err = %v, want the callback's own", err)
	}
}

// An index entry at the end of the .sub, which some tools write last, is no unit and no error.
func TestVobSubIndex_ForEachUnit_EntryPastTheEnd(t *testing.T) {
	unit := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)})
	sub := subPack(0x20, true, unit)
	ix := &VobSubIndex{Streams: []VobSubStream{{Entries: []VobSubEntry{{1000, 0}, {2000, int64(len(sub))}, {3000, 1 << 20}}}}}
	n := 0
	err := ix.ForEachUnit(bytes.NewReader(sub), 0, func(int64, []byte) error { n++; return nil })
	if err != nil || n != 1 {
		t.Errorf("%d units, err %v; want the one unit the .sub holds", n, err)
	}
}

func TestVobSubIndex_ForEachUnit_Refusals(t *testing.T) {
	unit := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)})
	one := func(pos int64) *VobSubIndex {
		return &VobSubIndex{Streams: []VobSubStream{{Index: 0, Entries: []VobSubEntry{{1000, pos}}}}}
	}
	mpeg1 := subPack(0x20, true, unit)
	mpeg1[4] = 0x21
	for name, tc := range map[string]struct {
		sub    []byte
		ix     *VobSubIndex
		stream int
		want   string
	}{
		"no such stream":        {nil, one(0), 3, "stream 3 of an index of 1"},
		"not a packet":          {[]byte("RIFF....WAVE"), one(0), 0, "does not start an MPEG packet"},
		"MPEG-1 pack":           {mpeg1, one(0), 0, "not an MPEG-2 program stream pack"},
		"unit cut short":        {subPack(0x20, true, unit[:9]), one(0), 0, "the file ends 9 bytes into a unit of"},
		"packet cut short":      {subPack(0x20, true, unit)[:30], one(0), 0, "cut short"},
		"program end":           {concat(subPack(0x20, true, unit[:9]), []byte{0, 0, 1, 0xb9, 0, 0}), one(0), 0, "the stream ends 9 bytes into a unit"},
		"empty packet":          {[]byte{0, 0, 1, 0xbd, 0, 3, 0x80, 0, 0}, one(0), 0, "holds no payload"},
		"unit of no size":       {subPack(0x20, true, []byte{0, 2, 0, 0}), one(0), 0, "the unit declares 2 bytes"},
		"only another language": {bytes.Repeat(subPack(0x21, true, unit), 1), one(0), 0, "no packet at byte"},
	} {
		err := tc.ix.ForEachUnit(bytes.NewReader(tc.sub), tc.stream, func(int64, []byte) error { return nil })
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

// supFile wraps display set segments as a .sup holds them, every segment of a set at the set's time.
func supFile(sets ...[]byte) []byte {
	var out []byte
	for i, set := range sets {
		pts := uint32(i+1) * 90000
		for len(set) > 0 {
			n := 3 + int(set[1])<<8 + int(set[2])
			out = append(out, 'P', 'G', byte(pts>>24), byte(pts>>16), byte(pts>>8), byte(pts), 0, 0, 0, 0)
			out = append(out, set[:n]...)
			set = set[n:]
		}
	}
	return out
}

func TestForEachSUPDisplaySet(t *testing.T) {
	show := concat(pgsPCS(1920, 1080, pgsEpochStart, 0, synthObject{id: 1, x: 10, y: 20}), pgsWDS(10, 20, 4, 2),
		pgsPDS(0, palWhite, palBlue), pgsODS(1, 4, 2, rle4x2), pgsEND())
	clear := concat(pgsPCS(1920, 1080, 0, 0), pgsEND())
	noEnd := clear[:len(clear)-len(pgsEND())]
	type got struct {
		timeMs int64
		set    []byte
	}
	read := func(file []byte) (out []got, err error) {
		err = ForEachSUPDisplaySet(bytes.NewReader(file), func(timeMs int64, set []byte) error {
			out = append(out, got{timeMs, append([]byte(nil), set...)})
			return nil
		})
		return out, err
	}
	sets, err := read(supFile(show, clear, noEnd))
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 3 || sets[0].timeMs != 1000 || sets[1].timeMs != 2000 || sets[2].timeMs != 3000 {
		t.Fatalf("got %d sets (%v), want three at 1, 2 and 3 s", len(sets), sets)
	}
	if !bytes.Equal(sets[0].set, show) || !bytes.Equal(sets[1].set, clear) || !bytes.Equal(sets[2].set, noEnd) {
		t.Errorf("a display set is not the segments the file holds for it")
	}
	d := NewPGSDecoder()
	if ds, err := d.Decode(sets[0].set); err != nil || len(ds) != 1 || len(ds[0].Objects) != 1 {
		t.Errorf("the first set decodes to %d sets, err %v", len(ds), err)
	}

	file := supFile(show, clear)
	for name, tc := range map[string]struct {
		file []byte
		sets int
		want string
	}{
		"cut in a header":  {file[:len(file)-len(clear)-20+5], 1, "is cut short in its header"},
		"cut in a segment": {file[:len(supFile(show))+15], 1, "is cut short in its"},
		"not a .sup":       {[]byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"), 0, "the file is not a .sup"},
	} {
		sets, err := read(tc.file)
		if err == nil || !strings.Contains(err.Error(), tc.want) || len(sets) != tc.sets {
			t.Errorf("%s: %d sets, err = %v; want %d sets and an error naming %q", name, len(sets), err, tc.sets, tc.want)
		}
	}
	if sets, err := read(nil); err != nil || len(sets) != 0 {
		t.Errorf("an empty file: %d sets, err %v", len(sets), err)
	}
	stop := io.ErrClosedPipe
	if err := ForEachSUPDisplaySet(bytes.NewReader(file), func(int64, []byte) error { return stop }); err != stop {
		t.Errorf("err = %v, want the callback's own", err)
	}
}

func TestForEachSUPDisplaySet_Bounded(t *testing.T) {
	seg := pgsSegment(pgsSegODS, make([]byte, 65535))
	var sets [][]byte
	big := bytes.Repeat(seg, supMaxDisplaySet/len(seg)+1)
	sets = append(sets, big)
	err := ForEachSUPDisplaySet(bytes.NewReader(supFile(sets...)), func(int64, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "holds more than") {
		t.Errorf("err = %v, want the display set bound", err)
	}
}
