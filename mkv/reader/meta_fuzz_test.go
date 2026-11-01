package reader

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzReadMeta exercises the metadata-only path on arbitrary bytes: it must never
// panic, hang, or allocate unboundedly (the runtime catches panics and the fuzz
// timeout catches non-termination). Seeded with the synthetic fixtures and the
// real muxer-written file so the mutator starts from valid EBML.
func FuzzReadMeta(f *testing.F) {
	f.Add(buildPlainMKV())
	f.Add(liveCuesMKV()) // unknown-size Tags, Cues and CuePoints: the resync path past them
	f.Add(segmentMKV(infoElem(), tracksElem(), cuesElem(8), clusterElem()))
	if b, err := os.ReadFile("testdata/probe/hdr_multi.mkv"); err == nil {
		f.Add(b)
	}
	// The in-band read walks the first samples of each video track.
	avcc := scanAvcC([][]byte{scanSPS{mbaff: true, picStruct: true}.nal()}, [][]byte{scanPPS(0, 0, true)})
	f.Add(avcScanMKV(avcc, nil, au(picTimingSEI(false, 3, -1), scanSlice{lsb: 2, bottomPOC: true, delta: 1}.nal())))
	if b, err := os.ReadFile("../../internal/testdata/mpeg2_tff.mkv"); err == nil {
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			// Result is irrelevant; the contract is "no panic / no hang on hostile input".
			_, _ = ReadMeta(context.Background(), bytes.NewReader(data), "fuzz.mkv")
			_, _ = ReadMeta(context.Background(), bytes.NewReader(data), "fuzz.mkv", WithInBandColourFallback())
		})
	})
}
