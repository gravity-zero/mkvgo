package mp4

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// trakParityFixtures are the progressive MP4/MOV files both table readers must read alike.
var trakParityFixtures = []string{"bframes_aac.mp4", "bframes_aac.mov", "fps30.mp4", "hevc_opengop.mp4", "interleaved.mp4", "negative_cts.mp4", "offset_ts48k.mp4", "quicktime.mov", "slices.mp4"}

// parityTraks lists a fixture's traks and its movie timescale.
func parityTraks(name string) ([][]byte, uint32, error) {
	f, err := os.Open(filepath.Join("..", "internal", "testdata", name))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	payload, err := readMoov(f, fi.Size())
	if err != nil {
		return nil, 0, err
	}
	boxes, err := iterBoxes(payload)
	if err != nil {
		return nil, 0, err
	}
	var traks [][]byte
	var movieTS uint32
	for _, b := range boxes {
		switch b.typ {
		case "mvhd":
			movieTS, _ = parseMovieHeader(b.payload)
		case "trak":
			traks = append(traks, b.payload)
		}
	}
	return traks, movieTS, nil
}

// tableDiff compares what the remux reader (parseTrak) and the edit reader (readTrakTable) make of one trak:
// two tables must list the same samples, and with strict one reader refusing what the other reads is a difference too.
func tableDiff(trak []byte, fileSize int64, movieTS uint32, strict bool) (string, bool) {
	tr, dropped, perr := parseTrak(trak, fileSize, movieTS, sampleFull)
	budget := int64(maxSamples)
	tt, terr := readTrakTable(trak, 0, fileSize, &budget, nil)
	parsed := perr == nil && dropped == nil && tr.unsupported == ""
	switch {
	case dropped != nil || tr.codec == "pcm":
		return "", false // a dropped track is never edited; PCM is gathered into packets on purpose
	case !parsed && terr != nil:
		return "", true
	case !strict && (!parsed || terr != nil):
		return "", false
	case !parsed:
		return fmt.Sprintf("parseTrak refuses (%v %s), readTrakTable reads %d samples", perr, tr.unsupported, len(tt.samples)), true
	case terr != nil:
		return fmt.Sprintf("readTrakTable refuses (%v), parseTrak reads %d samples", terr, len(tr.samples)), true
	case len(tr.samples) != len(tt.samples):
		return fmt.Sprintf("%d samples against %d", len(tr.samples), len(tt.samples)), true
	}
	for i, s := range tr.samples {
		w := tt.samples[i]
		if s.offset != w.src || s.size != w.size || s.sync != w.sync || s.ctsTicks != w.dts+int64(w.cto) {
			return fmt.Sprintf("sample %d: parseTrak %+v, readTrakTable %+v", i, s, w), true
		}
	}
	return "", true
}

// TestTrakReadersAgree: every trak of the corpus is read by both readers, sample for sample.
func TestTrakReadersAgree(t *testing.T) {
	for _, name := range trakParityFixtures {
		traks, movieTS, err := parityTraks(name)
		if err != nil {
			t.Fatal(err)
		}
		compared := 0
		for i, trak := range traks {
			diff, ok := tableDiff(trak, 1<<40, movieTS, true)
			if diff != "" {
				t.Errorf("%s trak %d: %s", name, i+1, diff)
			}
			if ok {
				compared++
			}
		}
		if compared == 0 {
			t.Errorf("%s: no trak compared", name)
		}
	}
}

// FuzzTrakReadersAgree: whatever bytes a trak holds, two readers that both accept it read the same samples.
func FuzzTrakReadersAgree(f *testing.F) {
	for _, name := range trakParityFixtures[:3] {
		traks, movieTS, err := parityTraks(name)
		if err != nil {
			f.Fatal(err)
		}
		for _, trak := range traks {
			f.Add(trak, movieTS)
		}
	}
	f.Fuzz(func(t *testing.T, trak []byte, movieTS uint32) {
		costguard.Fuzz(t, len(trak), func() {
			if diff, _ := tableDiff(trak, 1<<30, movieTS, false); diff != "" {
				t.Fatal(diff)
			}
		})
	})
}
