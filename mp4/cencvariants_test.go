package mp4

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestCENCVariantsBounded: 'cenc' variants share the key and part its IV space by variant number (i<<48), below the
// track domains (2^56 and up): a 257th variant's video took the IVs of the first variant's track 255. Past 256
// variants under one 'cenc' key, the ladder is refused before anything is read.
func TestCENCVariantsBounded(t *testing.T) {
	srcs := make([]string, 257)
	for i := range srcs {
		srcs[i] = fmt.Sprintf("/nonexistent/v%d.mkv", i)
	}
	cenc := &CENCOptions{Scheme: "cenc", KeyID: make([]byte, 16), Key: make([]byte, 16), IV: make([]byte, 8), InlineKey: true}
	_, planErr := PlanABR(context.Background(), srcs, Options{CENC: cenc})
	remuxErr := RemuxToABR(context.Background(), srcs, t.TempDir(), Options{CENC: cenc})
	for name, err := range map[string]error{"PlanABR": planErr, "RemuxToABR": remuxErr} {
		if err == nil || !strings.Contains(err.Error(), "at most 256 variants") {
			t.Errorf("%s with 257 'cenc' variants: %v, want the ladder refused", name, err)
		}
	}
}
