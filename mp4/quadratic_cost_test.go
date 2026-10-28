package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// TestFragmentReadGrowsLinearly: each traf summed the samples of every track for its room, and looked its track up
// by a scan of them: a file of n tracks and n fragments for the last one cost n² of each.
func TestFragmentReadGrowsLinearly(t *testing.T) {
	costguard.Linear(t, 4000, func(n int) time.Duration {
		mv := &movie{tracks: make([]inTrack, n)}
		for i := range mv.tracks {
			mv.tracks[i] = inTrack{trackID: uint32(i + 1), timescale: 1000}
		}
		moof := buildOneSampleMoof(uint32(n))
		data := bytes.Repeat(moof, n)
		start := time.Now()
		if err := readFragmentSamples(bytes.NewReader(data), int64(len(data)), mv, nil); err != nil {
			t.Fatal(err)
		}
		d := time.Since(start)
		if got := len(mv.tracks[n-1].samples); got != n {
			t.Fatalf("%d samples, want %d", got, n)
		}
		return d
	})
}

// TestUdtaTagsGrowLinearly: each QuickTime text atom scanned every tag read before it: n ilst tags and n text
// atoms cost n².
func TestUdtaTagsGrowLinearly(t *testing.T) {
	data := func(text string) []byte {
		p := make([]byte, 8, 8+len(text))
		binary.BigEndian.PutUint32(p, 1) // UTF-8 text
		return box("data", append(p, text...))
	}
	costguard.Linear(t, 16000, func(n int) time.Duration {
		var ilst []byte
		for range n {
			ilst = append(ilst, box("\xa9ART", data("someone"))...)
		}
		meta := append([]byte{0, 0, 0, 0}, box("ilst", ilst)...)
		boxes := []memBox{{typ: "meta", payload: meta}}
		text := []byte{0, 5, 0x55, 0xC4, 'a', 'l', 'b', 'u', 'm'}
		for range n {
			boxes = append(boxes, memBox{typ: "\xa9alb", payload: text})
		}
		start := time.Now()
		tags, _, _, _ := parseMP4Tags(boxes)
		d := time.Since(start)
		if len(tags) != n+1 {
			t.Fatalf("%d tags, want %d", len(tags), n+1)
		}
		return d
	})
}

// TestOneSampleDescriptionGrowsLinearly: each chunk run compared its sample description's bytes with the first's: n
// runs of a description of n bytes cost n².
func TestOneSampleDescriptionGrowsLinearly(t *testing.T) {
	costguard.Linear(t, 4000, func(n int) time.Duration {
		entry := box("mp4a", make([]byte, n))
		stsd := fullBox("stsd", 0, 0, func(w *bw) { w.u32(2); w.bytes(entry); w.bytes(entry) })
		stsc := fullBox("stsc", 0, 0, func(w *bw) {
			w.u32(uint32(n))
			for i := range n {
				w.u32(uint32(i + 1))
				w.u32(1)
				w.u32(2)
			}
		})
		boxes, err := iterBoxes(append(stsd, stsc...))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		err = oneSampleDescription(boxes)
		d := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}
