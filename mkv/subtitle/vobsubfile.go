package subtitle

// vobsubfile.go - VobSub as the two files that sit beside a video: the .idx, a text file holding the header a
// VobSubDecoder takes and, per language, the time and the position of each subpicture; and the .sub, an MPEG
// program stream whose private-stream packets carry the subpicture units, a unit over as many packets as it needs.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// vobsubMaxIndexLine bounds one line of a .idx; the palette line, the longest, is under 200 bytes.
	vobsubMaxIndexLine = 64 << 10
	// vobsubMaxUnitScan bounds the bytes of the .sub read for one unit: its 64 KiB at most, in 2 KiB packs
	// interleaved with those of the other languages.
	vobsubMaxUnitScan = 4 << 20
)

// VobSubIndex is a parsed .idx file.
type VobSubIndex struct {
	// Header is the part of the file ahead of the first language: what NewVobSubDecoder takes.
	Header  []byte
	Streams []VobSubStream
}

// VobSubStream is one language of a .idx/.sub pair.
type VobSubStream struct {
	Language string // the id: of the stream, as written ("en", "fr")
	Name     string // its alt: line, when it has one
	// Index is the stream's number in the .sub, the low bits of its packets' substream ID.
	Index   int
	Entries []VobSubEntry
}

// VobSubEntry places one subpicture: when it is shown and where its first packet starts in the .sub.
type VobSubEntry struct {
	TimeMs int64
	Pos    int64
}

// ParseVobSubIndex reads a .idx file. An entry's time has the stream's delay: lines and the header's
// time offset: applied.
func ParseVobSubIndex(r io.Reader) (*VobSubIndex, error) {
	ix := &VobSubIndex{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), vobsubMaxIndexLine)
	var offsetMs, delayMs int64
	var cur *VobSubStream
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, ":")
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		if !ok || strings.HasPrefix(line, "#") {
			key = ""
		}
		switch key {
		case "id":
			lang, rest, _ := strings.Cut(val, ",")
			_, num, _ := strings.Cut(rest, ":")
			index, err := strconv.Atoi(strings.TrimSpace(num))
			if err != nil || index < 0 || index > 31 {
				return nil, fmt.Errorf("VobSub index, line %d: %q does not name a stream as \"id: xx, index: 0..31\"", n, line)
			}
			ix.Streams = append(ix.Streams, VobSubStream{Language: strings.TrimSpace(lang), Index: index})
			cur, delayMs = &ix.Streams[len(ix.Streams)-1], 0
			continue
		case "time offset":
			if cur == nil {
				offsetMs, _ = strconv.ParseInt(val, 10, 64)
			}
		case "alt":
			if cur != nil {
				cur.Name = val
				continue
			}
		case "delay":
			if cur != nil {
				d, ok := parseVobSubTime(val)
				if !ok {
					return nil, fmt.Errorf("VobSub index, line %d: delay %q is not HH:MM:SS:mmm", n, val)
				}
				delayMs += d
				continue
			}
		case "timestamp":
			if cur == nil {
				return nil, fmt.Errorf("VobSub index, line %d: a timestamp ahead of any \"id:\" line", n)
			}
			ts, rest, _ := strings.Cut(val, ",")
			_, hex, _ := strings.Cut(rest, ":")
			t, okT := parseVobSubTime(strings.TrimSpace(ts))
			pos, err := strconv.ParseInt(strings.TrimSpace(hex), 16, 64)
			if !okT || err != nil || pos < 0 {
				return nil, fmt.Errorf("VobSub index, line %d: %q is not \"timestamp: HH:MM:SS:mmm, filepos: hex\"", n, line)
			}
			cur.Entries = append(cur.Entries, VobSubEntry{TimeMs: t + delayMs + offsetMs, Pos: pos})
			continue
		}
		if cur == nil {
			ix.Header = append(append(ix.Header, line...), '\n')
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("VobSub index: %w", err)
	}
	if len(ix.Streams) == 0 {
		return nil, fmt.Errorf("VobSub index: the file names no stream (no \"id:\" line): it is not a .idx")
	}
	return ix, nil
}

// parseVobSubTime reads [-]HH:MM:SS:mmm as milliseconds.
func parseVobSubTime(s string) (int64, bool) {
	neg := strings.HasPrefix(s, "-")
	parts := strings.Split(strings.TrimPrefix(s, "-"), ":")
	if len(parts) != 4 {
		return 0, false
	}
	var ms int64
	for i, scale := range [4]int64{3600000, 60000, 1000, 1} {
		v, err := strconv.ParseInt(strings.TrimSpace(parts[i]), 10, 32)
		if err != nil || v < 0 {
			return 0, false
		}
		ms += v * scale
	}
	if neg {
		ms = -ms
	}
	return ms, true
}

// ForEachUnit reads from the .sub every subpicture unit of one stream of the index, in the index's order, and
// calls fn with its time and its bytes: what VobSubDecoder.Decode takes as a block. An entry placed at or past the
// end of the .sub is passed over; a unit the .sub holds in part is an error. An error from fn stops the read and is
// returned as is.
func (ix *VobSubIndex) ForEachUnit(sub io.ReaderAt, stream int, fn func(timeMs int64, unit []byte) error) error {
	if stream < 0 || stream >= len(ix.Streams) {
		return fmt.Errorf("VobSub: stream %d of an index of %d", stream, len(ix.Streams))
	}
	s := &ix.Streams[stream]
	end := int64(-1)
	for _, e := range s.Entries {
		if e.Pos < end { // an index entry for a later packet of the unit before
			continue
		}
		unit, next, err := readVobSubUnit(sub, e.Pos, byte(0x20+s.Index))
		if err == errVobSubPastEnd {
			continue
		}
		if err != nil {
			return fmt.Errorf("VobSub: the unit at %d ms (byte %d of the .sub): %w", e.TimeMs, e.Pos, err)
		}
		end = next
		if err := fn(e.TimeMs, unit); err != nil {
			return err
		}
	}
	return nil
}

// errVobSubPastEnd is an index entry placed at or past the end of the .sub, as some tools write a last one.
var errVobSubPastEnd = errors.New("the entry is past the end of the .sub")

// readVobSubUnit gathers the unit that starts in the pack at pos from the packets of one substream, and returns it
// with the position after its last packet.
func readVobSubUnit(sub io.ReaderAt, pos int64, substream byte) ([]byte, int64, error) {
	var unit []byte
	want := -1
	var head [14]byte
	for start := pos; pos-start < vobsubMaxUnitScan; {
		if n, err := sub.ReadAt(head[:6], pos); err != nil {
			if n == 0 && pos == start && errors.Is(err, io.EOF) {
				return nil, 0, errVobSubPastEnd
			}
			if errors.Is(err, io.EOF) && want >= 0 {
				return nil, 0, fmt.Errorf("the file ends %d bytes into a unit of %d", len(unit), want)
			}
			return nil, 0, fmt.Errorf("no packet at byte %d: %w", pos, err)
		}
		if head[0] != 0 || head[1] != 0 || head[2] != 1 {
			return nil, 0, fmt.Errorf("byte %d does not start an MPEG packet", pos)
		}
		switch head[3] {
		case 0xba: // pack header: 14 bytes and the stuffing its last byte counts
			if _, err := sub.ReadAt(head[:], pos); err != nil {
				return nil, 0, fmt.Errorf("a pack header cut short at byte %d: %w", pos, err)
			}
			if head[4]>>6 != 1 {
				return nil, 0, fmt.Errorf("the pack at byte %d is not an MPEG-2 program stream pack", pos)
			}
			pos += 14 + int64(head[13]&7)
		case 0xb9: // program end
			return nil, 0, fmt.Errorf("the stream ends %d bytes into a unit of %d", len(unit), want)
		case 0xbd: // private stream 1: the subpictures
			size := int(binary.BigEndian.Uint16(head[4:6]))
			pkt := make([]byte, size)
			if _, err := sub.ReadAt(pkt, pos+6); err != nil {
				return nil, 0, fmt.Errorf("a packet of %d bytes cut short at byte %d: %w", size, pos, err)
			}
			pos += 6 + int64(size)
			if size < 4 || 3+int(pkt[2]) >= size {
				return nil, 0, fmt.Errorf("a packet of %d bytes holds no payload", size)
			}
			if pkt[3+int(pkt[2])] != substream {
				continue // another language
			}
			unit = append(unit, pkt[4+int(pkt[2]):]...)
			if want < 0 && len(unit) >= 2 {
				want = int(binary.BigEndian.Uint16(unit))
				if want < 4 {
					return nil, 0, fmt.Errorf("the unit declares %d bytes", want)
				}
			}
			if want >= 0 && len(unit) >= want {
				return unit[:want], pos, nil
			}
		default: // system header, padding, any other stream
			pos += 6 + int64(binary.BigEndian.Uint16(head[4:6]))
		}
	}
	return nil, 0, fmt.Errorf("the unit is not complete within %d bytes of the .sub", vobsubMaxUnitScan)
}
