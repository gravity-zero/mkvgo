package mp4

import (
	"context"
	"testing"
)

// TestPlanHLSValidatesEncryptOnAnMP4Source: an AES-128 key of 15 bytes was refused on a Matroska source and taken on
// an MP4 one, whose plan routes before the check.
func TestPlanHLSValidatesEncryptOnAnMP4Source(t *testing.T) {
	src := hlsplanMP4Source(t)
	bad := &HLSEncryption{Key: make([]byte, 15), KeyURI: "key.bin"}
	if _, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, Encrypt: bad}); err == nil {
		t.Error("PlanHLS of an MP4 source took a 15-byte AES-128 key")
	}
}
