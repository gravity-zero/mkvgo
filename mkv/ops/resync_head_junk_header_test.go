package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestResyncKeepsMetadataBehindJunkThatReadsAsAnElement: junk ahead of the Tracks that happens to decode as an
// element header, or a Void declaring more than it holds, used to send the tolerant walk to the first Cluster: exit
// 0 and an output with no track. The skipped range must be the junk and nothing else.
func TestResyncKeepsMetadataBehindJunkThatReadsAsAnElement(t *testing.T) {
	for name, junk := range map[string][]byte{
		"declares more than the file holds": []byte("0000\x00\x00"),
		"declares a few bytes into Tracks":  {'0', '0', '0', 0x88, 0x00, 0x00},
		"a Void declaring 6 bytes too many": {0xEC, 0x8A, 0x00, 0x00, 0x00, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			data := livefixture.Build(livefixture.Options{})
			at := int64(bytes.Index(data, []byte{0x16, 0x54, 0xAE, 0x6B}))
			if at < 0 {
				t.Fatal("no Tracks in the fixture")
			}
			spliced := append(append(append([]byte(nil), data[:at]...), junk...), data[at:]...)
			src := filepath.Join(t.TempDir(), "junk.mkv")
			writeAll(t, src, spliced)

			var skipped []mkv.DamagedRange
			dst := filepath.Join(t.TempDir(), "resync.mkv")
			err := Reindex(ctx, src, dst, mkv.Options{
				Resync: true,
				OnSkip: func(r mkv.DamagedRange) { skipped = append(skipped, r) },
			})
			if err != nil {
				t.Fatalf("Reindex with Resync: %v", err)
			}
			if end := at + int64(len(junk)); len(skipped) != 1 || skipped[0].StartOffset != at || skipped[0].EndOffset != end {
				t.Errorf("skipped = %+v, want exactly the junk [%d,%d)", skipped, at, end)
			}
			checkLiveOutput(t, dst)
		})
	}
}
