package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func emptyEl(id uint32) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, 0)
	return b.Bytes()
}

// TestEmptyElementsTakeTheirDefault: an element of size 0 with a default in RFC 9559 reads as that default (RFC 8794
// section 6.1), through Read and ReadStream alike: FlagDefault 1, Channels 1, SamplingFrequency 8000, TimestampScale
// 1000000, Language "eng".
func TestEmptyElementsTakeTheirDefault(t *testing.T) {
	var audio bytes.Buffer
	audio.Write(emptyEl(mkv.IDSamplingFreq))
	audio.Write(emptyEl(mkv.IDChannels))
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeAudio))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "A_AAC")
	te.Write(emptyEl(mkv.IDFlagDefault))
	te.Write(emptyEl(mkv.IDLanguage))
	te.Write(wrapEl(mkv.IDAudio, audio.Bytes()))
	var seg bytes.Buffer
	seg.Write(wrapEl(mkv.IDInfo, emptyEl(mkv.IDTimecodeScale)))
	seg.Write(wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes())))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	cs, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	cst, _, err := ReadStream(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*mkv.Container{"Read": cs, "ReadStream": cst} {
		if c.Info.TimecodeScale != 1000000 {
			t.Errorf("%s: TimestampScale %d, want 1000000", name, c.Info.TimecodeScale)
		}
		if len(c.Tracks) != 1 {
			t.Fatalf("%s: %d tracks", name, len(c.Tracks))
		}
		tr := c.Tracks[0]
		if !tr.IsDefault || tr.Language != "eng" || tr.Channels == nil || *tr.Channels != 1 || tr.SampleRate == nil || *tr.SampleRate != 8000 {
			t.Errorf("%s: default %v language %q channels %v rate %v; want true, eng, 1, 8000", name, tr.IsDefault, tr.Language, tr.Channels, tr.SampleRate)
		}
	}
}

// TestEmptyDisplaySizeIsThePixelSize: an empty DisplayWidth or DisplayHeight defaults to the pixel size, square
// pixels, which the model says with nil; it was read as a display size of 0.
func TestEmptyDisplaySizeIsThePixelSize(t *testing.T) {
	video := wrapEl(mkv.IDVideo, append(append(append(uintEl(mkv.IDPixelWidth, 200), uintEl(mkv.IDPixelHeight, 100)...),
		emptyEl(mkv.IDDisplayWidth)...), emptyEl(mkv.IDDisplayHeight)...))
	var te bytes.Buffer
	te.Write(uintEl(mkv.IDTrackNumber, 1))
	te.Write(uintEl(mkv.IDTrackType, mkv.TrackTypeVideo))
	ebml.WriteElementHeader(&te, mkv.IDCodecID, 5)
	ebml.WriteString(&te, "V_VP9")
	te.Write(video)
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	seg.Write(wrapEl(mkv.IDTracks, wrapEl(mkv.IDTrackEntry, te.Bytes())))
	seg.Write(realCluster())
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	c, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Tracks[0]; tr.DisplayWidth != nil || tr.DisplayHeight != nil {
		t.Errorf("display %v x %v, want nil (the 200x100 pixel size)", tr.DisplayWidth, tr.DisplayHeight)
	}
}
