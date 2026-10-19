package writer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestWriteAttachmentsRefusesAPayloadLeftOnDisk: an attachment read WithoutAttachmentData keeps its payload in the
// file; writing it with nothing to read it there is refused, where it panicked on a nil opener.
func TestWriteAttachmentsRefusesAPayloadLeftOnDisk(t *testing.T) {
	att := mkv.Attachment{Name: "font.ttf", MIMEType: "font/ttf", DataPath: "/films/a.mkv", Size: 10}
	var w bytes.Buffer
	if err := WriteAttachments(&w, []mkv.Attachment{att}); err == nil || !strings.Contains(err.Error(), "LoadAttachmentData") {
		t.Errorf("writing an attachment left on disk: %v", err)
	}
}
