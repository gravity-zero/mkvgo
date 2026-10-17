package reader

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// The pre-ROB-01 tail scan, kept to check the linear one decides the same.
func old_tailRun(buf []byte, bufStart, end int64) (at int, cues, ok bool) {
	for i, b := range buf {
		if b != 0x1C && b != 0x12 && b != 0x10 && b != 0x19 && b != 0xEC && b != 0x11 && b != 0x15 && b != 0x16 { // first byte of a tail element ID
			continue
		}
		if c, ok := old_metaRunTiles(buf[i:], bufStart+int64(i), end); ok {
			return i, c, true
		}
	}
	return len(buf), false, false
}

func old_metaRunTiles(buf []byte, abs, end int64) (cues, ok bool) {
	for off := 0; abs < end; {
		eh, hlen, err := ebml.ReadElementHeader(bytes.NewReader(buf[off:]))
		if err != nil || eh.Size < 0 || !isTailMeta(eh.ID) {
			return false, false
		}
		step := int64(hlen) + eh.Size
		if abs+step > end {
			return false, false
		}
		cues = cues || eh.ID == mkv.IDCues
		abs += step
		off += int(step)
	}
	return cues, abs == end
}

func old_metaEndsAt(buf []byte, x int) bool {
	for i := 0; i < x; i++ {
		if !innerFirstByte[buf[i]] {
			continue
		}
		if old_innerRunTiles(buf[i:x]) {
			return true
		}
	}
	return false
}

func old_innerRunTiles(b []byte) bool {
	for off := 0; off < len(b); {
		eh, hlen, err := ebml.ReadElementHeader(bytes.NewReader(b[off:]))
		if err != nil || eh.Size < 0 || int64(off)+int64(hlen)+eh.Size > int64(len(b)) {
			return false
		}
		body := b[off+hlen : off+hlen+int(eh.Size)]
		if !old_innerElement(eh.ID, body) {
			return false
		}
		off += hlen + int(eh.Size)
	}
	return len(b) > 0
}

func old_innerElement(id uint32, body []byte) bool {
	switch id {
	case mkv.IDTag, mkv.IDSimpleTag, mkv.IDTargets, mkv.IDTagName, mkv.IDTagString, mkv.IDTagLanguage,
		mkv.IDTagBinary, mkv.IDTargetTypeValue, mkv.IDTagTrackUID, mkv.IDEditionEntry, mkv.IDEditionUID,
		mkv.IDEditionFlagHidden, mkv.IDEditionFlagDefault, mkv.IDEditionFlagOrdered, mkv.IDChapterUID,
		mkv.IDChapterSegmentUID, mkv.IDAttachedFile, mkv.IDFileName, mkv.IDFileMimeType, mkv.IDFileData,
		mkv.IDFileUID, mkv.IDFileDescription:
		return true
	case mkv.IDCuePoint:
		return old_childrenAre(body, mkv.IDCueTime, mkv.IDCueTime, mkv.IDCueTrackPositions)
	case mkv.IDChapterAtom:
		return old_childrenAre(body, mkv.IDChapterUID, mkv.IDChapterUID, mkv.IDChapterTimeStart, mkv.IDChapterTimeEnd,
			mkv.IDChapterDisplay, mkv.IDChapterFlagHidden, mkv.IDChapterSegmentUID, mkv.IDChapterAtom)
	}
	return false
}

func old_childrenAre(body []byte, must uint32, ids ...uint32) bool {
	seen := false
	for off := 0; off < len(body); {
		eh, hlen, err := ebml.ReadElementHeader(bytes.NewReader(body[off:]))
		if err != nil || eh.Size < 0 || int64(off)+int64(hlen)+eh.Size > int64(len(body)) || !slices.Contains(ids, eh.ID) {
			return false
		}
		seen = seen || eh.ID == must
		off += hlen + int(eh.Size)
	}
	return seen
}

func old_tailAnchor(buf []byte, bufStart, end int64) (int, bool) {
	for i := 0; ; {
		j := bytes.Index(buf[i:], cuesMagic)
		if j < 0 {
			return 0, false
		}
		at := i + j
		if old_tilesToEnd(buf[at:], bufStart+int64(at), end) {
			return at, true
		}
		i = at + 1
	}
}

func old_tilesToEnd(buf []byte, abs, end int64) bool {
	for off := 0; abs < end; {
		eh, hlen, err := ebml.ReadElementHeader(bytes.NewReader(buf[off:]))
		if err != nil || eh.Size < 0 || !isTailElement(eh.ID) {
			return false
		}
		step := int64(hlen) + eh.Size
		if abs+step > end {
			return false
		}
		abs += step
		off += int(step)
	}
	return abs == end
}

func checkTailEquivalence(t *testing.T, name string, buf []byte, base int64) {
	t.Helper()
	end := base + int64(len(buf))
	at, cues, ok := tailRun(buf, base, end)
	oat, ocues, ook := old_tailRun(buf, base, end)
	if at != oat || cues != ocues || ok != ook {
		t.Fatalf("%s: tailRun = (%d,%v,%v), was (%d,%v,%v)", name, at, cues, ok, oat, ocues, ook)
	}
	for _, x := range []int{at, len(buf), len(buf) / 2, len(buf) - 1} {
		if x < 0 {
			continue
		}
		if got, want := metaEndsAt(buf, x), old_metaEndsAt(buf, x); got != want {
			t.Fatalf("%s: metaEndsAt(%d) = %v, was %v", name, x, got, want)
		}
	}
	a, aok := tailAnchor(buf, base, end)
	oa, oaok := old_tailAnchor(buf, base, end)
	if a != oa || aok != oaok {
		t.Fatalf("%s: tailAnchor = (%d,%v), was (%d,%v)", name, a, aok, oa, oaok)
	}
}

func TestROB01_TailScanEquivalence(t *testing.T) {
	var files []string
	for _, pat := range []string{"../../internal/testdata/*.mkv", "../../internal/testdata/*.webm", "testdata/*.mkv", "testdata/*.webm"} {
		m, _ := filepath.Glob(pat)
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []int{256, 4 << 10, 64 << 10} {
			if w > len(data) {
				w = len(data)
			}
			tail := data[len(data)-w:]
			checkTailEquivalence(t, filepath.Base(f), tail, int64(len(data)-w))
		}
	}
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte{0x1C, 0x53, 0xBB, 0x6B, 0xEC, 0x80, 0x81, 0x82, 0x84, 0x12, 0x54, 0xC3, 0x67, 0x73, 0x73, 0xB3, 0xB7, 0x00, 0x40, 0xFF, 0x10, 0x43, 0xA7}
	for n := 0; n < 3000; n++ {
		buf := make([]byte, 1+rng.Intn(300))
		for i := range buf {
			if rng.Intn(4) == 0 {
				buf[i] = byte(rng.Intn(256))
			} else {
				buf[i] = alphabet[rng.Intn(len(alphabet))]
			}
		}
		checkTailEquivalence(t, "random", buf, 1000)
	}
	hits, meta := 0, 0
	for n := 0; n < 3000; n++ {
		buf := structuredTail(rng)
		checkTailEquivalence(t, "structured", buf, 5000)
		if _, _, ok := tailRun(buf, 5000, 5000+int64(len(buf))); ok {
			hits++
		}
		if metaEndsAt(buf, len(buf)) {
			meta++
		}
	}
	if hits < 500 || meta < 100 {
		t.Fatalf("only %d tiling tails and %d metadata endings among the structured cases", hits, meta)
	}
}

// el is an EBML element with a one-byte size.
func el(id []byte, body ...[]byte) []byte {
	b := slices.Concat(body...)
	return slices.Concat(id, []byte{0x80 | byte(len(b))}, b)
}

// structuredTail is junk then a run of tail-shaped elements, one byte of it sometimes flipped.
func structuredTail(rng *rand.Rand) []byte {
	cuePoint := func() []byte {
		return el([]byte{0xBB}, el([]byte{0xB3}, []byte{byte(rng.Intn(256))}), el([]byte{0xB7}, el([]byte{0xF7}, []byte{1})))
	}
	tag := func() []byte {
		return el([]byte{0x73, 0x73}, el([]byte{0x67, 0xC8}, el([]byte{0x45, 0xA3}, []byte("T"))))
	}
	var parts [][]byte
	junk := make([]byte, rng.Intn(20))
	rng.Read(junk)
	parts = append(parts, junk)
	for k := rng.Intn(6); k >= 0; k-- {
		switch rng.Intn(4) {
		case 0:
			parts = append(parts, el([]byte{0xEC}, make([]byte, rng.Intn(3))))
		case 1:
			parts = append(parts, el([]byte{0x1C, 0x53, 0xBB, 0x6B}, cuePoint(), cuePoint()))
		case 2:
			parts = append(parts, el([]byte{0x12, 0x54, 0xC3, 0x67}, tag()))
		case 3:
			parts = append(parts, cuePoint(), tag())
		}
	}
	buf := slices.Concat(parts...)
	if len(buf) > 0 && rng.Intn(3) == 0 {
		buf[rng.Intn(len(buf))] ^= byte(1 << rng.Intn(8))
	}
	if len(buf) == 0 {
		buf = []byte{0xEC, 0x80}
	}
	return buf
}
