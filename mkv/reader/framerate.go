package reader

import (
	"bytes"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// headWindow is what a remote probe's first request fetches from the start of
// the file; the frame-rate estimate reads no block header past it.
const headWindow = 512 << 10

// maxRateFrames caps the timestamps kept per track for the estimate.
const maxRateFrames = 64

// standardRates are the frame rates an estimate may report, as num/den.
var standardRates = [][2]int64{
	{24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {48, 1}, {50, 1},
	{60000, 1001}, {60, 1}, {100, 1}, {120000, 1001}, {120, 1},
}

// estimateFrameRates sets EstimatedFrameRate on the video tracks without DefaultDuration from the blocks of the cluster whose body starts at body (ending at end, -1 unknown) and of the clusters after it.
func (p *parser) estimateFrameRates(c *mkv.Container, body, end int64) {
	want := map[uint64][]int64{}
	for i, t := range c.Tracks {
		if _, stated := codecFrameRate(&c.Tracks[i]); t.Type == mkv.VideoTrack && t.FrameRate == nil && !stated {
			want[t.ID] = nil
		}
	}
	if len(want) == 0 || body >= headWindow {
		return
	}
	defer p.r.Seek(body, io.SeekStart) //nolint:errcheck // the caller's next read reports a failed seek
	p.collectBlockTimes(body, end, want)
	scale := c.Info.TimecodeScale
	if scale == 0 {
		scale = 1_000_000
	}
	for i := range c.Tracks {
		if ts, ok := want[c.Tracks[i].ID]; ok {
			if r, ok := fitFrameRate(ts, scale); ok {
				c.Tracks[i].EstimatedFrameRate = &r
			}
		}
	}
}

// wantsFrameRate reports whether the head scan should go on to the first cluster for the frame-rate estimate: only while the next header costs no new read.
func (p *parser) wantsFrameRate(c *mkv.Container) bool {
	if p.firstCluster != 0 || p.pos() >= headWindow || !p.holds(p.pos(), 16) {
		return false
	}
	for _, t := range c.Tracks {
		if t.Type == mkv.VideoTrack && t.FrameRate == nil {
			return true
		}
	}
	return false
}

// collectBlockTimes appends to want the timestamps of the blocks met reading forward from pos, inside a cluster ending at end, whose header ends within the head window.
func (p *parser) collectBlockTimes(pos, end int64, want map[uint64][]int64) {
	full := func() bool {
		for _, ts := range want {
			if len(ts) < maxRateFrames {
				return false
			}
		}
		return true
	}
	var base, at int64
	var last []byte // the last bytes read, at at: an element shorter than them is followed from there
	buf := make([]byte, 32)
	for pos < headWindow && !full() {
		need := min(int64(len(buf)), headWindow-pos)
		switch end := at + int64(len(last)); {
		case pos >= at && pos+need <= end:
		case pos >= at && pos < end:
			last = append(last[pos-at:], p.peekAt(end, buf[:pos+need-end])...)
			at = pos
		default:
			at, last = pos, append(last[:0], p.peekAt(pos, buf[:need])...)
		}
		r := bytes.NewReader(last[pos-at:])
		eh, hl, err := ebml.ReadElementHeader(r)
		if err != nil {
			return
		}
		if eh.ID == mkv.IDCluster || end >= 0 && pos >= end { // the next cluster
			if eh.ID != mkv.IDCluster {
				return
			}
			pos += int64(hl)
			end, base = pos+eh.Size, 0
			if eh.Size < 0 {
				end = -1
			}
			continue
		}
		if eh.Size < 0 {
			return
		}
		switch eh.ID {
		case mkv.IDTimestamp:
			if eh.Size > int64(r.Len()) {
				return
			}
			u, _ := ebml.ReadUint(r, eh.Size)
			base = int64(u)
		case mkv.IDSimpleBlock, mkv.IDBlockGroup:
			if track, rel, ok := blockStamp(r, eh.ID); ok {
				if ts, w := want[track]; w && len(ts) < maxRateFrames {
					want[track] = append(ts, base+rel)
				}
			}
		}
		pos += int64(hl) + eh.Size
	}
}

// peekAt returns up to len(buf) bytes at off, from the read buffer when it holds them, else with one exact read; it never reads back.
func (p *parser) peekAt(off int64, buf []byte) []byte {
	b, ok := p.r.(*bufReadSeeker)
	if !ok {
		if !freeAt(p.r, off, int64(len(buf))) {
			return p.exactAt(p.r, off, buf)
		}
		if !p.forwardTo(off) {
			return nil
		}
		n, _ := io.ReadFull(p.r, buf)
		return buf[:n]
	}
	if off < b.off {
		return nil
	}
	if in := off - b.off; in < int64(b.br.Buffered()) { // in the buffer, or straddling its end: the read continues forward
		end := b.off + int64(b.br.Buffered())
		if off+int64(len(buf)) > end && !freeAt(b.rs, end, off+int64(len(buf))-end) {
			if d := p.exactAt(b.rs, off, buf); d != nil {
				return d
			}
			buf = buf[:end-off]
		}
		b.br.Discard(int(in)) //nolint:errcheck // within Buffered
		b.off = off
		d, _ := b.br.Peek(len(buf))
		return d
	}
	if !freeAt(b.rs, off, int64(len(buf))) {
		return p.exactAt(b.rs, off, buf)
	}
	if _, err := b.rs.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	n, _ := io.ReadFull(b.rs, buf)
	b.br.Reset(b.rs)
	b.off = off + int64(n)
	return buf[:n]
}

// clusterEnd is where a cluster whose body starts at body ends, -1 for an unknown size.
func clusterEnd(body, size int64) int64 {
	if size < 0 {
		return -1
	}
	return body + size
}

// blockStamp reads the track number and relative timestamp of a SimpleBlock, or of the Block a BlockGroup opens with.
func blockStamp(r *bytes.Reader, id uint32) (track uint64, rel int64, ok bool) {
	if id == mkv.IDBlockGroup {
		if ch, _, err := ebml.ReadElementHeader(r); err != nil || ch.ID != mkv.IDBlock {
			return 0, 0, false
		}
	}
	head := make([]byte, r.Len())
	n, _ := r.Read(head)
	raw, w := readVINTFromBuf(head[:n])
	if w == 0 || w > 8 || w+2 > n {
		return 0, 0, false
	}
	return raw & (1<<(7*w) - 1), int64(int16(uint16(head[w])<<8 | uint16(head[w+1]))), true
}

// fitFrameRate returns the one standard rate the timestamps (in ticks of scale ns) fit: on its grid from the first frame within a tick, else with 90% of them on one phase of it, a frame to a slot.
func fitFrameRate(ts []int64, scale int64) (float64, bool) {
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	var t []float64 // since the first frame, without repeats
	for i, v := range ts {
		if i == 0 || v != ts[i-1] {
			t = append(t, float64(v-ts[0]))
		}
	}
	if len(t) < 3 {
		return 0, false
	}
	pick := func(fit func([]float64, float64) bool) (out [][2]int64) {
		var fits [][2]int64
		for _, r := range standardRates {
			if fit(t, 1e9*float64(r[1])/(float64(r[0])*float64(scale))) {
				fits = append(fits, r)
			}
		}
		for _, r := range fits {
			harmonic := false
			for _, o := range fits {
				harmonic = harmonic || r != o && (r[0]*o[1])%(r[1]*o[0]) == 0
			}
			if !harmonic {
				out = append(out, r)
			}
		}
		return out
	}
	rates := pick(gridFit)
	if len(rates) == 0 {
		rates = pick(phaseFit)
	}
	if len(rates) != 1 {
		return 0, false
	}
	return float64(rates[0][0]) / float64(rates[0][1]), true
}

// gridFit reports whether every timestamp lies within a tick of the grid of frame ticks started at the first, a frame to a slot.
func gridFit(t []float64, frame float64) bool {
	lo, hi := math.Inf(1), math.Inf(-1)
	ks := make([]int64, len(t))
	for i, x := range t {
		ks[i] = int64(math.Round(x / frame))
		res := x - float64(ks[i])*frame
		lo, hi = min(lo, res), max(hi, res)
	}
	return hi-lo <= 1+1e-9 && stepsOfOne(ks)
}

// phaseFit reports whether 90% of the timestamps share one phase of the grid of frame ticks within a tick, a frame to a slot.
func phaseFit(t []float64, frame float64) bool {
	n := len(t)
	ph := make([]float64, 0, 2*n)
	for _, x := range t {
		ph = append(ph, math.Mod(x, frame))
	}
	sort.Float64s(ph)
	for i := 0; i < n; i++ {
		ph = append(ph, ph[i]+frame)
	}
	best, at := 0, 0.0
	for i, j := 0, 0; i < n; i++ {
		for j < i+n && ph[j]-ph[i] <= 1+1e-9 {
			j++
		}
		if j-i > best {
			best, at = j-i, ph[i]
		}
	}
	if best*10 < 9*n {
		return false
	}
	var ks []int64
	for _, x := range t {
		if math.Mod(math.Mod(x, frame)-at+frame, frame) <= 1+1e-9 {
			ks = append(ks, int64(math.Round((x-at)/frame)))
		}
	}
	return stepsOfOne(ks)
}

// stepsOfOne reports whether the grid slots, in order, are all distinct and mostly consecutive (median step of one).
func stepsOfOne(ks []int64) bool {
	if len(ks) < 2 {
		return false
	}
	d := make([]int64, 0, len(ks)-1)
	for i := 1; i < len(ks); i++ {
		if ks[i] == ks[i-1] {
			return false
		}
		d = append(d, ks[i]-ks[i-1])
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2] == 1
}

// forwardTo moves the read position forward to target with a relative seek, which keeps a buffered or streamed read going; it refuses to go back.
func (p *parser) forwardTo(target int64) bool {
	cur, err := p.r.Seek(0, io.SeekCurrent)
	if err != nil || target < cur {
		return false
	}
	_, err = p.r.Seek(target-cur, io.SeekCurrent)
	return err == nil
}

// freeReader is a source that tells whether bytes come without a new fetch: the HTTP reader, the WebAssembly Blob reader.
type freeReader interface{ Free(off, n int64) bool }

// freeAt reports whether n bytes at off cost rs no new read: always from memory, from another source only when it says so.
func freeAt(rs io.ReadSeeker, off, n int64) bool {
	switch f := rs.(type) {
	case freeReader:
		return f.Free(off, n)
	case *bytes.Reader:
		return true
	}
	return false
}

// exactAt reads buf at off with one exact read when WithFrameRateReads allows it and the source offers io.ReaderAt, else returns nil.
func (p *parser) exactAt(rs io.ReadSeeker, off int64, buf []byte) []byte {
	ra, ok := rs.(io.ReaderAt)
	if !p.rateReads || !ok {
		return nil
	}
	n, _ := ra.ReadAt(buf, off)
	return buf[:n]
}

// holds reports whether n bytes at off are in the read buffer or held by the source, or may be read under WithFrameRateReads.
func (p *parser) holds(off, n int64) bool {
	if p.rateReads {
		return true
	}
	b, ok := p.r.(*bufReadSeeker)
	if !ok {
		return freeAt(p.r, off, n)
	}
	end := b.off + int64(b.br.Buffered())
	switch {
	case off >= b.off && off+n <= end:
		return true
	case off >= b.off && off < end:
		return freeAt(b.rs, end, off+n-end)
	}
	return off >= end && freeAt(b.rs, off, n)
}

// statsFrameRates sets EstimatedFrameRate on the video tracks without DefaultDuration from their NUMBER_OF_FRAMES and DURATION statistics tags, snapped to a standard rate within 2e-4.
func statsFrameRates(c *mkv.Container) {
	tagsByUID := make(map[uint64][]int)
	for k, tag := range c.Tags {
		if tag.TargetID != 0 {
			tagsByUID[tag.TargetID] = append(tagsByUID[tag.TargetID], k)
		}
	}
	for i := range c.Tracks {
		t := &c.Tracks[i]
		if t.Type != mkv.VideoTrack || t.FrameRate != nil || t.UID == 0 {
			continue
		}
		for _, k := range tagsByUID[t.UID] {
			tag := c.Tags[k]
			var frames, seconds float64
			for _, st := range tag.SimpleTags {
				switch strings.ToUpper(st.Name) {
				case "NUMBER_OF_FRAMES":
					if n, err := strconv.ParseUint(strings.TrimSpace(st.Value), 10, 64); err == nil {
						frames = float64(n)
					}
				case "DURATION":
					seconds = tagSeconds(st.Value)
				}
			}
			if frames < 2 || seconds <= 0 {
				continue
			}
			r := frames / seconds
			for _, s := range standardRates {
				if std := float64(s[0]) / float64(s[1]); math.Abs(r-std) <= 2e-4*std {
					r = std
				}
			}
			t.EstimatedFrameRate = &r
		}
	}
}

// tagSeconds parses a statistics DURATION, HH:MM:SS.nnnnnnnnn, into seconds; 0 when malformed.
func tagSeconds(v string) float64 {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 3 {
		return 0
	}
	h, e1 := strconv.ParseUint(parts[0], 10, 32)
	m, e2 := strconv.ParseUint(parts[1], 10, 32)
	s, e3 := strconv.ParseFloat(parts[2], 64)
	if e1 != nil || e2 != nil || e3 != nil || m > 59 || s < 0 || s >= 60 {
		return 0
	}
	return float64(h)*3600 + float64(m)*60 + s
}

// codecFrameRate returns the frame rate the codec header in CodecPrivate states: the SPS VUI timing of an avcC or hvcC, an MPEG-2 frame_rate_code.
func codecFrameRate(t *mkv.Track) (float64, bool) {
	var bc *bitstreamColour
	switch t.Codec {
	case "h264":
		bc = avcColour(t.CodecPrivate)
	case "hevc":
		bc = hevcColour(t.CodecPrivate)
	case "V_MPEG2", "mpeg2":
		return mpeg2FrameRate(t.CodecPrivate)
	}
	if bc == nil || bc.fpsNum == 0 || bc.fpsDen == 0 {
		return 0, false
	}
	fps := float64(bc.fpsNum) / float64(bc.fpsDen)
	if fps < 1 || fps > 1000 { // a timing that names no picture rate (a 90 kHz clock left in num_units_in_tick)
		return 0, false
	}
	if !bc.fpsFixed && !isStandardRate(fps) { // a clock (2000 Hz reads as 1000 frames per second), not a frame rate
		return 0, false
	}
	return fps, true
}

// mpeg2FrameRate reads frame_rate_code from an MPEG-2 sequence header, with the sequence extension's frame_rate_extension when present.
func mpeg2FrameRate(cp []byte) (float64, bool) {
	i := bytes.Index(cp, []byte{0, 0, 1, 0xB3})
	if i < 0 || len(cp) < i+8 {
		return 0, false
	}
	rates := [9][2]float64{{}, {24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {50, 1}, {60000, 1001}, {60, 1}}
	code := cp[i+7] & 0x0f
	if code == 0 || int(code) >= len(rates) {
		return 0, false
	}
	fps := rates[code][0] / rates[code][1]
	if j := bytes.Index(cp[i:], []byte{0, 0, 1, 0xB5}); j >= 0 && len(cp) >= i+j+10 && cp[i+j+4]>>4 == 1 {
		ext := cp[i+j:]
		n := float64((ext[9]>>5)&0x03) + 1 // frame_rate_extension_n
		d := float64(ext[9]&0x1f) + 1      // frame_rate_extension_d
		fps = fps * n / d
	}
	return fps, true
}

// codecFrameRates sets EstimatedFrameRate from the codec header of every video track without DefaultDuration that states one, over any other estimate.
func codecFrameRates(c *mkv.Container) {
	for i := range c.Tracks {
		t := &c.Tracks[i]
		if t.Type != mkv.VideoTrack || t.FrameRate != nil {
			continue
		}
		if fps, ok := codecFrameRate(t); ok {
			t.EstimatedFrameRate = &fps
		}
	}
}

// isStandardRate tells whether fps is one of standardRates within 2e-4.
func isStandardRate(fps float64) bool {
	for _, s := range standardRates {
		if std := float64(s[0]) / float64(s[1]); math.Abs(fps-std) <= 2e-4*std {
			return true
		}
	}
	return false
}
