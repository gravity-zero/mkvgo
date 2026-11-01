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

// dispositionFlags names the five disposition flags: the element each one is, and the Track field it is read into.
var dispositionFlags = []struct {
	name  string
	id    uint32
	field func(*mkv.Track) *bool
}{
	{"HearingImpaired", mkv.IDFlagHearingImpaired, func(t *mkv.Track) *bool { return &t.HearingImpaired }},
	{"VisualImpaired", mkv.IDFlagVisualImpaired, func(t *mkv.Track) *bool { return &t.VisualImpaired }},
	{"TextDescriptions", mkv.IDFlagTextDescriptions, func(t *mkv.Track) *bool { return &t.TextDescriptions }},
	{"Original", mkv.IDFlagOriginal, func(t *mkv.Track) *bool { return &t.Original }},
	{"Commentary", mkv.IDFlagCommentary, func(t *mkv.Track) *bool { return &t.Commentary }},
}

// flaggedTrackEntry is an AAC TrackEntry carrying the one flag set to 1.
func flaggedTrackEntry(id uint32) []byte {
	return trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
		uintElem(id, 1, 1),
	)
}

// flagElements reports whether the written tracks carry the flag's element at all, as 0 or as 1.
func flagElements(tracks []byte, id uint32) (asZero, asOne bool) {
	return bytes.Contains(tracks, uintElem(id, 0, 1)), bytes.Contains(tracks, uintElem(id, 1, 1))
}

// TestDispositionFlags pins, for each of the five disposition flags, that the Track's field is what the writer writes:
// set through the API it is written; cleared on a source that sets it, it is absent from the rewrite; set by the
// source, it is read into its field (not kept as an unmodeled child) and comes back from a rewrite.
func TestDispositionFlags(t *testing.T) {
	ctx := context.Background()
	readTracks := func(t *testing.T, tracks []byte) mkv.Track {
		c, err := Read(ctx, bytes.NewReader(segmentMKV(infoElem(), tracks, clusterElem())), "x.mkv")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return c.Tracks[0]
	}
	for _, fl := range dispositionFlags {
		t.Run(fl.name+"/set-by-the-API-is-written", func(t *testing.T) {
			tr := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "aac", IsDefault: true}
			*fl.field(&tr) = true
			var out bytes.Buffer
			if err := writer.WriteTracks(&out, []mkv.Track{tr}); err != nil {
				t.Fatal(err)
			}
			if _, asOne := flagElements(out.Bytes(), fl.id); !asOne {
				t.Errorf("flag 0x%X set on the Track is not written", fl.id)
			}
			for _, other := range dispositionFlags {
				if asZero, asOne := flagElements(out.Bytes(), other.id); other.id != fl.id && (asZero || asOne) {
					t.Errorf("flag 0x%X, not set on the Track, is written", other.id)
				}
			}
			if back := readTracks(t, out.Bytes()); !*fl.field(&back) {
				t.Errorf("%s written, read back false", fl.name)
			}
		})
		t.Run(fl.name+"/cleared-on-a-source-that-sets-it-is-absent", func(t *testing.T) {
			tr := readTracks(t, masterElem(mkv.IDTracks, flaggedTrackEntry(fl.id)))
			if !*fl.field(&tr) {
				t.Fatalf("%s set by the source, read false", fl.name)
			}
			*fl.field(&tr) = false
			var out bytes.Buffer
			if err := writer.WriteTracks(&out, []mkv.Track{tr}); err != nil {
				t.Fatal(err)
			}
			if asZero, asOne := flagElements(out.Bytes(), fl.id); asZero || asOne {
				t.Errorf("flag 0x%X cleared on the Track is written (as 0: %v, as 1: %v)", fl.id, asZero, asOne)
			}
			if back := readTracks(t, out.Bytes()); *fl.field(&back) {
				t.Errorf("%s cleared, read back true", fl.name)
			}
		})
		t.Run(fl.name+"/set-by-the-source-is-kept-by-a-rewrite", func(t *testing.T) {
			tr := readTracks(t, masterElem(mkv.IDTracks, flaggedTrackEntry(fl.id)))
			if !*fl.field(&tr) {
				t.Fatalf("%s set by the source, read false", fl.name)
			}
			for _, r := range tr.Unmodeled {
				if r.ID == fl.id {
					t.Errorf("flag 0x%X kept as an unmodeled child: the field is what counts", fl.id)
				}
			}
			var out bytes.Buffer
			if err := writer.WriteTracks(&out, []mkv.Track{tr}); err != nil {
				t.Fatal(err)
			}
			back := readTracks(t, out.Bytes())
			if !*fl.field(&back) {
				t.Errorf("%s set by the source, false after a rewrite", fl.name)
			}
			for _, other := range dispositionFlags {
				if other.id != fl.id && *other.field(&back) {
					t.Errorf("%s, never set, true after a rewrite", other.name)
				}
			}
		})
	}
}
