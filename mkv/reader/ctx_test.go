package reader

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestInnerLoopHonorsCtx checks that the inner element loops stop on a cancelled
// context, not only the top-level Segment walk: parseInfo/parseTracks return the
// cancellation error on the first iteration.
func TestInnerLoopHonorsCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	infoBody := bytes.Join([][]byte{
		uintElem(mkv.IDTimecodeScale, 1000000, 4),
		strElem(mkv.IDTitle, "x"),
	}, nil)
	p := &parser{r: bytes.NewReader(infoBody), metaBudget: maxMetadataBytes, ctx: ctx}
	if err := p.parseInfo(int64(len(infoBody)), &mkv.Container{}); !errors.Is(err, context.Canceled) {
		t.Errorf("parseInfo with cancelled ctx = %v, want context.Canceled", err)
	}

	tracksBody := masterElem(mkv.IDTrackEntry, uintElem(mkv.IDTrackNumber, 1, 1))
	p2 := &parser{r: bytes.NewReader(tracksBody), metaBudget: maxMetadataBytes, ctx: ctx}
	if err := p2.parseTracks(int64(len(tracksBody)), &mkv.Container{}); !errors.Is(err, context.Canceled) {
		t.Errorf("parseTracks with cancelled ctx = %v, want context.Canceled", err)
	}

	// A nil ctx (defensive) must not panic and must parse normally.
	p3 := &parser{r: bytes.NewReader(infoBody), metaBudget: maxMetadataBytes}
	if err := p3.parseInfo(int64(len(infoBody)), &mkv.Container{}); err != nil {
		t.Errorf("nil ctx should parse fine, got %v", err)
	}
}
