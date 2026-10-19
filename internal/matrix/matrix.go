// Package matrix generates the synthetic Matroska sources split and join are held to, one per combination of the traits that broke them.
package matrix

import (
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// Subtitle is how a case's one text cue sits against the cuts.
type Subtitle int

const (
	NoSubtitle     Subtitle = iota
	CueOverCut              // a cue on screen across the first cut
	CueOverTwoCuts          // a cue on screen across both cuts
)

// Chapters is how a case's chapters are stated.
type Chapters int

const (
	NoChapters    Chapters = iota
	OpenChapters           // starts only, the second running over the first cut
	EndedChapters          // the same with every end stated
)

// Track numbers of the generated sources.
const (
	VideoTrack    = 1
	AudioTrack    = 2
	SubtitleTrack = 3
	SecondVideo   = 4
)

// Case is one source: its traits, and the cuts a split asks for.
type Case struct {
	Audio    bool
	OpenGOP  bool // two leading pictures stored after every keyframe but the first
	NTSC     bool // 29.97 fps: keyframes, hence cuts, fall between milliseconds on a fine timescale
	Subtitle Subtitle
	Chapters Chapters
	Cuts     int  // 1 or 2
	Late     bool // the cuts asked 300 ms past a keyframe: each part opens on the next one
	Second   bool // a second video track whose keyframes fall every 1.5 GOP, between the cuts
}

// Name labels a case for a subtest.
func (c Case) Name() string {
	return fmt.Sprintf("audio=%v/opengop=%v/ntsc=%v/sub=%d/chapters=%d/cuts=%d/late=%v/second=%v", c.Audio, c.OpenGOP, c.NTSC, c.Subtitle, c.Chapters, c.Cuts, c.Late, c.Second)
}

// Cases is every combination, a cue over two cuts only where there are two.
func Cases() []Case {
	var out []Case
	for _, audio := range []bool{false, true} {
		for _, open := range []bool{false, true} {
			for _, ntsc := range []bool{false, true} {
				for _, sub := range []Subtitle{NoSubtitle, CueOverCut, CueOverTwoCuts} {
					for _, chs := range []Chapters{NoChapters, OpenChapters, EndedChapters} {
						for _, cuts := range []int{1, 2} {
							if sub == CueOverTwoCuts && cuts == 1 {
								continue
							}
							for _, late := range []bool{false, true} {
								out = append(out, Case{Audio: audio, OpenGOP: open, NTSC: ntsc, Subtitle: sub, Chapters: chs, Cuts: cuts, Late: late})
							}
						}
					}
				}
			}
		}
	}
	return out
}

// SecondVideoCases are the cases with a second video track, its GOP out of step with the first: the cuts land inside it.
func SecondVideoCases() []Case {
	var out []Case
	for _, ntsc := range []bool{false, true} {
		for _, cuts := range []int{1, 2} {
			for _, late := range []bool{false, true} {
				for _, sub := range []Subtitle{NoSubtitle, CueOverCut} {
					out = append(out, Case{Audio: true, NTSC: ntsc, Subtitle: sub, Chapters: OpenChapters, Cuts: cuts, Late: late, Second: true})
				}
			}
			// Without sound the cut track alone says where the cut falls, its leading pictures hidden past it.
			out = append(out, Case{OpenGOP: true, NTSC: ntsc, Chapters: OpenChapters, Cuts: cuts, Second: true})
		}
	}
	return out
}

// frameNs and gop are the video cadence: 25 fps with a keyframe a second, or 30000/1001 fps with one every 25 frames, so keyframes fall between milliseconds.
func (c Case) frameNs() float64 {
	if c.NTSC {
		return 1e9 * 1001 / 30000
	}
	return 1e9 / 25
}

func (c Case) gop() int {
	return 25
}

func (c Case) frames() int { return int(6e9 / c.frameNs()) }

func (c Case) frameMs(j int) int64 { return int64(math.Round(float64(j) * c.frameNs() / 1e6)) }

// EndMs is where the video ends: the last frame plus its whole milliseconds, as a remux measures it; chapters end there.
func (c Case) EndMs() int64 { return c.frameMs(c.frames()-1) + int64(c.frameNs()/1e6) }

// DurationMs is where the longest track ends: the audio's 300 frames of 20 ms run past an NTSC video's end.
func (c Case) DurationMs() int64 {
	if c.Audio {
		return max(c.EndMs(), 300*20)
	}
	return c.EndMs()
}

// RequestedCuts are the times the split ranges name, in ms.
func (c Case) RequestedCuts() []int64 {
	cuts := []int64{3000}
	if c.Cuts == 2 {
		cuts = []int64{2000, 4000}
	}
	if c.Late {
		for i := range cuts {
			cuts[i] += 300
		}
	}
	return cuts
}

// ExpectedCuts are where the parts really open: the first keyframe at or after each requested cut.
func (c Case) ExpectedCuts() []int64 {
	var out []int64
	for _, want := range c.RequestedCuts() {
		for j := 0; j < c.frames(); j += c.gop() {
			if ms := c.frameMs(j); ms >= want {
				out = append(out, ms)
				break
			}
		}
	}
	return out
}

// Ranges are the split ranges: from 0 through each requested cut to the end.
func (c Case) Ranges() []mkv.TimeRange {
	cuts := c.RequestedCuts()
	out := make([]mkv.TimeRange, 0, len(cuts)+1)
	start := int64(0)
	for _, cut := range cuts {
		out = append(out, mkv.TimeRange{StartMs: start, EndMs: cut})
		start = cut
	}
	return append(out, mkv.TimeRange{StartMs: start})
}

// Tracks are the case's track entries.
func (c Case) Tracks() []mkv.Track {
	w, h := uint32(320), uint32(240)
	tracks := []mkv.Track{{ID: VideoTrack, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcC, Width: &w, Height: &h, DefaultDurationNs: int64(math.Round(c.frameNs()))}}
	if c.Audio {
		sr, ch := 48000.0, uint8(2)
		tracks = append(tracks, mkv.Track{ID: AudioTrack, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: asc, SampleRate: &sr, Channels: &ch, DefaultDurationNs: 20_000_000})
	}
	if c.Subtitle != NoSubtitle {
		tracks = append(tracks, mkv.Track{ID: SubtitleTrack, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"})
	}
	if c.Second {
		tracks = append(tracks, mkv.Track{ID: SecondVideo, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcC, Width: &w, Height: &h, DefaultDurationNs: int64(math.Round(c.frameNs()))})
	}
	return tracks
}

// Blocks are the case's frames per track, in storage order: what a split and its join must give back.
func (c Case) Blocks() map[uint64][]mkv.Block {
	out := map[uint64][]mkv.Block{}
	var video []mkv.Block
	for j := 0; j < c.frames(); j++ {
		video = append(video, mkv.Block{TrackNumber: VideoTrack, Timecode: c.frameMs(j), Keyframe: j%c.gop() == 0, Data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(j >> 8), byte(j)}})
	}
	if c.OpenGOP {
		// Every keyframe but the first is stored ahead of the two pictures shown just before it.
		for j := c.gop(); j < len(video); j += c.gop() {
			k := video[j]
			copy(video[j-1:j+1], video[j-2:j])
			video[j-2] = k
		}
	}
	out[VideoTrack] = video
	if c.Second {
		for j := 0; j < c.frames(); j++ {
			out[SecondVideo] = append(out[SecondVideo], mkv.Block{TrackNumber: SecondVideo, Timecode: c.frameMs(j), Keyframe: j%(c.gop()*3/2) == 0, Data: []byte{0x00, 0x00, 0x00, 0x01, 0x41, byte(j >> 8), byte(j)}})
		}
	}
	if c.Audio {
		for i := 0; i < 300; i++ {
			out[AudioTrack] = append(out[AudioTrack], mkv.Block{TrackNumber: AudioTrack, Timecode: int64(i) * 20, Keyframe: true, Data: []byte{0xAA, byte(i >> 8), byte(i)}})
		}
	}
	cuts := c.ExpectedCuts()
	switch c.Subtitle {
	case CueOverCut:
		out[SubtitleTrack] = []mkv.Block{{TrackNumber: SubtitleTrack, Timecode: cuts[0] - 500, Keyframe: true, Duration: 1000, Data: []byte("Across")}}
	case CueOverTwoCuts:
		out[SubtitleTrack] = []mkv.Block{{TrackNumber: SubtitleTrack, Timecode: cuts[0] - 500, Keyframe: true, Duration: cuts[1] - cuts[0] + 1000, Data: []byte("Across")}}
	}
	return out
}

// ChapterList is the case's chapters: the second (and third) start ahead of a cut and run over it.
func (c Case) ChapterList() []mkv.Chapter {
	if c.Chapters == NoChapters {
		return nil
	}
	starts := []int64{0, c.ExpectedCuts()[0] - 500}
	if c.Cuts == 2 {
		starts = append(starts, c.ExpectedCuts()[1]-500)
	}
	chs := make([]mkv.Chapter, len(starts))
	for i, s := range starts {
		chs[i] = mkv.Chapter{ID: uint64(i + 1), Title: fmt.Sprintf("Chapter %d", i+1), StartMs: s}
		if c.Chapters == EndedChapters {
			chs[i].EndMs = c.EndMs()
			if i+1 < len(starts) {
				chs[i].EndMs = starts[i+1]
			}
		}
	}
	return chs
}

// BuildMKV writes the case's source to path: seekable, ~1 s clusters, Cues.
func (c Case) BuildMKV(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	// Tracks interleave by decode time, each in its own storage order: a video frame decodes at its storage slot.
	type stored struct {
		dts int64
		b   mkv.Block
	}
	var all []stored
	for track, bs := range c.Blocks() {
		for j, b := range bs {
			dts := b.Timecode
			if track == VideoTrack {
				dts = c.frameMs(j)
			}
			all = append(all, stored{dts, b})
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].dts != all[b].dts {
			return all[a].dts < all[b].dts
		}
		return all[a].b.TrackNumber < all[b].b.TrackNumber
	})
	blocks := make([]mkv.Block, len(all))
	for i, x := range all {
		blocks[i] = x.b
	}
	const scale = 1_000_000
	m := writer.NewMKVWriter(f)
	dur := c.DurationMs()
	cont := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-matrix", WritingApp: "mkvgo-matrix"}, Chapters: c.ChapterList()}
	err = m.WriteStart()
	if err == nil {
		err = m.WriteMetadata(cont, c.Tracks(), dur)
	}
	for start := 0; err == nil && start < len(blocks); {
		end := start + 1
		for end < len(blocks) && blocks[end].Timecode-blocks[start].Timecode < 1000 {
			end++
		}
		err = m.WriteClusterWithCues(blocks[start].Timecode, scale, blocks[start:end])
		start = end
	}
	if err == nil {
		err = m.Finalize()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

var (
	avcC = []byte{0x01, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}
	asc  = []byte{0x11, 0x90} // AAC-LC, 48000 Hz, stereo
)
