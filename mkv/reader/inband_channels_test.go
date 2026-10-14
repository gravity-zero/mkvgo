package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// audioEntry is an audio TrackEntry declaring channels in the container.
func audioEntry(num uint64, codecID string, channels uint64, cp []byte) []byte {
	kids := [][]byte{uintElem(mkv.IDTrackNumber, num, 1), uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1), strElem(mkv.IDCodecID, codecID),
		masterElem(mkv.IDAudio, floatElem(mkv.IDSamplingFreq, 48000), uintElem(mkv.IDChannels, channels, 1))}
	if cp != nil {
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, mkv.IDCodecPrivate, int64(len(cp)))
		b.Write(cp)
		kids = append(kids, b.Bytes())
	}
	return trackEntry(kids...)
}

// TestInBandChannels: WithInBandChannels reads the channel count the stream
// states (the first AC-3 or E-AC-3 frame header, the AAC AudioSpecificConfig),
// keeps it apart and flags a container count it contradicts.
func TestInBandChannels(t *testing.T) {
	ac3Stereo := []byte{0x0B, 0x77, 0, 0, 0x14, 0x40, 0x40, 0, 0, 0} // bsid 8, acmod 2/0, no LFE
	eac3Surround := []byte{0x0B, 0x77, 0, 0xFF, 0x3F, 0x80, 0, 0, 0} // fscod 0, numblkscod 3, acmod 3/2, LFE, bsid 16
	block := func(track byte, data []byte) []byte {
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, mkv.IDSimpleBlock, int64(4+len(data)))
		b.Write([]byte{0x80 | track, 0, 0, 0x80})
		b.Write(data)
		return b.Bytes()
	}
	tracks := masterElem(mkv.IDTracks,
		audioEntry(1, "A_AC3", 6, nil),
		audioEntry(2, "A_EAC3", 6, nil),
		audioEntry(3, "A_AAC", 6, []byte{0x11, 0x90}), // AAC-LC, 48 kHz, channelConfiguration 2
	)
	data := segmentMKV(infoElem(), tracks, masterElem(mkv.IDCluster, uintElem(mkv.IDTimestamp, 0, 1), block(1, ac3Stereo), block(2, eac3Surround)))
	c, err := Read(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandChannels())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		ch       uint8
		mismatch bool
	}{{2, true}, {6, false}, {2, true}} {
		tr := c.Tracks[i]
		if tr.StreamChannels == nil || *tr.StreamChannels != want.ch || tr.ChannelsMismatch != want.mismatch {
			t.Errorf("track %d (%s): stream channels %v, mismatch %v; want %d, %v", tr.ID, tr.Codec, tr.StreamChannels, tr.ChannelsMismatch, want.ch, want.mismatch)
		}
	}
	if c, _ := Read(context.Background(), bytes.NewReader(data), "x.mkv"); c.Tracks[0].StreamChannels != nil {
		t.Error("without the option the stream is not read")
	}
}
