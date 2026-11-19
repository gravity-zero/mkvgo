package subtitle

import "testing"

// TestVobSubSequenceEndsWhereTheNextStarts: a control sequence with no end command was read on into the header of
// the next one, whose date bytes then ran as commands: a date starting with 0x00 forced the picture on, at the first
// sequence's date. A sequence ends where the next one starts.
func TestVobSubSequenceEndsWhereTheNextStarts(t *testing.T) {
	first := []byte{spuColours, 0x42, 0x10, spuContrast, 0xff, 0xf0}
	first = append(first, spuArea4x2(100, 400)...)
	first = append(first, spuOffsets, 0, 4, 0, 6) // no start, and no end command
	ctrl := 4 + len(spuTop4x2) + len(spuBottom4x2)
	spu := []byte{0, 0, byte(ctrl >> 8), byte(ctrl)}
	spu = append(append(spu, spuTop4x2...), spuBottom4x2...)
	second := len(spu) + 4 + len(first)
	spu = append(spu, 0, 0, byte(second>>8), byte(second))
	spu = append(spu, first...)
	spu = append(spu, 0x00, 0x64, byte(second>>8), byte(second), spuStart, spuEnd) // shows at date 0x64, not forced
	spu[0], spu[1] = byte(len(spu)>>8), byte(len(spu))

	set := decodeOneVobSub(t, spu)
	if len(set.Objects) != 1 || set.Objects[0].Forced || set.Delay <= 0 {
		t.Errorf("objects %d, forced %v, delay %v; want one picture, not forced, shown at the second sequence's date",
			len(set.Objects), len(set.Objects) == 1 && set.Objects[0].Forced, set.Delay)
	}
}
