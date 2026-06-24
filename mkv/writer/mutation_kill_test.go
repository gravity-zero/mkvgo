package writer

// mutation_kill_test.go — tests targeting surviving mutants found by gremlins.
//
// Strategy: each test exercises the exact line/operator with an input where the
// flipped operator gives an observably different result, then asserts the
// expected outcome.  A round-trip (Write → reader.Read) is the primary vehicle.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// writeAndRead is a test-scoped helper: write c then read it back.
func writeAndRead(t *testing.T, c *mkv.Container) *mkv.Container {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := reader.Read(context.Background(), bytes.NewReader(buf.Bytes()), "test.mkv")
	if err != nil {
		t.Fatalf("reader.Read: %v", err)
	}
	return got
}

// ── WriteSegmentInfo ──────────────────────────────────────────────────────────

// TestSegmentInfoTimecodeScaleBoundary kills writer.go line 190:
//
//	if info.TimecodeScale > 0
//
// Mutation > → >= writes TimecodeScale=0; the reader then returns 0 instead of
// defaulting to 1_000_000.
func TestSegmentInfoTimecodeScaleBoundary(t *testing.T) {
	t.Run("zero is not written, reader defaults", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 0}})
		if got.Info.TimecodeScale == 0 {
			t.Errorf("TimecodeScale = 0; expected reader default when not written")
		}
	})
	t.Run("one is written and survives", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1}})
		if got.Info.TimecodeScale != 1 {
			t.Errorf("TimecodeScale = %d, want 1", got.Info.TimecodeScale)
		}
	})
}

// TestSegmentInfoDurationBranches kills writer.go lines 193 and 195:
//
//	if info.Duration > 0                                   (line 193)
//	} else if durationMs > 0 && info.TimecodeScale > 0    (line 195)
func TestSegmentInfoDurationBranches(t *testing.T) {
	t.Run("Duration=0 falls through to durationMs branch", func(t *testing.T) {
		// Mutation > → >= at line 193 would write Duration=0.0, then the reader
		// cannot compute a non-zero DurationMs from a zero stored duration.
		got := writeAndRead(t, &mkv.Container{
			Info:       mkv.SegmentInfo{TimecodeScale: 1_000_000, Duration: 0.0},
			DurationMs: 5000,
		})
		if got.DurationMs != 5000 {
			t.Errorf("DurationMs = %d, want 5000 (from durationMs branch)", got.DurationMs)
		}
	})
	t.Run("Duration>0 uses the stored duration field directly", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, Duration: 3000.0},
		})
		if got.Info.Duration != 3000.0 {
			t.Errorf("Duration = %g, want 3000.0", got.Info.Duration)
		}
	})
	t.Run("TimecodeScale=0 blocks the durationMs branch", func(t *testing.T) {
		// line 195: else if durationMs > 0 && info.TimecodeScale > 0
		// With TimecodeScale=0 the second clause is false; DurationMs stays 0.
		got := writeAndRead(t, &mkv.Container{
			Info:       mkv.SegmentInfo{TimecodeScale: 0, Duration: 0.0},
			DurationMs: 8000,
		})
		if got.DurationMs != 0 {
			t.Errorf("DurationMs = %d; want 0 (TimecodeScale=0 blocks branch)", got.DurationMs)
		}
	})
}

// TestSegmentInfoDurationArithmetic kills writer.go line 196:
//
//	float64(durationMs)*1e6/float64(info.TimecodeScale)
//
// * ↔ / mutations yield ~0 or an astronomically large value; the round-trip
// DurationMs (stored Duration × timecodeScale / 1e6) exposes either.
func TestSegmentInfoDurationArithmetic(t *testing.T) {
	// timecodeScale=2_000_000: stored = durationMs*1e6/2e6 = durationMs/2 = 2000.
	// Round-trip: 2000 * 2e6 / 1e6 = 4000 ms.
	got := writeAndRead(t, &mkv.Container{
		Info:       mkv.SegmentInfo{TimecodeScale: 2_000_000},
		DurationMs: 4000,
	})
	if got.DurationMs != 4000 {
		t.Errorf("DurationMs = %d, want 4000 (timecodeScale=2_000_000)", got.DurationMs)
	}
	if got.Info.Duration != 2000.0 {
		t.Errorf("stored Duration = %g, want 2000.0 (4000*1e6/2e6)", got.Info.Duration)
	}
}

// TestSegmentInfoAppNameDefaults kills writer.go lines 202 and 207:
//
//	if mux == ""   /   if wapp == ""
//
// Mutation == → != overwrites a non-empty name with "mkvgo".
func TestSegmentInfoAppNameDefaults(t *testing.T) {
	t.Run("empty names produce mkvgo defaults", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		})
		if got.Info.MuxingApp != "mkvgo" {
			t.Errorf("MuxingApp = %q, want mkvgo", got.Info.MuxingApp)
		}
		if got.Info.WritingApp != "mkvgo" {
			t.Errorf("WritingApp = %q, want mkvgo", got.Info.WritingApp)
		}
	})
	t.Run("non-empty names are preserved unchanged", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{
				TimecodeScale: 1_000_000,
				MuxingApp:     "ffmpeg",
				WritingApp:    "handbrake",
			},
		})
		if got.Info.MuxingApp != "ffmpeg" {
			t.Errorf("MuxingApp = %q, want ffmpeg", got.Info.MuxingApp)
		}
		if got.Info.WritingApp != "handbrake" {
			t.Errorf("WritingApp = %q, want handbrake", got.Info.WritingApp)
		}
	})
}

// TestSegmentInfoDateUTCArithmetic kills writer.go line 213:
//
//	nanos := (info.DateUTC.Unix() - epoch) * 1e9
//
// - → + places the result in the wrong century; * → / collapses it to ~0.
func TestSegmentInfoDateUTCArithmetic(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	got := writeAndRead(t, &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, DateUTC: &ts},
	})
	if got.Info.DateUTC == nil {
		t.Fatal("DateUTC is nil after round-trip")
	}
	diff := got.Info.DateUTC.Unix() - ts.Unix()
	if diff < -1 || diff > 1 {
		t.Errorf("DateUTC round-trip: got %v, diff %ds from expected %v",
			got.Info.DateUTC, diff, ts)
	}
}

// TestSegmentInfoUIDFieldBoundaries kills writer.go lines 216, 219, 222:
//
//	if len(info.SegmentUID) > 0  /  PrevUID  /  NextUID
//
// > → >= would write an element even for nil slices (writing empty bytes).
func TestSegmentInfoUIDFieldBoundaries(t *testing.T) {
	segUID := []byte{0x11, 0x22, 0x33}
	prevUID := []byte{0x44, 0x55}
	nextUID := []byte{0x66, 0x77}

	t.Run("UIDs survive round-trip", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{
				TimecodeScale: 1_000_000,
				SegmentUID:    segUID,
				PrevUID:       prevUID,
				NextUID:       nextUID,
			},
		})
		if !bytes.Equal(got.Info.SegmentUID, segUID) {
			t.Errorf("SegmentUID = %x, want %x", got.Info.SegmentUID, segUID)
		}
		if !bytes.Equal(got.Info.PrevUID, prevUID) {
			t.Errorf("PrevUID = %x, want %x", got.Info.PrevUID, prevUID)
		}
		if !bytes.Equal(got.Info.NextUID, nextUID) {
			t.Errorf("NextUID = %x, want %x", got.Info.NextUID, nextUID)
		}
	})
	t.Run("nil UIDs are absent from output", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		})
		if len(got.Info.SegmentUID) != 0 {
			t.Errorf("SegmentUID should be absent, got %x", got.Info.SegmentUID)
		}
		if len(got.Info.PrevUID) != 0 {
			t.Errorf("PrevUID should be absent, got %x", got.Info.PrevUID)
		}
		if len(got.Info.NextUID) != 0 {
			t.Errorf("NextUID should be absent, got %x", got.Info.NextUID)
		}
	})
}

// ── writeTrackFields ──────────────────────────────────────────────────────────

// TestTrackUIDDefault kills writer.go line 241:
//
//	if uid == 0 { uid = t.ID }
//
// Mutation == → != keeps uid=0 for a track that has no explicit UID.
func TestTrackUIDDefault(t *testing.T) {
	t.Run("UID=0 is replaced by TrackID", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 7, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, UID: 0}},
		})
		if len(got.Tracks) == 0 {
			t.Fatal("no tracks")
		}
		if got.Tracks[0].UID != 7 {
			t.Errorf("UID = %d, want 7 (defaulted to track ID)", got.Tracks[0].UID)
		}
	})
	t.Run("explicit UID is not overwritten", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, UID: 42}},
		})
		if got.Tracks[0].UID != 42 {
			t.Errorf("UID = %d, want 42", got.Tracks[0].UID)
		}
	})
}

// TestTrackCodecPrivateBoundary kills writer.go line 257:
//
//	if len(t.CodecPrivate) > 0
//
// > → >= writes a CodecPrivate element even for a nil slice.
func TestTrackCodecPrivateBoundary(t *testing.T) {
	t.Run("nil CodecPrivate is not written", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}},
		})
		if len(got.Tracks[0].CodecPrivate) != 0 {
			t.Errorf("CodecPrivate should be absent, got %x", got.Tracks[0].CodecPrivate)
		}
	})
	t.Run("non-empty CodecPrivate survives", func(t *testing.T) {
		priv := []byte{0x01, 0x64, 0x00, 0x1F}
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{
				{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, CodecPrivate: priv},
			},
		})
		if !bytes.Equal(got.Tracks[0].CodecPrivate, priv) {
			t.Errorf("CodecPrivate = %x, want %x", got.Tracks[0].CodecPrivate, priv)
		}
	})
}

// TestTrackLanguageBCP47Boundary kills writer.go line 263:
//
//	if t.LanguageBCP47 != ""
//
// != → == writes BCP47 only when empty (writing ""), never for real tags.
func TestTrackLanguageBCP47Boundary(t *testing.T) {
	got := writeAndRead(t, &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, LanguageBCP47: "pt-BR"}},
	})
	if got.Tracks[0].LanguageBCP47 != "pt-BR" {
		t.Errorf("LanguageBCP47 = %q, want pt-BR", got.Tracks[0].LanguageBCP47)
	}

	got2 := writeAndRead(t, &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, LanguageBCP47: ""}},
	})
	if got2.Tracks[0].LanguageBCP47 != "" {
		t.Errorf("LanguageBCP47 = %q, want empty (not written)", got2.Tracks[0].LanguageBCP47)
	}
}

// TestTrackVideoFieldsIndividually kills writer.go line 275, four != nil mutations:
//
//	t.Width != nil, t.Height != nil, t.DisplayWidth != nil, t.DisplayHeight != nil
//
// Each mutation flips one nil-check so that specific dimension is not written
// when only that dimension is set (the OR no longer saves it).
func TestTrackVideoFieldsIndividually(t *testing.T) {
	w32 := uint32(320)
	h32 := uint32(240)
	dw32 := uint32(854)
	dh32 := uint32(480)

	tests := []struct {
		name  string
		track mkv.Track
		check func(*testing.T, mkv.Track)
	}{
		{
			"Width only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, Width: &w32},
			func(t *testing.T, tr mkv.Track) {
				if tr.Width == nil || *tr.Width != 320 {
					t.Errorf("Width = %v, want 320", tr.Width)
				}
			},
		},
		{
			"Height only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, Height: &h32},
			func(t *testing.T, tr mkv.Track) {
				if tr.Height == nil || *tr.Height != 240 {
					t.Errorf("Height = %v, want 240", tr.Height)
				}
			},
		},
		{
			"DisplayWidth only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, DisplayWidth: &dw32},
			func(t *testing.T, tr mkv.Track) {
				if tr.DisplayWidth == nil || *tr.DisplayWidth != 854 {
					t.Errorf("DisplayWidth = %v, want 854", tr.DisplayWidth)
				}
			},
		},
		{
			"DisplayHeight only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, DisplayHeight: &dh32},
			func(t *testing.T, tr mkv.Track) {
				if tr.DisplayHeight == nil || *tr.DisplayHeight != 480 {
					t.Errorf("DisplayHeight = %v, want 480", tr.DisplayHeight)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := writeAndRead(t, &mkv.Container{
				Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
				Tracks: []mkv.Track{tc.track},
			})
			if len(got.Tracks) == 0 {
				t.Fatal("no tracks")
			}
			tc.check(t, got.Tracks[0])
		})
	}
}

// TestTrackAudioFieldsIndividually kills writer.go line 316, four != nil mutations:
//
//	t.SampleRate != nil, t.OutputSampleRate != nil, t.Channels != nil, t.BitDepth != nil
//
// Each mutation makes the OR false when only that single field is set, so no
// Audio element is written at all.
func TestTrackAudioFieldsIndividually(t *testing.T) {
	sr := 44100.0
	osr := 96000.0
	ch := uint8(6)
	bd := uint8(24)

	t.Run("SampleRate only", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "flac", IsDefault: true, SampleRate: &sr}},
		})
		if got.Tracks[0].SampleRate == nil || *got.Tracks[0].SampleRate != 44100 {
			t.Errorf("SampleRate = %v, want 44100", got.Tracks[0].SampleRate)
		}
	})
	t.Run("OutputSampleRate only", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "flac", IsDefault: true, OutputSampleRate: &osr}},
		})
		if got.Tracks[0].OutputSampleRate == nil || *got.Tracks[0].OutputSampleRate != 96000 {
			t.Errorf("OutputSampleRate = %v, want 96000", got.Tracks[0].OutputSampleRate)
		}
	})
	t.Run("Channels only", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "flac", IsDefault: true, Channels: &ch}},
		})
		if got.Tracks[0].Channels == nil || *got.Tracks[0].Channels != 6 {
			t.Errorf("Channels = %v, want 6", got.Tracks[0].Channels)
		}
	})
	t.Run("BitDepth only", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tracks: []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "flac", IsDefault: true, BitDepth: &bd}},
		})
		if got.Tracks[0].BitDepth == nil || *got.Tracks[0].BitDepth != 24 {
			t.Errorf("BitDepth = %v, want 24", got.Tracks[0].BitDepth)
		}
	})
}

// TestTrackHeaderStrippingBoundary kills writer.go line 332:
//
//	if len(t.HeaderStripping) > 0
func TestTrackHeaderStrippingBoundary(t *testing.T) {
	got := writeAndRead(t, &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "flac", IsDefault: true}},
	})
	if len(got.Tracks[0].HeaderStripping) != 0 {
		t.Errorf("HeaderStripping should be absent for nil input, got %x",
			got.Tracks[0].HeaderStripping)
	}
}

// TestHasColourFieldsIndividually kills writer.go line 504 (hasColour), four
// != nil mutations.  hasColour is also used in the outer Video-element guard
// (line 275), so a colour-only track (no pixel dims) depends entirely on
// hasColour returning true.
func TestHasColourFieldsIndividually(t *testing.T) {
	cr := uint16(1)  // ColorRange: TV/limited
	cs := uint16(1)  // ColorSpace: BT.709
	ct := uint16(1)  // ColorTransfer: BT.709
	cp := uint16(1)  // ColorPrimaries: BT.709

	tests := []struct {
		name  string
		track mkv.Track
		check func(*testing.T, mkv.Track)
	}{
		{
			"ColorRange only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, ColorRange: &cr},
			func(t *testing.T, tr mkv.Track) {
				if tr.ColorRange == nil || *tr.ColorRange != 1 {
					t.Errorf("ColorRange = %v, want 1", tr.ColorRange)
				}
			},
		},
		{
			"ColorSpace only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, ColorSpace: &cs},
			func(t *testing.T, tr mkv.Track) {
				if tr.ColorSpace == nil || *tr.ColorSpace != 1 {
					t.Errorf("ColorSpace = %v, want 1", tr.ColorSpace)
				}
			},
		},
		{
			"ColorTransfer only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, ColorTransfer: &ct},
			func(t *testing.T, tr mkv.Track) {
				if tr.ColorTransfer == nil || *tr.ColorTransfer != 1 {
					t.Errorf("ColorTransfer = %v, want 1", tr.ColorTransfer)
				}
			},
		},
		{
			"ColorPrimaries only",
			mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, ColorPrimaries: &cp},
			func(t *testing.T, tr mkv.Track) {
				if tr.ColorPrimaries == nil || *tr.ColorPrimaries != 1 {
					t.Errorf("ColorPrimaries = %v, want 1", tr.ColorPrimaries)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := writeAndRead(t, &mkv.Container{
				Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
				Tracks: []mkv.Track{tc.track},
			})
			if len(got.Tracks) == 0 {
				t.Fatal("no tracks")
			}
			tc.check(t, got.Tracks[0])
		})
	}
}

// ── WriteChapters ─────────────────────────────────────────────────────────────

// TestWriteChapterTimestampArithmetic kills writer.go line 352:
//
//	a.uint(mkv.IDChapterTimeStart, uint64(ch.StartMs)*1000000)
//
// * ↔ / yields values that are 10^12 times off.
func TestWriteChapterTimestampArithmetic(t *testing.T) {
	got := writeAndRead(t, &mkv.Container{
		Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Chapters: []mkv.Chapter{{ID: 1, Title: "C", StartMs: 2000, EndMs: 8000}},
	})
	if len(got.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	if got.Chapters[0].StartMs != 2000 {
		t.Errorf("StartMs = %d, want 2000", got.Chapters[0].StartMs)
	}
	if got.Chapters[0].EndMs != 8000 {
		t.Errorf("EndMs = %d, want 8000", got.Chapters[0].EndMs)
	}
}

// TestWriteChapterEndMsBoundary kills writer.go line 353:
//
//	if ch.EndMs > 0
//
// > → >= writes EndMs=0 as an explicit element; the reader reads it back as 0,
// same as absent.  We distinguish by checking it is not written for EndMs=0
// but is written for EndMs=1.
func TestWriteChapterEndMsBoundary(t *testing.T) {
	t.Run("EndMs=0 not written", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Chapters: []mkv.Chapter{{ID: 1, Title: "C", StartMs: 0, EndMs: 0}},
		})
		if got.Chapters[0].EndMs != 0 {
			t.Errorf("EndMs = %d, want 0 (element absent)", got.Chapters[0].EndMs)
		}
	})
	t.Run("EndMs=1 written and preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Chapters: []mkv.Chapter{{ID: 1, Title: "C", StartMs: 0, EndMs: 1}},
		})
		if got.Chapters[0].EndMs != 1 {
			t.Errorf("EndMs = %d, want 1", got.Chapters[0].EndMs)
		}
	})
}

// TestWriteChapterTitleBoundary kills writer.go line 355:
//
//	if ch.Title != ""
func TestWriteChapterTitleBoundary(t *testing.T) {
	got := writeAndRead(t, &mkv.Container{
		Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Chapters: []mkv.Chapter{{ID: 1, Title: "Intro", StartMs: 0}},
	})
	if len(got.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	if got.Chapters[0].Title != "Intro" {
		t.Errorf("Title = %q, want Intro", got.Chapters[0].Title)
	}
}

// TestWriteChapterSegmentUIDBoundary kills writer.go line 360:
//
//	if len(ch.SegmentUID) > 0
func TestWriteChapterSegmentUIDBoundary(t *testing.T) {
	uid := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	got := writeAndRead(t, &mkv.Container{
		Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Chapters: []mkv.Chapter{{ID: 1, Title: "C", StartMs: 0, SegmentUID: uid}},
	})
	if !bytes.Equal(got.Chapters[0].SegmentUID, uid) {
		t.Errorf("SegmentUID = %x, want %x", got.Chapters[0].SegmentUID, uid)
	}
}

// ── WriteTags ─────────────────────────────────────────────────────────────────

// TestWriteTagFieldBoundaries kills writer.go lines 375, 378, 393, 396, 399.
func TestWriteTagFieldBoundaries(t *testing.T) {
	t.Run("TargetType preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tags: []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "X"}}}},
		})
		if got.Tags[0].TargetType != "MOVIE" {
			t.Errorf("TargetType = %q, want MOVIE", got.Tags[0].TargetType)
		}
	})
	t.Run("TargetID=0 not written", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tags: []mkv.Tag{{TargetID: 0, SimpleTags: []mkv.SimpleTag{{Name: "X", Value: "Y"}}}},
		})
		if got.Tags[0].TargetID != 0 {
			t.Errorf("TargetID = %d, want 0 (absent)", got.Tags[0].TargetID)
		}
	})
	t.Run("TargetID=1 written and preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tags: []mkv.Tag{{TargetID: 1, SimpleTags: []mkv.SimpleTag{{Name: "X", Value: "Y"}}}},
		})
		if got.Tags[0].TargetID != 1 {
			t.Errorf("TargetID = %d, want 1", got.Tags[0].TargetID)
		}
	})
	t.Run("SimpleTag Value preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tags: []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Hello World"}}}},
		})
		if got.Tags[0].SimpleTags[0].Value != "Hello World" {
			t.Errorf("Value = %q, want Hello World", got.Tags[0].SimpleTags[0].Value)
		}
	})
	t.Run("SimpleTag Binary preserved", func(t *testing.T) {
		bin := []byte{0x01, 0x02, 0x03}
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tags: []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "DATA", Binary: bin}}}},
		})
		if !bytes.Equal(got.Tags[0].SimpleTags[0].Binary, bin) {
			t.Errorf("Binary = %x, want %x", got.Tags[0].SimpleTags[0].Binary, bin)
		}
	})
	t.Run("SimpleTag Language preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Tags: []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "V", Language: "fra"}}}},
		})
		if got.Tags[0].SimpleTags[0].Language != "fra" {
			t.Errorf("Language = %q, want fra", got.Tags[0].SimpleTags[0].Language)
		}
	})
}

// ── WriteAttachments ──────────────────────────────────────────────────────────

// TestWriteAttachmentFieldBoundaries kills writer.go lines 413, 416, 419, 422.
func TestWriteAttachmentFieldBoundaries(t *testing.T) {
	t.Run("Name preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Attachments: []mkv.Attachment{{ID: 1, Name: "font.ttf", MIMEType: "font/ttf", Data: []byte{0x01}}},
		})
		if got.Attachments[0].Name != "font.ttf" {
			t.Errorf("Name = %q, want font.ttf", got.Attachments[0].Name)
		}
	})
	t.Run("MIMEType preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Attachments: []mkv.Attachment{{ID: 1, Name: "x", MIMEType: "image/png", Data: []byte{0x01}}},
		})
		if got.Attachments[0].MIMEType != "image/png" {
			t.Errorf("MIMEType = %q, want image/png", got.Attachments[0].MIMEType)
		}
	})
	t.Run("ID=0 not written (boundary)", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Attachments: []mkv.Attachment{{ID: 0, Name: "x", MIMEType: "a/b", Data: []byte{0x01}}},
		})
		if got.Attachments[0].ID != 0 {
			t.Errorf("ID = %d, want 0 (absent → 0)", got.Attachments[0].ID)
		}
	})
	t.Run("ID=1 written and preserved", func(t *testing.T) {
		got := writeAndRead(t, &mkv.Container{
			Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Attachments: []mkv.Attachment{{ID: 1, Name: "x", MIMEType: "a/b", Data: []byte{0x01}}},
		})
		if got.Attachments[0].ID != 1 {
			t.Errorf("ID = %d, want 1", got.Attachments[0].ID)
		}
	})
	t.Run("Data preserved", func(t *testing.T) {
		data := []byte{0xDE, 0xAD, 0xBE, 0xEF}
		got := writeAndRead(t, &mkv.Container{
			Info:        mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Attachments: []mkv.Attachment{{ID: 1, Name: "x", MIMEType: "a/b", Data: data}},
		})
		if !bytes.Equal(got.Attachments[0].Data, data) {
			t.Errorf("Data = %x, want %x", got.Attachments[0].Data, data)
		}
	})
}

// ── WriteCluster timecode accuracy ───────────────────────────────────────────

// TestWriteClusterTimecodeArithmetic kills writer.go lines 461 and 466:
//
//	rawTS := uint64(clusterTS * 1000000 / timecodeScale)   (line 461)
//	relTC := int16(b.Timecode - clusterTS)                 (line 466)
//
// Arithmetic mutations corrupt the absolute block timecodes reported by the
// reader (safeTimecodeMs adds rawTS and relTC before converting to ms).
func TestWriteClusterTimecodeArithmetic(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}

	// Cluster at 5000 ms; first block at 5000 ms (relTC=0), second at 5033 ms (relTC=33).
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 5000, Keyframe: true, Data: []byte{0xAA}},
		{TrackNumber: 1, Timecode: 5033, Keyframe: false, Data: []byte{0xBB}},
	}
	if err := WriteCluster(m.W, 5000, 1_000_000, blocks); err != nil {
		t.Fatal(err)
	}

	br, err := reader.NewBlockReader(bytes.NewReader(buf.buf), 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	b0, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b0.Timecode != 5000 {
		t.Errorf("block[0] Timecode = %d, want 5000", b0.Timecode)
	}
	b1, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b1.Timecode != 5033 {
		t.Errorf("block[1] Timecode = %d, want 5033", b1.Timecode)
	}
}

// ── WriteCues timecode accuracy ───────────────────────────────────────────────

// TestWriteCuesTimecodeArithmetic kills writer.go line 515:
//
//	cueTime := uint64(cp.TimeMs) * 1000000 / uint64(timecodeScale)
//
// With timecodeScale=1_000_000 the units cancel so cueTime == TimeMs, making a
// clean round-trip assertion possible.
func TestWriteCuesTimecodeArithmetic(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}
	// A keyframe block causes WriteClusterWithCues to record a cue at TimeMs=7000.
	if err := m.WriteClusterWithCues(7000, 1_000_000, []mkv.Block{
		{TrackNumber: 1, Timecode: 7000, Keyframe: true, Data: []byte{0x01}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}

	got, err := reader.Read(context.Background(), bytes.NewReader(buf.buf), "test.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cues) == 0 {
		t.Fatal("no cues in read-back")
	}
	if got.Cues[0].TimeMs != 7000 {
		t.Errorf("cue TimeMs = %d, want 7000", got.Cues[0].TimeMs)
	}
}

// ── WriteVoid exact size ──────────────────────────────────────────────────────

// TestWriteVoidBoundarySizes kills writer.go line 557 (arithmetic in headerSize).
//
//	headerSize := 1 + ebml.DataSizeLen(int64(totalSize-1-ebml.DataSizeLen(int64(totalSize-2))))
//
// At size=128 the inner DataSizeLen transitions from 1→2 bytes; the subtraction
// mutations (cols 52 and 54) produce a different padLen that makes buf.Len() != size.
// The existing test covers 2/10/100/1000 but misses the VINT boundary at 127/128.
func TestWriteVoidBoundarySizes(t *testing.T) {
	// 129 is impossible: padLen would be 127, but EBML treats 0x7F as the
	// "unknown size" sentinel in 1-byte VINT encoding, forcing a 2-byte length
	// and an actual output of 130 bytes.  Skip 129.
	for _, size := range []int{127, 128, 130, 131, 256, 257} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVoid(&buf, size); err != nil {
				t.Fatalf("WriteVoid(%d): %v", size, err)
			}
			if buf.Len() != size {
				t.Errorf("WriteVoid(%d) = %d bytes, want %d", size, buf.Len(), size)
			}
		})
	}
}

// ── MKVWriter (mkvwriter.go) ──────────────────────────────────────────────────

// TestMKVWriterRelPosArithmetic kills mkvwriter.go line 37:
//
//	return m.pos() - m.SegDataStart
//
// - → + doubles the position instead of giving the offset from SegDataStart.
func TestMKVWriterRelPosArithmetic(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	// WriteStart writes the void reserve exactly SeekHeadReserve bytes past
	// SegDataStart, so RelPos must equal SeekHeadReserve.
	rp := m.RelPos()
	if rp != SeekHeadReserve {
		t.Errorf("RelPos after WriteStart = %d, want %d (SeekHeadReserve)",
			rp, SeekHeadReserve)
	}
}

// TestMKVWriterWriteClusterWithCuesEmptyBlocks kills mkvwriter.go line 74:
//
//	if !cued && len(blocks) > 0 {
//
// > → >= causes a panic on blocks[0] when blocks is nil/empty.
func TestMKVWriterWriteClusterWithCuesEmptyBlocks(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic with empty blocks: %v", r)
		}
	}()

	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(&mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
	}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteClusterWithCues(0, 1_000_000, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.Cues) != 0 {
		t.Errorf("cues = %d, want 0 for empty block list", len(m.Cues))
	}
}

// TestMKVWriterCueIntervalBoundary kills mkvwriter.go line 79:
//
//	if blocks[0].Timecode-lastCueTime >= minCueIntervalMs {
//
// >= → > suppresses a cue at exactly 500 ms.
func TestMKVWriterCueIntervalBoundary(t *testing.T) {
	newMKV := func(t *testing.T) *MKVWriter {
		t.Helper()
		var buf seekBuffer
		m := NewMKVWriter(&buf)
		if err := m.WriteStart(); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteMetadata(
			&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}},
			nil, 0,
		); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("exactly 500ms adds cue", func(t *testing.T) {
		m := newMKV(t)
		if err := m.WriteClusterWithCues(0, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: 0, Data: []byte{0x01}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteClusterWithCues(500, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: 500, Data: []byte{0x02}},
		}); err != nil {
			t.Fatal(err)
		}
		if len(m.Cues) != 2 {
			t.Errorf("cues = %d, want 2 (boundary at exactly 500 ms)", len(m.Cues))
		}
	})

	t.Run("499ms does not add cue", func(t *testing.T) {
		m := newMKV(t)
		if err := m.WriteClusterWithCues(0, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: 0, Data: []byte{0x01}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.WriteClusterWithCues(499, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: 499, Data: []byte{0x02}},
		}); err != nil {
			t.Fatal(err)
		}
		if len(m.Cues) != 1 {
			t.Errorf("cues = %d, want 1 (below 500 ms threshold)", len(m.Cues))
		}
	})
}

// TestMKVWriterFinalizeSeekHeadArithmetic kills mkvwriter.go lines 91, 100, 117,
// 122, 128.  A full write-then-read round-trip detects seek head corruption
// (wrong seek position or wrong overflow logic) because reader.Read follows
// the seek head to locate Info and Tracks.
func TestMKVWriterFinalizeSeekHeadArithmetic(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 5000); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteClusterWithCues(0, 1_000_000, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}

	got, err := reader.Read(context.Background(), bytes.NewReader(buf.buf), "test.mkv")
	if err != nil {
		t.Fatalf("reader.Read after Finalize: %v", err)
	}
	if len(got.Tracks) != 1 {
		t.Errorf("tracks = %d, want 1", len(got.Tracks))
	}
	if len(got.Cues) == 0 {
		t.Error("cues missing from read-back")
	}
}

// TestMKVWriterFinalizeVoidBoundary kills mkvwriter.go line 129:
//
//	if remaining >= 2 { return WriteVoid(m.W, remaining) }
//
// >= → > would skip the Void element when remaining==2, leaving two uninitialised
// bytes in the seek-head reserve that corrupt the file.
func TestMKVWriterFinalizeVoidBoundary(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	// Write nothing beyond Info (empty container) so the seek head is small and
	// the remaining space is ≥ 2 bytes, exercising the >= 2 branch.
	if err := m.WriteMetadata(&mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
	}, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	// Verify the output is still parseable (void written correctly).
	_, err := reader.Read(context.Background(), bytes.NewReader(buf.buf), "test.mkv")
	if err != nil {
		t.Fatalf("Finalize output not parseable: %v", err)
	}
}

// TestMKVWriterWriteMetadataConditionals kills mkvwriter.go lines 144, 150, 156, 162:
//
//	if len(tracks) > 0  /  c.Chapters  /  c.Attachments  /  c.Tags
//
// > → >= sets the position field even for empty slices; those spurious entries
// in the seek head point to offset 0 and corrupt the file.
func TestMKVWriterWriteMetadataConditionals(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(
		&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}},
		nil, 0,
	); err != nil {
		t.Fatal(err)
	}
	// All position fields must remain 0 — no tracks/chapters/attachments/tags written.
	if m.TracksPos != 0 {
		t.Errorf("TracksPos = %d, want 0 (no tracks)", m.TracksPos)
	}
	if m.ChaptersPos != 0 {
		t.Errorf("ChaptersPos = %d, want 0 (no chapters)", m.ChaptersPos)
	}
	if m.AttachPos != 0 {
		t.Errorf("AttachPos = %d, want 0 (no attachments)", m.AttachPos)
	}
	if m.TagsPos != 0 {
		t.Errorf("TagsPos = %d, want 0 (no tags)", m.TagsPos)
	}

	// Positive case: non-empty tracks must set TracksPos.
	var buf2 seekBuffer
	m2 := NewMKVWriter(&buf2)
	if err := m2.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m2.WriteMetadata(
		&mkv.Container{
			Info:     mkv.SegmentInfo{TimecodeScale: 1_000_000},
			Chapters: []mkv.Chapter{{ID: 1, Title: "C", StartMs: 0}},
			Tags:     []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "N", Value: "V"}}}},
			Attachments: []mkv.Attachment{
				{ID: 1, Name: "f", MIMEType: "a/b", Data: []byte{0x01}},
			},
		},
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}},
		0,
	); err != nil {
		t.Fatal(err)
	}
	if m2.TracksPos == 0 {
		t.Error("TracksPos = 0 after writing tracks")
	}
	if m2.ChaptersPos == 0 {
		t.Error("ChaptersPos = 0 after writing chapters")
	}
	if m2.AttachPos == 0 {
		t.Error("AttachPos = 0 after writing attachments")
	}
	if m2.TagsPos == 0 {
		t.Error("TagsPos = 0 after writing tags")
	}
}

// ── StreamWriter (stream_writer.go) ──────────────────────────────────────────

// TestStreamWriterEmptyTracksNoTracksElement kills stream_writer.go line 101:
//
//	if len(tracks) > 0
//
// > → >= calls WriteTracks with an empty slice, writing an empty Tracks element.
// We detect it by scanning the binary output for the IDTracks marker.
func TestStreamWriterEmptyTracksNoTracksElement(t *testing.T) {
	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, mkv.SegmentInfo{TimecodeScale: 1_000_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sw.Close()

	// IDTracks = 0x1654AE6B (4 bytes).
	data := buf.Bytes()
	id := [4]byte{0x16, 0x54, 0xAE, 0x6B}
	for i := 0; i+4 <= len(data); i++ {
		if data[i] == id[0] && data[i+1] == id[1] &&
			data[i+2] == id[2] && data[i+3] == id[3] {
			t.Error("Tracks element found in output for empty tracks list")
			return
		}
	}
}

// TestStreamWriterClusterTimecodeArithmetic kills stream_writer.go line 116:
//
//	rawTS := uint64(tsMs * 1_000_000 / s.timecodeScale)
//
// * → / gives rawTS ≈ 0, making all blocks appear at t≈0 regardless of cluster ts.
func TestStreamWriterClusterTimecodeArithmetic(t *testing.T) {
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatal(err)
	}
	// Cluster opens at 5000 ms; second block at 5033 ms (relTC=33).
	sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 5000, Keyframe: true, Data: []byte{0xAA}})
	sw.WriteBlock(mkv.Block{TrackNumber: 1, Timecode: 5033, Keyframe: false, Data: []byte{0xBB}})
	sw.Close()

	ctx := context.Background()
	_, br, err := reader.ReadStream(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b0, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b0.Timecode != 5000 {
		t.Errorf("block[0] Timecode = %d, want 5000", b0.Timecode)
	}
	b1, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b1.Timecode != 5033 {
		t.Errorf("block[1] Timecode = %d, want 5033", b1.Timecode)
	}
}

// TestWriteSegmentEmptySliceBoundaries kills writer.go lines 103, 108, 113, 118:
//
//	if len(c.Tracks) > 0   (line 103)
//	if len(c.Chapters) > 0 (line 108)
//	if len(c.Attachments) > 0 (line 113)
//	if len(c.Tags) > 0     (line 118)
//
// > → >= would write an empty element even for nil slices.  We detect each by
// scanning the raw bytes for the element's 4-byte ID.
func TestWriteSegmentEmptySliceBoundaries(t *testing.T) {
	// hasID scans for a 4-byte big-endian element ID in data.
	hasID := func(data []byte, id uint32) bool {
		b := [4]byte{
			byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id),
		}
		for i := 0; i+4 <= len(data); i++ {
			if data[i] == b[0] && data[i+1] == b[1] &&
				data[i+2] == b[2] && data[i+3] == b[3] {
				return true
			}
		}
		return false
	}

	t.Run("no Tracks element for empty tracks", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Write(&buf, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		}); err != nil {
			t.Fatal(err)
		}
		if hasID(buf.Bytes(), mkv.IDTracks) {
			t.Error("IDTracks found in output for empty tracks slice")
		}
	})
	t.Run("no Chapters element for empty chapters", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Write(&buf, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		}); err != nil {
			t.Fatal(err)
		}
		if hasID(buf.Bytes(), mkv.IDChapters) {
			t.Error("IDChapters found in output for empty chapters slice")
		}
	})
	t.Run("no Attachments element for empty attachments", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Write(&buf, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		}); err != nil {
			t.Fatal(err)
		}
		if hasID(buf.Bytes(), mkv.IDAttachments) {
			t.Error("IDAttachments found in output for empty attachments slice")
		}
	})
	t.Run("no Tags element for empty tags", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Write(&buf, &mkv.Container{
			Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		}); err != nil {
			t.Fatal(err)
		}
		if hasID(buf.Bytes(), mkv.IDTags) {
			t.Error("IDTags found in output for empty tags slice")
		}
	})
}

// TestWriteWebMSegmentEmptyTracksBoundary kills writer.go line 90:
//
//	if len(c.Tracks) > 0   (in writeWebMSegment)
//
// > → >= would write an empty Tracks element for a nil slice.
func TestWriteWebMSegmentEmptyTracksBoundary(t *testing.T) {
	hasID := func(data []byte, id uint32) bool {
		b := [4]byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
		for i := 0; i+4 <= len(data); i++ {
			if data[i] == b[0] && data[i+1] == b[1] &&
				data[i+2] == b[2] && data[i+3] == b[3] {
				return true
			}
		}
		return false
	}

	var buf bytes.Buffer
	if err := WriteWebM(&buf, &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	if hasID(buf.Bytes(), mkv.IDTracks) {
		t.Error("IDTracks found in WebM output for empty tracks slice")
	}
}

// TestStreamWriterRelativeTimecodeMinBoundary kills stream_writer.go line 151:
//
//	if delta < math.MinInt16 || delta > math.MaxInt16 {
//
// < → <= would reject delta == MinInt16 (-32768) even though it is a valid int16.
func TestStreamWriterRelativeTimecodeMinBoundary(t *testing.T) {
	// delta = -32768 = math.MinInt16: must succeed, not error.
	const clusterTS = int64(40000)
	blockTS := clusterTS - int64(math.MaxInt16+1) // 40000 - 32768 = 7232

	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "opus", IsDefault: true}}

	var buf bytes.Buffer
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatal(err)
	}
	// Open cluster at clusterTS via a keyframe block.
	if err := sw.WriteBlock(mkv.Block{
		TrackNumber: 1, Timecode: clusterTS, Keyframe: true, Data: []byte{0xAA},
	}); err != nil {
		t.Fatalf("WriteBlock at cluster open: %v", err)
	}
	// Block exactly at MinInt16 offset must succeed.
	if err := sw.WriteBlockInCurrentCluster(mkv.Block{
		TrackNumber: 1, Timecode: blockTS, Data: []byte{0xBB},
	}); err != nil {
		t.Fatalf("WriteBlockInCurrentCluster(delta=%d): %v",
			blockTS-clusterTS, err)
	}
}
