package dovi

import "errors"

var (
	errShort    = errors.New("the RPU ends inside a field")
	errExpGolom = errors.New("an Exp-Golomb value runs past 32 bits")
)

// bitReader reads an RPU most significant bit first; its first error sticks.
type bitReader struct {
	b   []byte
	pos int
	err error
}

func (r *bitReader) left() int { return len(r.b)*8 - r.pos }

func (r *bitReader) bits(n int) uint64 {
	if r.err != nil {
		return 0
	}
	if n > r.left() {
		r.err = errShort
		return 0
	}
	var v uint64
	for ; n > 0; n-- {
		v = v<<1 | uint64(r.b[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.bits(1) == 1 }

func (r *bitReader) ue() uint64 {
	zeros := 0
	for r.err == nil && r.bits(1) == 0 {
		if zeros++; zeros > 32 {
			r.err = errExpGolom
		}
	}
	return 1<<zeros - 1 + r.bits(zeros)
}

// align skips to the next byte boundary and reports whether the bits skipped were all zero.
func (r *bitReader) align() bool {
	zero := true
	for r.err == nil && r.pos&7 != 0 {
		zero = r.bits(1) == 0 && zero
	}
	return zero
}

// bitWriter appends bits most significant bit first.
type bitWriter struct {
	b    []byte
	free int // bits still unset in the last byte
}

func (w *bitWriter) bits(v uint64, n int) {
	for n--; n >= 0; n-- {
		if w.free == 0 {
			w.b, w.free = append(w.b, 0), 8
		}
		w.free--
		w.b[len(w.b)-1] |= byte(v>>n&1) << w.free
	}
}

func (w *bitWriter) ue(v uint64) {
	n := 0
	for (v+1)>>n > 1 {
		n++
	}
	w.bits(0, n)
	w.bits(v+1, n+1)
}

// copyBits appends the bits [from, to) of src.
func (w *bitWriter) copyBits(src []byte, from, to int) {
	for ; from < to && (from&7 != 0 || w.free != 0 || to-from < 8); from++ {
		w.bits(uint64(src[from>>3]>>(7-from&7)&1), 1)
	}
	if from < to {
		whole := (to - from) >> 3
		w.b = append(w.b, src[from>>3:from>>3+whole]...)
		w.copyBits(src, from+whole<<3, to)
	}
}

func (w *bitWriter) align() { w.free = 0 }
