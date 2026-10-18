package reader

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestEmptyEntriesChargeTheBudget: only strings and binaries were charged to the metadata budget, so a Tracks of
// empty TrackEntry elements, two bytes each, became one 560-byte Track apiece: 4 MB of input held 1.5 GB. Each entry
// a decoder keeps is charged what it holds in memory; the same for chapters, editions, tags and attachments.
func TestEmptyEntriesChargeTheBudget(t *testing.T) {
	cases := map[string]struct {
		entry  []byte
		decode func(p *parser, size int64, c *mkv.Container) error
	}{
		"tracks":      {[]byte{0xAE, 0x80}, (*parser).parseTracks},
		"chapters":    {[]byte{0x45, 0xB9, 0x80}, (*parser).parseChapters},
		"tags":        {[]byte{0x73, 0x73, 0x80}, (*parser).parseTags},
		"attachments": {[]byte{0x61, 0xA7, 0x80}, (*parser).parseAttachments},
	}
	for name, tc := range cases {
		body := bytes.Repeat(tc.entry, 50000)
		p := &parser{r: bytes.NewReader(body), metaBudget: 1 << 20, segEnd: -1}
		if err := tc.decode(p, int64(len(body)), &mkv.Container{}); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Errorf("%s: 50000 empty entries in a 1 MiB budget: %v, want the budget refusing them", name, err)
		}
	}
}
