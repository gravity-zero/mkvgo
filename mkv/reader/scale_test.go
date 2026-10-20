package reader

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/costguard"
	"github.com/gravity-zero/mkvgo/mkv"
)

// scaledSegment is a file whose Segment holds Info and the n entries one metadata element of kind lists.
func scaledSegment(kind string, n int) []byte {
	var body bytes.Buffer
	for i := 0; i < n; i++ {
		switch kind {
		case "tracks":
			body.Write(wrapEl(mkv.IDTrackEntry, bytes.Join([][]byte{uintElem(mkv.IDTrackNumber, uint64(i+1), 8),
				uintElem(mkv.IDTrackUID, uint64(i+1), 8), uintElem(mkv.IDTrackType, 0x11, 1), strElem(mkv.IDCodecID, "S_TEXT/UTF8")}, nil)))
		case "chapters":
			body.Write(wrapEl(mkv.IDChapterAtom, bytes.Join([][]byte{uintElem(mkv.IDChapterUID, uint64(i+1), 8),
				uintElem(mkv.IDChapterTimeStart, uint64(i)*1_000_000_000, 8),
				wrapEl(mkv.IDChapterDisplay, strElem(mkv.IDChapString, fmt.Sprintf("Chapter %d", i)))}, nil)))
		case "tags":
			body.Write(wrapEl(mkv.IDTag, bytes.Join([][]byte{wrapEl(mkv.IDTargets, nil),
				wrapEl(mkv.IDSimpleTag, bytes.Join([][]byte{strElem(mkv.IDTagName, fmt.Sprintf("NAME%d", i)), strElem(mkv.IDTagString, "value")}, nil))}, nil)))
		case "cues":
			body.Write(wrapEl(mkv.IDCuePoint, bytes.Join([][]byte{uintElem(mkv.IDCueTime, uint64(i), 8),
				wrapEl(mkv.IDCueTrackPositions, bytes.Join([][]byte{uintElem(mkv.IDCueTrack, 1, 1), uintElem(mkv.IDCueClusterPos, uint64(i), 8)}, nil))}, nil)))
		}
	}
	var el []byte
	switch kind {
	case "tracks":
		el = wrapEl(mkv.IDTracks, body.Bytes())
	case "chapters":
		el = wrapEl(mkv.IDChapters, wrapEl(mkv.IDEditionEntry, body.Bytes()))
	case "tags":
		el = wrapEl(mkv.IDTags, body.Bytes())
	case "cues":
		el = wrapEl(mkv.IDCues, body.Bytes())
	}
	info := wrapEl(mkv.IDInfo, uintEl(mkv.IDTimecodeScale, 1000000))
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&b, mkv.IDSegment, int64(len(info)+len(el)))
	b.Write(info)
	b.Write(el)
	return b.Bytes()
}

// Reading a file costs in proportion to its metadata, whichever element holds it and whichever reader reads it.
func TestMetadataReadCostGrowsLinearly(t *testing.T) {
	readers := map[string]func(data []byte) (*mkv.Container, error){
		"Read": func(data []byte) (*mkv.Container, error) {
			return Read(context.Background(), bytes.NewReader(data), "scale.mkv")
		},
		"ReadMeta": func(data []byte) (*mkv.Container, error) {
			return ReadMeta(context.Background(), bytes.NewReader(data), "scale.mkv", WithChapters(), WithTags(), WithCues())
		},
		"ReadStream": func(data []byte) (*mkv.Container, error) {
			c, _, err := ReadStream(context.Background(), bytes.NewReader(data))
			return c, err
		},
	}
	for _, kind := range []string{"tracks", "chapters", "tags", "cues"} {
		for name, read := range readers {
			if kind == "cues" && name == "ReadStream" {
				continue // a stream stops at the first Cluster and leaves the Cues to the seekable reader
			}
			t.Run(kind+"/"+name, func(t *testing.T) {
				costguard.Linear(t, 20_000, func(n int) time.Duration {
					data := scaledSegment(kind, n)
					start := time.Now()
					if _, err := read(data); err != nil {
						t.Fatal(err)
					}
					return costguard.Since(start)
				})
			})
		}
	}
}
