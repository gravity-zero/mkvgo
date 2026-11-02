package mp4

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// cuedOnTrackMKV writes blocks in one-second clusters, each cued on cueTrack only.
func cuedOnTrackMKV(t *testing.T, tracks []mkv.Track, blocks []genBlock, cueTrack uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cued.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const scale = 1_000_000
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteMetadata(c, tracks, blocks[len(blocks)-1].pts); err != nil {
		t.Fatal(err)
	}
	var blks []mkv.Block
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
	}
	start := 0
	for i := 1; i <= len(blks); i++ {
		if i == len(blks) || blks[i].Timecode-blks[start].Timecode >= 1000 {
			m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: blks[start].Timecode, Track: cueTrack, ClusterPos: m.RelPos()})
			if err := writer.WriteCluster(m.W, blks[start].Timecode, scale, blks[start:i]); err != nil {
				t.Fatal(err)
			}
			start = i
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlanRefusalSentinels: the plan's and concat's refusals carry the
// sentinel of their condition, as the library's other entry points do.
func TestPlanRefusalSentinels(t *testing.T) {
	ctx := context.Background()
	w, h := uint32(320), uint32(240)
	sr, ch := 48000.0, uint8(2)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}
	audio := mkv.Track{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}
	var av, audioOnly []genBlock
	for i := 0; i < 75; i++ {
		v := genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}}
		a := genBlock{track: 2, pts: int64(i) * 40, key: true, data: []byte{0xAA, byte(i)}}
		av = append(av, v, a)
		audioOnly = append(audioOnly, genBlock{track: 1, pts: a.pts, key: true, data: a.data})
	}
	check := func(name string, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want errors.Is %v", name, err, want)
		}
	}

	_, err := PlanHLS(ctx, pkgcovNoCuesMKV(t, []mkv.Track{video, audio}, av), Options{SegmentMs: 1000})
	check("PlanHLS, no Cues", err, mkv.ErrNoKeyframeIndex)
	_, err = PlanHLS(ctx, cuedOnTrackMKV(t, []mkv.Track{video, audio}, av, 2), Options{SegmentMs: 1000})
	check("PlanHLS, Cues on audio only", err, mkv.ErrNoKeyframeIndex)

	src := headCuesFixture(t, []mkv.Track{video}, videoBlocks(av))
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := reader.ReadMeta(ctx, bytes.NewReader(data), src, reader.WithCues())
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.mkv")
	if err := os.WriteFile(cut, data[:meta.SegmentStart+meta.Cues[0].ClusterPos], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = PlanHLS(ctx, cut, Options{SegmentMs: 1000})
	check("PlanHLS, cut before the first cued cluster", err, mkv.ErrSourceTruncated)

	audioPart := cuedOnTrackMKV(t, []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}}, audioOnly, 1)
	_, err = PlanConcat(ctx, []string{buildMKV(t, []mkv.Track{video}, videoBlocks(av)), audioPart}, Options{SegmentMs: 1000})
	check("PlanConcat, a part without video", err, mkv.ErrNoVideo)
}

func videoBlocks(blocks []genBlock) []genBlock {
	var out []genBlock
	for _, b := range blocks {
		if b.track == 1 {
			out = append(out, b)
		}
	}
	return out
}
