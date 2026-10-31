package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestRewriteKeepsTheTrackChildrenItDoesNotModel pins that a TrackEntry's, a Video's and an Audio's children the
// writer has no field for (FlagLacing, MaxBlockAdditionID, StereoMode 0, DisplayUnit, the Video FrameRate, Emphasis) come
// back from a read and a rewrite as they were.
func TestRewriteKeepsTheTrackChildrenItDoesNotModel(t *testing.T) {
	el := func(id uint32, body []byte) []byte {
		var b bytes.Buffer
		_, _ = ebml.WriteElementHeader(&b, id, int64(len(body)))
		b.Write(body)
		return b.Bytes()
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	video := el(mkv.IDTrackEntry, cat(el(mkv.IDTrackNumber, []byte{1}), el(mkv.IDTrackType, []byte{1}), el(mkv.IDCodecID, []byte("V_VP9")),
		el(0x9C, []byte{0}), el(0x55EE, []byte{1}),
		el(mkv.IDVideo, cat(el(mkv.IDPixelWidth, []byte{64}), el(mkv.IDPixelHeight, []byte{64}), el(mkv.IDStereoMode, []byte{0}), el(0x54B2, []byte{3}), el(0x2383E3, []byte{0x41, 0xEF, 0xC2, 0x91})))))
	audio := el(mkv.IDTrackEntry, cat(el(mkv.IDTrackNumber, []byte{2}), el(mkv.IDTrackType, []byte{2}), el(mkv.IDCodecID, []byte("A_OPUS")),
		el(mkv.IDAudio, cat(el(mkv.IDChannels, []byte{2}), el(0x52F1, []byte{1})))))
	src := el(mkv.IDTracks, cat(video, audio))
	p := &parser{r: bytes.NewReader(src), segEnd: -1, metaBudget: 1 << 20}
	h, _, err := p.readHeader()
	if err != nil {
		t.Fatal(err)
	}
	var c mkv.Container
	if err := p.parseTracks(h.Size, &c); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writer.WriteTracks(&out, c.Tracks); err != nil {
		t.Fatal(err)
	}
	for _, child := range [][]byte{el(0x9C, []byte{0}), el(0x55EE, []byte{1}), el(mkv.IDStereoMode, []byte{0}), el(0x54B2, []byte{3}),
		el(0x2383E3, []byte{0x41, 0xEF, 0xC2, 0x91}), el(0x52F1, []byte{1})} {
		if !bytes.Contains(out.Bytes(), child) {
			t.Errorf("the rewrite lost % X", child)
		}
	}
}

// TestDispositionsAreWrittenFromTheTrackFields pins that the five disposition flags are the Track's fields at write
// time: one a source sets is read into its field, not kept as an unmodeled child, and comes back from a rewrite; one
// the caller sets is written; one the caller clears is left out, whatever the source said.
func TestDispositionsAreWrittenFromTheTrackFields(t *testing.T) {
	flags := []uint32{mkv.IDFlagHearingImpaired, mkv.IDFlagVisualImpaired, mkv.IDFlagTextDescriptions, mkv.IDFlagOriginal, mkv.IDFlagCommentary}
	track := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
		uintElem(mkv.IDFlagCommentary, 1, 1),
		uintElem(mkv.IDFlagOriginal, 1, 1),
	)
	c, err := Read(context.Background(), bytes.NewReader(segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), clusterElem())), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, r := range c.Tracks[0].Unmodeled {
		for _, id := range flags {
			if r.ID == id {
				t.Errorf("flag 0x%X kept as an unmodeled child: the field is what counts", id)
			}
		}
	}

	// A source flag is kept by a rewrite, the one cleared by the caller is left out.
	c.Tracks[0].Original = false
	var out bytes.Buffer
	if err := writer.WriteTracks(&out, c.Tracks); err != nil {
		t.Fatal(err)
	}
	again, err := Read(context.Background(), bytes.NewReader(segmentMKV(infoElem(), out.Bytes(), clusterElem())), "y.mkv")
	if err != nil {
		t.Fatalf("Read after rewrite: %v", err)
	}
	if tr := again.Tracks[0]; !tr.Commentary || tr.Original || tr.HearingImpaired || tr.VisualImpaired || tr.TextDescriptions {
		t.Errorf("after rewrite: commentary=%v original=%v hearing=%v visual=%v descriptions=%v, want commentary alone",
			tr.Commentary, tr.Original, tr.HearingImpaired, tr.VisualImpaired, tr.TextDescriptions)
	}
	if bytes.Contains(out.Bytes(), uintElem(mkv.IDFlagOriginal, 1, 1)) || bytes.Contains(out.Bytes(), uintElem(mkv.IDFlagOriginal, 0, 1)) {
		t.Error("a cleared flag is written")
	}

	// A flag set through the API is written.
	out.Reset()
	set := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "aac", IsDefault: true, HearingImpaired: true, VisualImpaired: true, TextDescriptions: true, Original: true, Commentary: true}
	if err := writer.WriteTracks(&out, []mkv.Track{set}); err != nil {
		t.Fatal(err)
	}
	for _, id := range flags {
		if !bytes.Contains(out.Bytes(), uintElem(id, 1, 1)) {
			t.Errorf("flag 0x%X set on the Track is not written", id)
		}
	}
	out.Reset()
	if err := writer.WriteTracks(&out, []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "aac", IsDefault: true}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range flags {
		if bytes.Contains(out.Bytes(), uintElem(id, 0, 1)) || bytes.Contains(out.Bytes(), uintElem(id, 1, 1)) {
			t.Errorf("flag 0x%X is written on a Track that does not set it", id)
		}
	}
}
