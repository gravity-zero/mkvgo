package reader

import (
	"bytes"
	"testing"
)

// TestStreamParserChargesBudget covers the streaming reader's cumulative metadata
// budget (parity with the seekable parser): a read past the remaining budget is
// rejected before allocating, so a forged stream cannot exhaust memory.
func TestStreamParserChargesBudget(t *testing.T) {
	over := &streamParser{r: bytes.NewReader(make([]byte, 100)), metaBudget: 10}
	if _, err := over.readBytes(50); err == nil {
		t.Error("readBytes past the budget must error")
	}
	if _, err := over.readString(50); err == nil {
		t.Error("readString past the budget must error")
	}

	ok := &streamParser{r: bytes.NewReader(make([]byte, 100)), metaBudget: maxMetadataBytes}
	if _, err := ok.readBytes(50); err != nil {
		t.Errorf("within budget: %v", err)
	}
}
