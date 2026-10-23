package mp4

import (
	"encoding/binary"
	"errors"
	"math"
)

// pcmTable is a PCM track's sample table read for its packets: one-frame samples of a constant size, which a
// movie of minutes declares by the million, are never expanded one by one.
type pcmTable struct {
	size, count uint32 // stsz's constant sample size and sample count
	chunks      []int64
	stsc        []stscEntry
	stts        []byte // stts payload, read run by run
	perBlock    uint32 // samples per packet: the most whose frames stay within pcmPacketFrames, at least one
}

// readPCMTable returns the table of a PCM track whose stsz has a constant size and which has no ctts, or false
// when the one-frame table has to be expanded (and budgeted) sample by sample.
func readPCMTable(tr *inTrack, stblBoxes []memBox) (*pcmTable, bool, error) {
	if tr.pcmFrame == 0 {
		return nil, false, nil
	}
	if _, ok := findMemBox(stblBoxes, "ctts"); ok {
		return nil, false, nil
	}
	stsz, ok := findMemBox(stblBoxes, "stsz")
	if !ok || len(stsz.payload) < 12 || binary.BigEndian.Uint32(stsz.payload[4:8]) == 0 {
		return nil, false, nil
	}
	t := &pcmTable{size: binary.BigEndian.Uint32(stsz.payload[4:8]), count: binary.BigEndian.Uint32(stsz.payload[8:12])}
	var err error
	if t.chunks, err = parseChunkOffsets(stblBoxes); err != nil {
		return nil, false, err
	}
	stsc, ok := findMemBox(stblBoxes, "stsc")
	if !ok {
		return nil, false, errf("stbl without stsc")
	}
	if t.stsc, err = parseStsc(stsc.payload); err != nil {
		return nil, false, err
	}
	if stts, ok := findMemBox(stblBoxes, "stts"); ok {
		t.stts = stts.payload
	}
	frames := max(t.size/tr.pcmFrame, 1)
	t.perBlock = max(pcmPacketFrames/frames, 1)
	return t, true, nil
}

// walkChunks calls fn with each chunk's offset, its first sample and its sample count, the counts stopping at
// stsz's; it reports how many samples the chunks cover.
func (t *pcmTable) walkChunks(fn func(off int64, first, n uint32) error) (uint32, error) {
	covered, ei := uint32(0), 0
	for ci, off := range t.chunks {
		chunk := uint32(ci + 1)
		for ei+1 < len(t.stsc) && t.stsc[ei+1].firstChunk <= chunk {
			ei++
		}
		var n uint32
		if len(t.stsc) > 0 && chunk >= t.stsc[ei].firstChunk {
			n = min(t.stsc[ei].perChunk, t.count-covered)
		}
		if n == 0 {
			continue
		}
		if err := fn(off, covered, n); err != nil {
			return covered, err
		}
		covered += n
	}
	return covered, nil
}

// blocks counts the packets the table gathers into, without building them.
func (t *pcmTable) blocks() int64 {
	var total int64
	_, _ = t.walkChunks(func(_ int64, _, n uint32) error {
		total += int64((n + t.perBlock - 1) / t.perBlock)
		return nil
	})
	return total
}

// sttsRuns hands out stts deltas run by run, as parseStts reads them: a negative delta stored unsigned is one
// tick, and samples past the table last zero.
type sttsRuns struct {
	payload   []byte
	entry     uint32
	entries   uint32
	left      uint32
	delta     int64
	exhausted bool
}

func newSttsRuns(payload []byte, n uint32) (*sttsRuns, error) {
	r := &sttsRuns{payload: payload, exhausted: true}
	if len(payload) < 8 {
		if n == 0 {
			return r, nil
		}
		return nil, errf("stts missing or too short")
	}
	r.entries = binary.BigEndian.Uint32(payload[4:8])
	if !tableFits(payload, 8, r.entries, 8) {
		return nil, errf("stts declares %d entries but is too short", r.entries)
	}
	r.exhausted = false
	return r, nil
}

// take consumes k samples' deltas and returns their sum and the last one.
func (r *sttsRuns) take(k uint32) (sum, last int64) {
	for k > 0 {
		for !r.exhausted && r.left == 0 {
			if r.entry >= r.entries {
				r.exhausted = true
				break
			}
			base := 8 + r.entry*8
			r.left = binary.BigEndian.Uint32(r.payload[base : base+4])
			d := binary.BigEndian.Uint32(r.payload[base+4 : base+8])
			if d > math.MaxInt32 {
				d = 1
			}
			r.delta = int64(d)
			r.entry++
		}
		if r.exhausted {
			return sum, 0
		}
		step := min(k, r.left)
		sum += int64(step) * r.delta
		last = r.delta
		r.left -= step
		k -= step
	}
	return sum, last
}

// endMs is the last sample's presentation time, as buildKeyframeTimes computes it, from the stts runs alone.
func (t *pcmTable) endMs(timescale uint32, editShiftMs int64) int64 {
	if t.count == 0 {
		return 0
	}
	runs, err := newSttsRuns(t.stts, t.count)
	if err != nil {
		return 0
	}
	last, _ := runs.take(t.count - 1)
	return max(ticksToMs(last, timescale)+editShiftMs, 0)
}

// errFileCut stops a PCM table walk at the end of a file that was cut short.
var errFileCut = errors.New("the file ends inside the sample table")

// buildPCMBlocks builds the packets coalescePCM would gather the one-frame table into, chunk by chunk.
func buildPCMBlocks(tr *inTrack, t *pcmTable, fileSize int64) error {
	if fileSize > 0 && int64(t.count)*int64(t.size) > fileSize {
		return errf("stsz constant-size table (%d × %d) exceeds file size %d", t.count, t.size, fileSize)
	}
	runs, err := newSttsRuns(t.stts, t.count)
	if err != nil {
		return err
	}
	ts := tr.timescale
	var out []inSample
	dts := int64(0)
	covered, err := t.walkChunks(func(off int64, first, n uint32) error {
		for j := uint32(0); j < n; j += t.perBlock {
			k := min(t.perBlock, n-j)
			pos := off + int64(j)*int64(t.size)
			end := pos + int64(k)*int64(t.size)
			if pos < 0 {
				return errf("sample %d at [%d:%d] is outside the file (size %d)", first+j, pos, end, fileSize)
			}
			if end > fileSize && len(out) > 0 {
				return errFileCut // the file was cut there: the packets before play
			}
			if end > fileSize {
				return errf("sample %d at [%d:%d] is outside the file (size %d)", first+j, pos, end, fileSize)
			}
			sum, last := runs.take(k)
			s := inSample{offset: pos, size: k * t.size, dtsMs: ticksToMs(dts, ts), ctsTicks: dts, sync: true, run: j == 0}
			s.ctsMs = max(ticksToMs(dts, ts)+tr.editShiftMs, 0)
			if k == 1 {
				s.durMs = ticksToMs(last, ts)
			} else {
				s.durMs = ticksToMs(dts+sum-last+ticksOf(ticksToMs(last, ts), ts)-dts, ts)
			}
			out = append(out, s)
			dts += sum
		}
		return nil
	})
	if errors.Is(err, errFileCut) {
		tr.samples = out
		return nil
	}
	if err != nil {
		return err
	}
	if covered != t.count {
		return errf("chunk table covers %d samples but stsz lists %d", covered, t.count)
	}
	tr.samples = out
	return nil
}
