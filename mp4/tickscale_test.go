package mp4

import "testing"

// TestTickConversionsDoNotWrap: a sample table's decode times reach 16M samples of 2^31 ticks, past what ticks*1000
// holds in an int64: the product wrapped and a late sample read at a negative time. Large values scale apart and
// saturate; the usual range converts exactly as before.
func TestTickConversionsDoNotWrap(t *testing.T) {
	if got := ticksToMs(1<<62, 90000); got <= 0 || got != (1<<62)/90000*1000+((1<<62)%90000)*1000/90000 {
		t.Errorf("ticksToMs(2^62, 90000) = %d, want about 5.1e16", got)
	}
	if got := ticksToNs(1<<40, 48000); got <= 0 {
		t.Errorf("ticksToNs(2^40, 48000) = %d, want positive", got)
	}
	if got := ticksToMs(-(1 << 62), 1); got >= 0 {
		t.Errorf("ticksToMs(-2^62, 1) = %d, want it to stay negative", got)
	}
	if ticksToMs(90000, 90000) != 1000 || ticksToNs(1024, 48000) != 21333333 {
		t.Error("the usual range must convert as before")
	}
}
