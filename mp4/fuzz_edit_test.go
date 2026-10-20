package mp4

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// FuzzMP4EditTables runs a forged file through the in-memory planning of edit, rewrite, split and join: refuse or succeed, never panic or stall.
func FuzzMP4EditTables(f *testing.F) {
	for _, name := range []string{"bframes_aac.mp4", "hevc_opengop.mp4", "offset_ts48k.mp4"} {
		if data, err := os.ReadFile("../internal/testdata/" + name); err == nil {
			f.Add(data)
		}
	}
	f.Add(sampleMP4(f))
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			start := time.Now()
			r := bytes.NewReader(data)
			size := int64(len(data))
			if payload, err := readMoovLimit(r, size, DefaultMaxMoovBytes); err == nil {
				_, _, _ = collectTraks(payload, nil, size)
				if mv, err := parseMoov(payload, size, sampleNone); err == nil {
					_ = traksInMdat(payload, nil, sourceMdats(r, size), size, size)
					if before, _, err := moovContainer(r, payload, size); err == nil {
						after, _, _ := moovContainer(r, payload, size)
						after.Info.Title = "x"
						after.Chapters = []mkv.Chapter{{Title: "A", StartMs: 0}}
						if checkEditable(before, after) == nil {
							_, _, _ = patchMoovMeta(payload, mv, before, after, size)
						}
					}
				}
			}
			budget := int64(maxSamples)
			m, err := loadCutModel(r, "", size, 0, &budget, Options{})
			if err == nil {
				cur := make([]int, len(m.tracks))
				for _, rg := range []mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 1000}} {
					p, err := m.planPart(rg, cur)
					if err != nil {
						continue
					}
					for i := range m.tracks {
						c := &m.tracks[i]
						tt := c.tt.slice(p.a[i], p.b[i])
						edts, dur := partEdit(c, tt, min(p.a[i], len(c.tt.samples)-1), m.movieTS, m.outTS, p.tsNs, p.teNs)
						_, _ = rebuildTrak(c.trak, tt, edts, dur)
					}
				}
				var vs vsource
				for ti := range m.tracks {
					_, _, _ = joinTables([]*cutModel{m, m}, []int64{0, m.movieNs}, ti, &vs)
				}
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("edit planning took %v on a %d-byte input: %x", d, len(data), data)
			}
		})
	})
}
