package reader

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestReadStreamDescribesWhatReadDoes: ReadStream decoded each element with a copy of the seekable decoders that
// had fallen behind them - an Opus track lost its CodecDelay and SeekPreRoll, a video its HDR and stereo mode, a
// track its role flags, the Info its date. Both readers describe a richly described file the same now.
func TestReadStreamDescribesWhatReadDoes(t *testing.T) {
	w, h := uint32(1920), uint32(1080)
	ch, rate := uint8(2), 48000.0
	stereo := uint16(1)
	date := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, Title: "t", MuxingApp: "m", WritingApp: "w", DateUTC: &date},
		Tracks: []mkv.Track{
			{ID: 1, UID: 11, Type: mkv.VideoTrack, Codec: "hevc", Width: &w, Height: &h, StereoMode: &stereo,
				HDR: &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400, MasteringDisplay: &mkv.MasteringDisplay{
					RedX: 0.68, RedY: 0.32, GreenX: 0.265, GreenY: 0.69, BlueX: 0.15, BlueY: 0.06, WhiteX: 0.3127, WhiteY: 0.329}}},
			{ID: 2, UID: 12, Type: mkv.AudioTrack, Codec: "opus", Channels: &ch, SampleRate: &rate, Language: "fra",
				CodecDelay: 6500000, SeekPreRoll: 80000000, Original: true, Commentary: true, HearingImpaired: true},
		},
		Chapters: []mkv.Chapter{{ID: 1, Title: "one", StartMs: 0, EndMs: 1000}},
	}
	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	cs, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if cs.Tracks[1].CodecDelay != 6500000 {
		t.Fatalf("Read: CodecDelay %d, the fixture does not carry it", cs.Tracks[1].CodecDelay)
	}
	if got, want := comparable(cst), comparable(cs); got != want {
		t.Errorf("ReadStream and Read differ:\nstream: %s\nread:   %s", got, want)
	}
}
