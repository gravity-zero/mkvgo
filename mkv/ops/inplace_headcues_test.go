package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// headCuesMKV writes a file with no SeekHead whose head is Info, Tracks, Cues, then the given elements (each a
// complete element), then the clusters; it returns the Cues' offset and bytes.
func headCuesMKV(t *testing.T, path string, after ...[]byte) (int, []byte) {
	src := filepath.Join(t.TempDir(), "src.mkv")
	f, err := os.Create(src)
	mustNil(t, err)
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1)}, 2000))
	for tc := int64(0); tc < 2000; tc += 1000 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: make([]byte, 500)}}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	data, err := os.ReadFile(src)
	mustNil(t, err)

	r := bytes.NewReader(data)
	h, hl, err := ebml.ReadElementHeader(r)
	mustNil(t, err)
	segStart := hl + int(h.Size)
	sh, shl, err := ebml.ReadElementHeader(bytes.NewReader(data[segStart:]))
	mustNil(t, err)
	if sh.ID != mkv.IDSegment {
		t.Fatal("no Segment")
	}
	var info, tracks, clusters []byte
	var clusterRel []int64 // each cluster's offset inside clusters
	for pos := segStart + shl; pos < len(data); {
		eh, ehl, err := ebml.ReadElementHeader(bytes.NewReader(data[pos:]))
		mustNil(t, err)
		el := data[pos : pos+ehl+int(eh.Size)]
		switch eh.ID {
		case mkv.IDInfo:
			info = el
		case mkv.IDTracks:
			tracks = el
		case mkv.IDCluster:
			clusterRel = append(clusterRel, int64(len(clusters)))
			clusters = append(clusters, el...)
		}
		pos += len(el)
	}
	var rest []byte
	for _, a := range after {
		rest = append(rest, a...)
	}
	// The Cues' size depends on the positions they hold, which depend on it: settle it in a few passes.
	var cues bytes.Buffer
	for range 4 {
		base := int64(len(info) + len(tracks) + cues.Len() + len(rest))
		var cps []mkv.CuePoint
		for i, rel := range clusterRel {
			cps = append(cps, mkv.CuePoint{TimeMs: int64(i) * 1000, Track: 1, ClusterPos: base + rel})
		}
		cues.Reset()
		mustNil(t, writer.WriteCues(&cues, cps, 1_000_000))
	}
	var out bytes.Buffer
	out.Write(data[:segStart])
	out.Write([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) // Segment, unknown size
	out.Write(info)
	out.Write(tracks)
	cuesAt := out.Len()
	out.Write(cues.Bytes())
	out.Write(rest)
	out.Write(clusters)
	mustNil(t, os.WriteFile(path, out.Bytes(), 0o644))
	return cuesAt, append([]byte(nil), cues.Bytes()...)
}

// TestEditInPlaceKeepsCuesInTheHead: in a head of Info, Tracks, Cues, Tags, Void, the region an edit rewrites ran
// from Info to the end of the Void, over the Cues. The edit must leave the Cues' bytes as they were, or refuse.
func TestEditInPlaceKeepsCuesInTheHead(t *testing.T) {
	var tags, void bytes.Buffer
	mustNil(t, writer.WriteTags(&tags, []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "NOTE", Value: "x"}}}}))
	mustNil(t, writer.WriteVoid(&void, 400))
	path := filepath.Join(t.TempDir(), "headcues.mkv")
	cuesAt, cues := headCuesMKV(t, path, tags.Bytes(), void.Bytes())
	if c, err := reader.Open(context.Background(), path); err != nil || len(c.Cues) != 2 {
		t.Fatalf("fixture: %v, cues %v", err, c)
	}
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "A longer title than before" })
	data, rerr := os.ReadFile(path)
	mustNil(t, rerr)
	if !bytes.Equal(data[cuesAt:cuesAt+len(cues)], cues) {
		t.Fatalf("EditInPlace (err %v) overwrote the Cues in the head", err)
	}
	if err != nil && !errors.Is(err, ErrNoRoomInPlace) {
		t.Fatalf("EditInPlace = %v; want success or ErrNoRoomInPlace", err)
	}
	if err == nil {
		c, err := reader.Open(context.Background(), path)
		mustNil(t, err)
		if c.Info.Title != "A longer title than before" || len(c.Cues) != 2 {
			t.Errorf("title %q, %d cues", c.Info.Title, len(c.Cues))
		}
	}
}
