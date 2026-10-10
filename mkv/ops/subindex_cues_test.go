package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildMKVCuedBlocks writes the clusters and cues every video keyframe and every subtitle block with its CueRelativePosition, as muxers do.
func buildMKVCuedBlocks(t *testing.T, dir, name string, tracks []mkv.Track, sets [][]mkv.Block, durationMs int64, tags []mkv.Tag) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}, Tags: tags}
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		t.Fatal(err)
	}
	subs := map[uint64]bool{}
	for _, tr := range tracks {
		if tr.Type == mkv.SubtitleTrack {
			subs[tr.ID] = true
		}
	}
	for _, blocks := range sets {
		clusterPos := mw.RelPos()
		offsets, err := writer.WriteClusterOffsets(f, blocks[0].Timecode, 1000000, blocks)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range blocks {
			if b.Keyframe || subs[b.TrackNumber] {
				mw.Cues = append(mw.Cues, mkv.CuePoint{TimeMs: b.Timecode, Track: b.TrackNumber, ClusterPos: clusterPos, RelativePos: offsets[i]})
			}
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A subtitle index derived from the Cues, once resolved, names the very blocks
// a walk finds; it is unverified without a trusted frame count and verified
// with one; it cannot be stored before it is resolved.
func TestSubtitleIndexFromCuesMatchesWalk(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var video, subs []mkv.Block
	for i := 0; i < 100; i++ {
		video = append(video, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: i%25 == 0, Data: append([]byte{0, 0, 1, 0x2D, 0x65}, make([]byte, 300)...)})
	}
	for i := 0; i < 6; i++ {
		subs = append(subs, mkv.Block{TrackNumber: 2, Timecode: int64(i)*600 + 100, Keyframe: true, Duration: 500, Data: []byte("cue " + string(rune('A'+i)))})
	}
	// Four clusters of one second, the subtitle blocks interleaved by time, every block cued.
	var sets [][]mkv.Block
	for c := 0; c < 4; c++ {
		var set []mkv.Block
		for _, b := range video[c*25 : (c+1)*25] {
			set = append(set, b)
			for _, s := range subs {
				if s.Timecode >= b.Timecode && s.Timecode < b.Timecode+40 {
					set = append(set, s)
				}
			}
		}
		sets = append(sets, set)
	}
	dir := t.TempDir()
	build := func(name string, tags []mkv.Tag) string {
		return buildMKVCuedBlocks(t, dir, name, []mkv.Track{
			{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
			{ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: "srt"},
		}, sets, 4000, tags)
	}
	plain := build("plain.mkv", nil)
	ctx := context.Background()
	c, err := reader.OpenMetaWithFS(ctx, plain, nil, reader.WithCues(), reader.WithTags())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(plain)
	var subCues int
	for _, cue := range c.Cues {
		if cue.Track == 2 {
			subCues++
			if cue.RelativePos <= 0 {
				t.Fatalf("the writer must record CueRelativePosition: %+v", cue)
			}
		}
	}
	if subCues != 6 {
		t.Fatalf("fixture: %d subtitle cues, want 6", subCues)
	}
	derived, verified, err := SubtitleIndexFromCues(c, st.Size(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if verified[2] {
		t.Errorf("without a NUMBER_OF_FRAMES tag the track must stay unverified")
	}
	if _, err := derived.MarshalBinary(); err == nil {
		t.Errorf("an unresolved index must refuse to marshal")
	}
	if derived.TrackBlocks(2) != nil {
		t.Errorf("an unresolved index must not hand out positions")
	}
	if err := derived.Resolve(ctx, plain); err != nil {
		t.Fatal(err)
	}
	walked, err := BuildSubtitleIndex(ctx, plain, []uint64{2})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := derived.TrackBlocks(2), walked.TrackBlocks(2); !reflect.DeepEqual(got, want) {
		t.Errorf("derived index differs from the walk:\n got %+v\nwant %+v", got, want)
	}
	if _, err := derived.MarshalBinary(); err != nil {
		t.Errorf("resolved index must marshal: %v", err)
	}
	// With trusted statistics naming 6 frames the track is verified; a wrong count is not.
	tagsFor := func(frames string) []mkv.Tag {
		return []mkv.Tag{{TargetID: 22, SimpleTags: []mkv.SimpleTag{{Name: "_STATISTICS_WRITING_APP", Value: "test"}, {Name: "DURATION", Value: "00:00:03.700000000"}, {Name: "NUMBER_OF_FRAMES", Value: frames}}}}
	}
	for frames, want := range map[string]bool{"6": true, "7": false} {
		p := build("tagged"+frames+".mkv", tagsFor(frames))
		c2, err := reader.OpenMetaWithFS(ctx, p, nil, reader.WithCues(), reader.WithTags())
		if err != nil {
			t.Fatal(err)
		}
		if _, v, err := SubtitleIndexFromCues(c2, 0, []uint64{2}); err != nil || v[2] != want {
			t.Errorf("NUMBER_OF_FRAMES=%s: verified=%v, want %v", frames, v[2], want)
		}
	}
}

// ResolveClusterBlock reads a cluster's header and timestamp and lands on the block the relative offset names.
func TestResolveClusterBlock(t *testing.T) {
	w, h := uint32(320), uint32(240)
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: 2000, Keyframe: true, Data: make([]byte, 50)}, {TrackNumber: 1, Timecode: 2040, Data: make([]byte, 60)}}
	src := buildMKVCuedBlocks(t, t.TempDir(), "r.mkv", []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h}}, [][]mkv.Block{blocks}, 2080, nil)
	c, err := reader.OpenMetaWithFS(context.Background(), src, nil, reader.WithCues())
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(src)
	defer f.Close()
	cue := c.Cues[0]
	pos, err := reader.ResolveClusterBlock(f, c.SegmentStart+cue.ClusterPos, cue.RelativePos)
	if err != nil {
		t.Fatal(err)
	}
	if pos.ClusterTS != 2000*1_000_000/c.Info.TimecodeScale || pos.ClusterEnd <= pos.Off {
		t.Fatalf("resolved %+v", pos)
	}
	br, err := reader.NewBlockReaderFrom(f, c.Info.TimecodeScale, pos)
	if err != nil {
		t.Fatal(err)
	}
	b, err := br.Next()
	if err != nil || b.Timecode != 2000 || len(b.Data) != 50 {
		t.Errorf("seated block: %+v err %v", b, err)
	}
}

type failAfterReads struct {
	mkv.ReadSeekCloser
	left int
}

func (f *failAfterReads) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, errors.New("interrupted")
	}
	f.left--
	return f.ReadSeekCloser.Read(p)
}

// A Resolve interrupted midway leaves the index unresolved; the next call must
// settle the remaining entries without shifting the ones already settled.
func TestResolveInterruptedThenResumed(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var sets [][]mkv.Block
	for c := 0; c < 6; c++ {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: int64(c) * 1000, Keyframe: true, Data: make([]byte, 200)},
			{TrackNumber: 2, Timecode: int64(c)*1000 + 100, Keyframe: true, Duration: 500, Data: []byte("cue")},
		})
	}
	src := buildMKVCuedBlocks(t, t.TempDir(), "r.mkv", []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
		{ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: "srt"},
	}, sets, 6000, nil)
	ctx := context.Background()
	c, err := reader.OpenMetaWithFS(ctx, src, nil, reader.WithCues())
	if err != nil {
		t.Fatal(err)
	}
	ix, _, err := SubtitleIndexFromCues(c, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	failing := &mkv.FS{Open: func(path string) (mkv.ReadSeekCloser, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return &failAfterReads{ReadSeekCloser: f, left: 3}, nil
	}}
	if err := ix.Resolve(ctx, src, mkv.Options{FS: failing}); err == nil {
		t.Fatal("the interrupted Resolve must fail")
	}
	if !ix.Unresolved() {
		t.Fatal("an interrupted index must stay unresolved")
	}
	if err := ix.Resolve(ctx, src); err != nil {
		t.Fatal(err)
	}
	walked, err := BuildSubtitleIndex(ctx, src, []uint64{2})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ix.TrackBlocks(2), walked.TrackBlocks(2); !reflect.DeepEqual(got, want) {
		t.Errorf("resumed index differs from the walk:\n got %+v\nwant %+v", got, want)
	}
}
