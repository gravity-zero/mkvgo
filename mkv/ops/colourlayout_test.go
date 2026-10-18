package ops

import (
	"context"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestEditInPlaceKeepsTheColourLayout pins that a rewrite of the Tracks keeps the Colour element's sample layout
// (bits per channel, chroma and Cb subsampling, chroma siting) as the source states it.
func TestEditInPlaceKeepsTheColourLayout(t *testing.T) {
	u := func(v uint16) *uint16 { return &v }
	layout := &mkv.ColourLayout{BitsPerChannel: u(10), ChromaSubsamplingHorz: u(1), ChromaSubsamplingVert: u(1), CbSubsamplingHorz: u(1), CbSubsamplingVert: u(1), ChromaSitingHorz: u(1), ChromaSitingVert: u(2)}
	v := videoTrack(1)
	v.ColourLayout = layout
	path := buildMinimalMKV(t, t.TempDir(), "c.mkv", []mkv.Track{v}, []mkv.Block{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{1}}}, 1000)
	mustNil(t, EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "t" }))
	c, err := reader.Open(context.Background(), path)
	mustNil(t, err)
	if got := c.Tracks[0].ColourLayout; !reflect.DeepEqual(got, layout) {
		t.Errorf("colour layout %+v, want %+v", got, layout)
	}
}
