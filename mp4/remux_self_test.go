package mp4

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestRemuxRefusesItsSourceAsOutput pins that a remux never writes over the file it reads: creating the output
// truncated the source before its samples were read, and the failed write's cleanup then removed it.
func TestRemuxRefusesItsSourceAsOutput(t *testing.T) {
	ctx := context.Background()
	mkvPath := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))
	mp4Path := editShiftFixture(t, false)
	for name, c := range map[string]struct {
		path string
		run  func(string) error
	}{
		"RemuxToMP4":   {mkvPath, func(p string) error { return RemuxToMP4(ctx, p, p) }},
		"RemuxFromMP4": {mp4Path, func(p string) error { return RemuxFromMP4(ctx, p, p) }},
	} {
		before, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.run(c.path); err == nil {
			t.Errorf("%s with its source as output succeeded", name)
		}
		if after, err := os.ReadFile(c.path); err != nil || !bytes.Equal(before, after) {
			t.Errorf("%s changed or removed its source (%v)", name, err)
		}
	}
}
