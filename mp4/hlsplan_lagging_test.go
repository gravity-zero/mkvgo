package mp4

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildLaggingAudioSourceLead writes a source whose audio is stored lagSec
// seconds of video behind its video (cued too when cueAudio is set). With
// lead, the audio's very first block - t=0, one frame - sits at the head of
// the file beside the first video, alone in its cluster, and the rest of the
// track trails behind: the shape of a real file whose audio lies hundreds of
// megabytes from its video.
func buildLaggingAudioSourceLead(tb testing.TB, seconds, lagSec int, cueAudio, lead bool) string {
	tb.Helper()
	const (
		fps        = 25
		gopSec     = 2
		clusterMs  = 2000
		keyBytes   = 80 << 10
		deltaBytes = 14 << 10
		audioBytes = 1792
		audioMs    = 32
		scale      = 1_000_000
	)
	sample := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = seed + byte(i)
		}
		return b
	}
	w, h := uint32(1920), uint32(800)
	sr := 48000.0
	ch := uint8(6)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
	}
	path := filepath.Join(tb.TempDir(), "lagging.mkv")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		tb.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, int64(seconds)*1000); err != nil {
		tb.Fatal(err)
	}
	video := func(cs int64) {
		clusterPos := m.RelPos()
		var blks []mkv.Block
		for fr := cs * fps / 1000; fr*1000/fps < cs+clusterMs; fr++ {
			ms := fr * 1000 / fps
			if ms < cs {
				continue
			}
			key := fr%(gopSec*fps) == 0
			data := sample(deltaBytes, byte(fr))
			if key {
				data = sample(keyBytes, byte(fr))
				m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: ms, Track: 1, ClusterPos: clusterPos})
			}
			blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: ms, Keyframe: key, Data: data})
		}
		if err := writer.WriteCluster(m.W, cs, scale, blks); err != nil {
			tb.Fatal(err)
		}
	}
	audio := func(cs int64) {
		if cueAudio {
			m.Cues = append(m.Cues, mkv.CuePoint{TimeMs: cs, Track: 2, ClusterPos: m.RelPos()})
		}
		var blks []mkv.Block
		for ms := cs; ms < cs+clusterMs; ms += audioMs {
			if lead && ms == 0 {
				continue // written at the head instead
			}
			blks = append(blks, mkv.Block{TrackNumber: 2, Timecode: ms, Keyframe: true, Data: sample(audioBytes, byte(ms))})
		}
		if err := writer.WriteCluster(m.W, cs, scale, blks); err != nil {
			tb.Fatal(err)
		}
	}
	totalMs, lagMs := int64(seconds)*1000, int64(lagSec)*1000
	for cs := int64(0); cs < totalMs+lagMs; cs += clusterMs {
		if cs < totalMs {
			video(cs)
		}
		if lead && cs == 0 {
			blks := []mkv.Block{{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: sample(audioBytes, 0)}}
			if err := writer.WriteCluster(m.W, 0, scale, blks); err != nil {
				tb.Fatal(err)
			}
		}
		if a := cs - lagMs; a >= 0 && a < totalMs {
			audio(a)
		}
	}
	if err := m.Finalize(); err != nil {
		tb.Fatal(err)
	}
	return path
}

// A real file can store an audio track's first second beside the video and
// the rest of it far behind - further than one probe of the search reaches.
// Planning must not walk to the track's second block to learn its frame
// stride (its first block holds one frame: there is no stride to learn), and
// every cold segment must still be the full pass's.
func TestPlanHLSTrackFarBehindAfterItsFirstSecond(t *testing.T) {
	ctx := context.Background()
	src := buildLaggingAudioSourceLead(t, 240, 180, false, true) // ~47 MiB behind, past one probe's reach
	full := t.TempDir()
	if err := RemuxToHLS(ctx, src, full, Options{SegmentMs: 6000}); err != nil {
		t.Fatal(err)
	}
	tally := &readTally{}
	probe, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, FS: countingFS(tally)})
	if err != nil {
		t.Fatal(err)
	}
	// The tail peek reads the audio stored after the last video (its last
	// 180 s, ~10 MiB); walking toward the audio's second block from the head
	// would read the ~47 MiB of video in between on top of it.
	if tally.bytes > 24<<20 {
		t.Errorf("planning read %.1f MiB: it walked toward the audio's second block", float64(tally.bytes)/(1<<20))
	}
	fts := probe.fts()
	for _, n := range rand.New(rand.NewSource(9)).Perm(probe.NumSegments()) {
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
		if err != nil {
			t.Fatal(err)
		}
		for i := range fts {
			name := renditionSegment(fts, i, n)
			got, _, err := plan.Resource(ctx, name)
			if err != nil {
				t.Fatalf("%s, asked for cold: %v", name, err)
			}
			want, err := os.ReadFile(filepath.Join(full, name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s, asked for cold, differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
			}
		}
	}
	for _, name := range []string{"init.mp4", "init_a1.mp4", "playlist.m3u8", "audio1.m3u8"} {
		got, _, err := probe.Resource(ctx, name)
		want, ferr := os.ReadFile(filepath.Join(full, name))
		if err != nil || ferr != nil || !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%v, %v)", name, err, ferr)
		}
	}
}
