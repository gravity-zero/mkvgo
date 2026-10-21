package mp4

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
)

// splitOK checks WalkReadBytes + CopyReadBytes = SourceReadBytes and returns the two.
func splitOK(t *testing.T, label string, p *HLSPlan) (walk, cp int64) {
	t.Helper()
	st := p.Stats()
	if st.WalkReadBytes+st.CopyReadBytes != st.SourceReadBytes {
		t.Errorf("%s: walk %d + copy %d != source %d", label, st.WalkReadBytes, st.CopyReadBytes, st.SourceReadBytes)
	}
	return st.WalkReadBytes, st.CopyReadBytes
}

// writeOpen serves a resource of p through Open, as a server does.
func writeOpen(t *testing.T, p *HLSPlan, name string) {
	t.Helper()
	ctx := context.Background()
	h, err := p.Open(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.WriteTo(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// Every read is the walk's or the copy's, on every path: a Matroska window's
// walk reads, its segments' copies read (none when the walk kept them); an
// MP4 source has no walk; an MP4 segment built whole, an encrypted segment and
// a remote MP4 window's shared read are copies; an audio segment served from
// that shared read reads nothing.
func TestReadsSplitWalkAndCopy(t *testing.T) {
	ctx := context.Background()
	mkvSrc := buildInterleavedSource(t, 20)
	mp4Src := hlsplanMP4Source(t)
	cencMKV := buildCENCFixture(t) // CENC reads its samples' NAL units: real ones
	cencMP4 := filepath.Join(t.TempDir(), "cenc.mp4")
	if err := RemuxToMP4(ctx, cencMKV, cencMP4, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef")
	for _, c := range []struct {
		name       string
		src        string
		o          Options
		walk, copy bool // whether each side must have read
	}{
		{"matroska local", mkvSrc, Options{SourceAccess: SourceLocal}, true, true},
		{"matroska network, kept", mkvSrc, Options{SourceAccess: SourceNetwork}, true, false},
		{"matroska local AES-128", mkvSrc, Options{SourceAccess: SourceLocal, Encrypt: &HLSEncryption{Key: key, KeyURI: "k"}}, true, true},
		{"matroska local CENC", cencMKV, Options{SourceAccess: SourceLocal, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "k"}}, true, true},
		{"mp4 local", mp4Src, Options{SourceAccess: SourceLocal}, false, true},
		{"mp4 local CENC", cencMP4, Options{SourceAccess: SourceLocal, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "k"}}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var tally ioTally
			c.o.FS, c.o.StreamFromFS, c.o.SegmentMs = tallyFS(&tally), true, 2000
			p, err := PlanHLS(ctx, c.src, c.o)
			if err != nil {
				t.Fatal(err)
			}
			built := tally.read
			writeOpen(t, p, p.SegmentName(1))
			writeOpen(t, p, "seg_a1_00002.m4s")
			walk, cp := splitOK(t, c.name, p)
			if (walk > 0) != c.walk || (cp > 0) != c.copy {
				t.Errorf("walk %d, copy %d: want walk read %v, copy read %v", walk, cp, c.walk, c.copy)
			}
			if got := walk + cp; got != tally.read-built {
				t.Errorf("walk + copy %d, the filesystem served %d", got, tally.read-built)
			}
		})
	}
	t.Run("mp4 built whole", func(t *testing.T) {
		p, err := PlanHLS(ctx, mp4Src, Options{SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Resource(ctx, p.SegmentName(1)); err != nil {
			t.Fatal(err)
		}
		if walk, cp := splitOK(t, "built whole", p); walk != 0 || cp == 0 {
			t.Errorf("walk %d, copy %d: an MP4 segment built whole is a copy", walk, cp)
		}
	})
	t.Run("mp4 remote, shared", func(t *testing.T) {
		src := twoAudioMP4(t)
		rs := newRemoteSource(t, src)
		p, err := PlanHLS(ctx, rs.srv.URL+"/s.mp4", Options{SegmentMs: 2000, FS: httpfs.New().Port(), StreamFromFS: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.share.dropAll)
		writeOpen(t, p, "seg00002.m4s")
		_, afterVideo := splitOK(t, "shared video", p)
		writeOpen(t, p, "seg_a1_00002.m4s")
		walk, cp := splitOK(t, "shared audio", p)
		if walk != 0 || afterVideo == 0 || cp != afterVideo {
			t.Errorf("walk %d, copy %d after the video, %d after its audio: the shared read is the video's copy, the audio reads nothing", walk, afterVideo, cp)
		}
		if p.Stats().KeptSegments != 1 {
			t.Error(fmt.Sprint("the audio was not served from the shared read: ", p.Stats().KeptSegments))
		}
	})
}
