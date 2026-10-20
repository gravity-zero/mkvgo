package mkvhttp

import (
	"net/http"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzByteRange: any Range header against any size yields a span inside the resource, or a refusal.
func FuzzByteRange(f *testing.F) {
	for _, h := range []string{"", "bytes=0-", "bytes=10-19", "bytes=-5", "bytes=5-2", "bytes=0-1,4-5", "items=0-1"} {
		f.Add(h, int64(100))
	}
	f.Fuzz(func(t *testing.T, header string, size int64) {
		costguard.Fuzz(t, len(header), func() {
			if size < 0 {
				size = -(size + 1)
			}
			start, n, status, ok := byteRange(header, size)
			if !ok {
				return
			}
			if start < 0 || n < 0 || start > size-n {
				t.Fatalf("%q on %d bytes: span %d+%d outside the resource", header, size, start, n)
			}
			if status == http.StatusPartialContent && n == 0 {
				t.Fatalf("%q on %d bytes: 206 with an empty span", header, size)
			}
		})
	})
}
