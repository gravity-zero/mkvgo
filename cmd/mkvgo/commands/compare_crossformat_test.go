package commands_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mp4"
)

// compare accepts an MP4 on either side (via the head-only MP4 probe), so a
// remux round-trip can be verified: mkvgo compare in.mkv out.mp4.
func TestCmdCompare_CrossFormat(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, out); err != nil {
		t.Fatal(err)
	}

	recordExit(t)
	stdout := capture(t, func() { commands.CmdCompare(regfixMKV, out) })

	// The remux changes the muxing/writing app ("mkvgo"), so the compare must
	// run cross-format and report that — not fail to open the MP4.
	if !strings.Contains(stdout, "muxing_app") && !strings.Contains(stdout, "identical metadata") {
		t.Errorf("cross-format compare output unexpected:\n%s", stdout)
	}
	// Track layout survives the remux: no added/removed track lines.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "track[") && (strings.Contains(line, "added") || strings.Contains(line, "removed")) {
			t.Errorf("remux must not add/remove tracks: %s", line)
		}
	}
}
