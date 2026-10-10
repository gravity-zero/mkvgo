package mp4_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

// cuedFixture writes a 60 s source whose Cues name every subtitle block with
// its CueRelativePosition, as muxers do; frames gives the NUMBER_OF_FRAMES
// statistic written per subtitle track UID (none when nil); skew misplaces one
// subtitle cue one byte into its block when set.
func cuedFixture(t testing.TB, frames map[uint64]string, skew bool) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{
		{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: []byte{0x01, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}, Width: &w, Height: &h},
		{ID: 2, UID: 22, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
		{ID: 3, UID: 33, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Name: "forcé"},
	}
	frame := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65}, make([]byte, 2<<10)...)
	var blks []mkv.Block
	for i := 0; i < 300; i++ {
		blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 200, Keyframe: i%5 == 0, Data: frame})
	}
	subs := []mkv.Block{
		{TrackNumber: 2, Timecode: 500, Duration: 4000, Data: []byte("A chevauche")},
		{TrackNumber: 2, Timecode: 5900, Duration: 300, Data: []byte("B")},
		{TrackNumber: 2, Timecode: 7900, Data: []byte("C sans durée")},
		{TrackNumber: 2, Timecode: 8400, Data: []byte("D sans durée")},
		{TrackNumber: 2, Timecode: 20000, Duration: 12000, Data: []byte("E longue")},
		{TrackNumber: 2, Timecode: 59500, Data: []byte("F finale")},
		{TrackNumber: 3, Timecode: 40000, Duration: 2000, Data: []byte("piste creuse")},
	}
	for _, b := range subs {
		b.Keyframe = true
		blks = append(blks, b)
	}
	for i := 1; i < len(blks); i++ {
		for j := i; j > 0 && blks[j].Timecode < blks[j-1].Timecode; j-- {
			blks[j], blks[j-1] = blks[j-1], blks[j]
		}
	}
	var tags []mkv.Tag
	for uid, n := range frames {
		tags = append(tags, mkv.Tag{TargetID: uid, SimpleTags: []mkv.SimpleTag{
			{Name: "_STATISTICS_WRITING_APP", Value: "mkvgo-test"}, {Name: "DURATION", Value: "00:00:59.500000000"}, {Name: "NUMBER_OF_FRAMES", Value: n}}})
	}
	path := filepath.Join(t.TempDir(), "cued.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}, Tags: tags}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 60_000); err != nil {
		t.Fatal(err)
	}
	skewed := false
	start := 0
	for i := 1; i <= len(blks); i++ {
		if i < len(blks) && blks[i].Timecode-blks[start].Timecode < 1000 {
			continue
		}
		cluster := blks[start:i]
		clusterPos := m.RelPos()
		offsets, err := writer.WriteClusterOffsets(f, cluster[0].Timecode, scale, cluster)
		if err != nil {
			t.Fatal(err)
		}
		for k, b := range cluster {
			if b.TrackNumber == 1 && !b.Keyframe {
				continue
			}
			rel := offsets[k]
			if skew && !skewed && b.TrackNumber == 2 && k > 0 {
				rel, skewed = offsets[k]+1, true
			}
			m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: b.Timecode, Track: b.TrackNumber, ClusterPos: clusterPos, RelativePos: rel})
		}
		start = i
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A plan derives its subtitle index from the source's Cues when they cue
// every block of a track and a trusted NUMBER_OF_FRAMES says so: it then
// serves, without walking, the very WebVTT the walk serves. A track whose cue
// count the statistics do not confirm, or a cue that does not land on the
// block it names, walks as before.
func TestPlanHLSSubtitleIndexDerivedFromCues(t *testing.T) {
	ctx := context.Background()
	full := map[uint64]string{22: "6", 33: "1"}
	reference := cuedFixture(t, nil, false)
	walked, err := mp4.PlanHLS(ctx, reference, mp4.Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		src         string
		indexed     int64
		unverified  int64
		walks       bool
		blocksByPos bool
	}{
		"verified":          {cuedFixture(t, full, false), 2, 0, false, true},
		"no statistics":     {reference, 0, 2, true, false},
		"count mismatch":    {cuedFixture(t, map[uint64]string{22: "7", 33: "1"}, false), 1, 1, true, true},
		"cue off its block": {cuedFixture(t, full, true), 2, 0, true, true},
	}
	for name, tc := range cases {
		for orderName, order := range subOrders {
			t.Run(name+"/"+orderName, func(t *testing.T) {
				plan, err := mp4.PlanHLS(ctx, tc.src, mp4.Options{SegmentMs: 2000})
				if err != nil {
					t.Fatal(err)
				}
				st := plan.Stats()
				if st.SubtitleCueIndexed != tc.indexed || st.SubtitleCueUnverified != tc.unverified {
					t.Fatalf("cue index: %d verified, %d unverified; want %d, %d", st.SubtitleCueIndexed, st.SubtitleCueUnverified, tc.indexed, tc.unverified)
				}
				for _, res := range subNames(walked, order) {
					want, _, werr := walked.Resource(ctx, res)
					got, _, gerr := plan.Resource(ctx, res)
					if werr != nil || gerr != nil {
						t.Fatalf("%s: walked %v, derived %v", res, werr, gerr)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("%s differs:\n%s\n--- walk:\n%s", res, got, want)
					}
				}
				st = plan.Stats()
				if (st.SubtitleWalks > 0) != tc.walks || (st.SubtitleIndexedBlocks > 0) != tc.blocksByPos {
					t.Errorf("%d walks, %d blocks by position; want walks=%v, byPosition=%v", st.SubtitleWalks, st.SubtitleIndexedBlocks, tc.walks, tc.blocksByPos)
				}
			})
		}
	}
	if st := walked.Stats(); st.SubtitleWalks == 0 {
		t.Errorf("the reference plan must have walked")
	}
}
