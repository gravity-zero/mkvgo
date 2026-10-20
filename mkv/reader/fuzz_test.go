package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzRead exercises the metadata parser against arbitrary bytes. Its only
// contract: Read must never panic or hang on malformed input - it may only
// return a Container or an error. Allocations are bounded by ebml.MaxElementSize,
// so the fuzzer cannot OOM the process. The seed corpus covers valid files and
// the corruption shapes the resync handles.
func FuzzRead(f *testing.F) {
	f.Add(buildMinimalMKV().Bytes())
	f.Add(liveCuesMKV()) // unknown-size Tags, Cues and CuePoints: the resync path past them
	f.Add(buildGappedMKV(4096, true))
	f.Add(buildGappedMKV(200<<10, false))
	f.Add(append(realCluster(), 0x00, 0x00, 0x00, 0x00)) // cluster then padding
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3})                // bare EBML id
	f.Add([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF})    // segment, truncated size
	f.Add([]byte{0x1F, 0x43, 0xB6, 0x75})                // bare cluster magic
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			_, _ = Read(context.Background(), bytes.NewReader(data), "fuzz.mkv")
		})
	})
}
