package reader

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// FuzzReadMeta exercises the metadata-only path on arbitrary bytes: it must never
// panic, hang, or allocate unboundedly (the runtime catches panics and the fuzz
// timeout catches non-termination). Seeded with the synthetic fixtures and the
// real muxer-written file so the mutator starts from valid EBML.
func FuzzReadMeta(f *testing.F) {
	f.Add(buildPlainMKV())
	f.Add(segmentMKV(infoElem(), tracksElem(), cuesElem(8), clusterElem()))
	if b, err := os.ReadFile("testdata/probe/hdr_multi.mkv"); err == nil {
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Result is irrelevant; the contract is "no panic / no hang on hostile input".
		_, _ = ReadMeta(context.Background(), bytes.NewReader(data), "fuzz.mkv")
	})
}
