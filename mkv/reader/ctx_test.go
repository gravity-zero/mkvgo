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

// TestInfoStringsChargedAgainstBudget checks that the Info strings/UIDs count
// against the cumulative metadata budget (they previously bypassed it, capped only
// at 512 MB per element).
func TestInfoStringsChargedAgainstBudget(t *testing.T) {
	body := strElem(mkv.IDTitle, "0123456789abcdef0123456789") // 26-byte title value
	tiny := &parser{r: bytes.NewReader(body), metaBudget: 10}
	if err := tiny.parseInfo(int64(len(body)), &mkv.Container{}); err == nil {
		t.Error("a Title larger than the remaining budget must error")
	}
	ample := &parser{r: bytes.NewReader(body), metaBudget: maxMetadataBytes}
	if err := ample.parseInfo(int64(len(body)), &mkv.Container{}); err != nil {
		t.Errorf("within budget should parse: %v", err)
	}
}

// TestSkipRejectsUnknownSize checks the skip guard: an unknown size (-1) errors
// rather than seeking a byte backwards and desyncing the framing.
func TestSkipRejectsUnknownSize(t *testing.T) {
	p := &parser{r: bytes.NewReader(make([]byte, 16))}
	if err := p.skip(-1); err == nil {
		t.Error("skip(-1) must error, not seek backwards")
	}
	if err := p.skip(4); err != nil {
		t.Errorf("skip(4) should advance fine, got %v", err)
	}
}
