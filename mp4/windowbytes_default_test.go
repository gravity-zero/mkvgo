package mp4

import "testing"

// TestWindowBytesDefault: the process keeps 64 MiB of a network source's windows by default, what served them as 256 MiB
// did for a peak a quarter as high; zero restores it.
func TestWindowBytesDefault(t *testing.T) {
	was := windowBytesBudget.Load()
	t.Cleanup(func() { windowBytesBudget.Store(was) })
	SetWindowBytes(-1)
	SetWindowBytes(0)
	if got := windowBytesBudget.Load(); got != 64<<20 {
		t.Errorf("default window budget %d MiB, want 64", got>>20)
	}
}
