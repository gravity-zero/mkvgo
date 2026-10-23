package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzH26xSubsamples: a H.264/HEVC sample of any bytes, against any CodecPrivate, is split into subsamples that
// cover it exactly, each protected run a whole number of AES blocks under 'cenc', or refused; never a panic.
func FuzzH26xSubsamples(f *testing.F) {
	f.Add(cencVideoSample(), fakeAVCC, false, true)
	f.Add(cencVideoSample(), []byte(nil), false, false)
	f.Add(testAVCFrame(600), []byte(nil), false, true)
	f.Add([]byte{0, 0, 0, 3, 0x26, 0x01, 0xAF}, []byte(nil), true, false)
	f.Fuzz(func(t *testing.T, sample, private []byte, hevc, cenc bool) {
		costguard.Fuzz(t, len(sample)+len(private), func() {
			codec, scheme := "h264", "cbcs"
			if hevc {
				codec = "hevc"
			}
			if cenc {
				scheme = "cenc"
			}
			sp, err := newH26xSplitter(codec, private, scheme)
			if err != nil {
				return
			}
			subs, err := sp.split(sample)
			if err != nil {
				return
			}
			total := 0
			for _, s := range subs {
				if cenc && s.protected%16 != 0 {
					t.Fatalf("protected run of %d bytes is not whole AES blocks", s.protected)
				}
				total += int(s.clear) + int(s.protected)
			}
			if total != len(sample) {
				t.Fatalf("subsamples cover %d bytes of a %d-byte sample", total, len(sample))
			}
		})
	})
}
