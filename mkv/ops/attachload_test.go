package ops

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestLoadAttachmentDataGrowsAsBytesArrive: an attachment whose FileData declares far more bytes than the file holds
// fails without the declared size ever being allocated.
func TestLoadAttachmentDataGrowsAsBytesArrive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.mkv")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &mkv.Attachment{Name: "font.ttf", DataPath: path, DataOffset: 100, Size: 300 << 20}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := LoadAttachmentData(context.Background(), a)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("a payload past the end of the file was loaded")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("%d MiB allocated for a 4 KiB file", grew>>20)
	}
}
