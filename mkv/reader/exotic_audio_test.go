package reader

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// float32Elem writes a 4-byte float element from its raw IEEE bits.
func float32Elem(id uint32, bits uint32) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, 4)
	b.Write([]byte{byte(bits >> 24), byte(bits >> 16), byte(bits >> 8), byte(bits)})
	return b.Bytes()
}

// audioFixture is a Segment with one audio TrackEntry of the given codec and Audio/ContentEncodings children.
func audioFixture(codec string, audio, extra []byte) []byte {
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, codec),
		audio, extra)
	var seg bytes.Buffer
	seg.Write(infoElem())
	seg.Write(masterElem(mkv.IDTracks, te))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

// readBoth parses the fixture with the seekable and the streaming reader.
func readBoth(t *testing.T, data []byte) [2]mkv.Track {
	t.Helper()
	c, err := Read(context.Background(), bytes.NewReader(data), "a.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	cs, _, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	return [2]mkv.Track{c.Tracks[0], cs.Tracks[0]}
}

// TestAudioSpecDefaults: Channels and SamplingFrequency left out read as the
// spec defaults, 1 and 8000 Hz (48000 for Opus), on both readers.
func TestAudioSpecDefaults(t *testing.T) {
	for _, tc := range []struct {
		codec string
		audio []byte
		ch    uint8
		rate  float64
	}{
		{"A_AC3", masterElem(mkv.IDAudio, uintElem(mkv.IDBitDepth, 16, 1)), 1, 8000},
		{"A_VORBIS", nil, 1, 8000},
		{"A_OPUS", masterElem(mkv.IDAudio, uintElem(mkv.IDChannels, 2, 1)), 2, 48000},
	} {
		for i, tr := range readBoth(t, audioFixture(tc.codec, tc.audio, nil)) {
			if tr.Channels == nil || *tr.Channels != tc.ch {
				t.Errorf("%s reader %d: Channels = %v, want %d", tc.codec, i, tr.Channels, tc.ch)
			}
			if tr.SampleRate == nil || *tr.SampleRate != tc.rate {
				t.Errorf("%s reader %d: SampleRate = %v, want %v", tc.codec, i, tr.SampleRate, tc.rate)
			}
		}
	}
}

// TestAbsurdSampleRateRejected: a SamplingFrequency no file can mean (a
// corrupt float, 1.4e-37 Hz) is not reported, and not replaced by the default.
func TestAbsurdSampleRateRejected(t *testing.T) {
	bad := float32Elem(mkv.IDSamplingFreq, math.Float32bits(1.4e-37))
	badOut := float32Elem(mkv.IDOutputSamplingFreq, math.Float32bits(float32(math.Inf(1))))
	audio := masterElem(mkv.IDAudio, bad, badOut, uintElem(mkv.IDChannels, 2, 1))
	for i, tr := range readBoth(t, audioFixture("A_VORBIS", audio, nil)) {
		if tr.SampleRate != nil {
			t.Errorf("reader %d: SampleRate = %v, want nil (rejected)", i, *tr.SampleRate)
		}
		if tr.OutputSampleRate != nil {
			t.Errorf("reader %d: OutputSampleRate = %v, want nil (rejected)", i, *tr.OutputSampleRate)
		}
		if tr.Channels == nil || *tr.Channels != 2 {
			t.Errorf("reader %d: Channels = %v, want 2", i, tr.Channels)
		}
	}
}

// TestContentEncryptionNamed: a ContentEncoding of type 1 names the track's
// encryption from ContentEncAlgo and the AES cipher mode, on both readers.
func TestContentEncryptionNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		enc  []byte
		want string
	}{
		{"aes-ctr", masterElem(mkv.IDContentEncoding,
			uintElem(mkv.IDContentEncodingType, 1, 1),
			masterElem(mkv.IDContentEncryption,
				uintElem(mkv.IDContentEncAlgo, 5, 1),
				masterElem(mkv.IDContentEncAESSettings, uintElem(mkv.IDAESSettingsCipherMode, 1, 1)))), "AES-CTR"},
		{"des", masterElem(mkv.IDContentEncoding,
			masterElem(mkv.IDContentEncryption, uintElem(mkv.IDContentEncAlgo, 1, 1))), "DES"},
		{"type only", masterElem(mkv.IDContentEncoding, uintElem(mkv.IDContentEncodingType, 1, 1)), "unspecified"},
		{"compression", masterElem(mkv.IDContentEncoding,
			masterElem(mkv.IDContentCompression, uintElem(mkv.IDContentCompAlgo, 0, 1))), ""},
	} {
		extra := masterElem(mkv.IDContentEncodings, tc.enc)
		for i, tr := range readBoth(t, audioFixture("A_OPUS", nil, extra)) {
			if tr.Encryption != tc.want {
				t.Errorf("%s reader %d: Encryption = %q, want %q", tc.name, i, tr.Encryption, tc.want)
			}
		}
	}
}

// TestAudioHeadersFillAbsentFields: Vorbis, FLAC and Opus headers give the
// channels and rate the container left out, before the spec defaults apply.
func TestAudioHeadersFillAbsentFields(t *testing.T) {
	vorbisID := append([]byte{1}, "vorbis\x00\x00\x00\x00\x02\x80\xbb\x00\x00"...) // version 0, 2 channels, 48000 Hz
	vorbis := append([]byte{2, byte(len(vorbisID)), 1}, vorbisID...)               // Xiph lacing: 3 packets
	vorbis = append(vorbis, 3, 5)
	flac := []byte("fLaC\x80\x00\x00\x22" + "\x10\x00\x10\x00\x00\x00\x00\x00\x00\x00" + "\x0a\xc4\x42\xf0" + "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00") // 44100 Hz, 2 ch, 16 bits
	opus := []byte("OpusHead\x01\x06\x38\x01\x80\xbb\x00\x00\x00\x00\x00")                                                                                                                        // 6 channels
	for _, tc := range []struct {
		codec string
		cp    []byte
		ch    uint8
		rate  float64
		bits  uint8
	}{
		{"A_VORBIS", vorbis, 2, 48000, 0},
		{"A_FLAC", flac, 2, 44100, 16},
		{"A_OPUS", opus, 6, 48000, 0},
	} {
		var cpEl bytes.Buffer
		ebml.WriteElementHeader(&cpEl, mkv.IDCodecPrivate, int64(len(tc.cp)))
		cpEl.Write(tc.cp)
		for i, tr := range readBoth(t, audioFixture(tc.codec, nil, cpEl.Bytes())) {
			if tr.Channels == nil || *tr.Channels != tc.ch || tr.SampleRate == nil || *tr.SampleRate != tc.rate {
				t.Errorf("%s reader %d: channels %v rate %v, want %d %v", tc.codec, i, tr.Channels, tr.SampleRate, tc.ch, tc.rate)
			}
			if tc.bits > 0 && (tr.BitDepth == nil || *tr.BitDepth != tc.bits) {
				t.Errorf("%s reader %d: bit depth %v, want %d", tc.codec, i, tr.BitDepth, tc.bits)
			}
		}
	}
}

// TestUnknownSizeCuesTolerated: a live muxer's Cues and CuePoints declare no
// size; the walk resumes on the next segment-level element or ends, and the
// file probes on both readers.
func TestUnknownSizeCuesTolerated(t *testing.T) {
	data := liveCuesMKV()
	c, err := Read(context.Background(), bytes.NewReader(data), "live.webm")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "opus" {
		t.Errorf("tracks = %+v, want the Opus track", c.Tracks)
	}
	m, err := ReadMeta(context.Background(), bytes.NewReader(data), "live.webm")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(m.Tracks) != 1 {
		t.Errorf("ReadMeta tracks = %d, want 1", len(m.Tracks))
	}
}

// liveCuesMKV is a file as a live muxer leaves it: Tags, Cues and CuePoints of unknown size.
func liveCuesMKV() []byte {
	unknown := func(id uint32, body []byte) []byte {
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, id, -1)
		b.Write(body)
		return b.Bytes()
	}
	cuePoint := unknown(mkv.IDCuePoint, append(uintElem(mkv.IDCueTime, 0, 1),
		unknown(mkv.IDCueTrackPositions, append(uintElem(mkv.IDCueTrack, 1, 1), uintElem(mkv.IDCueClusterPos, 100, 1)...))...))
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_OPUS"))
	var seg bytes.Buffer
	seg.Write(infoElem())
	seg.Write(unknown(mkv.IDTags, masterElem(mkv.IDTag, masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "TITLE")))))
	seg.Write(masterElem(mkv.IDTracks, te))
	seg.Write(realCluster())
	seg.Write(unknown(mkv.IDCues, cuePoint))
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}
