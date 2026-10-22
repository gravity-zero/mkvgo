package mp4

import (
	"context"
	"fmt"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// iframePlaylist returns the trick-play playlist, built once and cached. MP4
// plans build it eagerly at PlanHLS time (the sample table makes it free);
// Matroska plans build it lazily, on whichever request needs it first, from
// a structure-only pass over the video track (walkVideoHeadersOnly - no
// sample bytes read for the unlaced case), so PlanHLS's construction stays
// the bounded few reads it always was. Only a PERMANENT error is cached (a
// cancelled first request must not poison every later one - the same fix as
// the subtitle-cue scan's).
func (p *HLSPlan) iframePlaylist(ctx context.Context) ([]byte, error) {
	if p.opts.NoIFrames {
		return nil, noResource("no I-frame playlist for this plan (Options.NoIFrames is set)")
	}
	if p.mp4src {
		if p.iframe == nil {
			return nil, noResource("no I-frame playlist for this plan (an encrypted presentation does not expose one)")
		}
		return p.iframe, nil
	}
	if !p.hasIframe {
		return nil, noResource("no I-frame playlist for this plan (an encrypted presentation does not expose one)")
	}
	p.iframeMu.Lock()
	defer p.iframeMu.Unlock()
	if p.iframe != nil {
		return p.iframe, nil
	}
	if p.iframeErr != nil {
		return nil, p.iframeErr
	}
	pl, iframes, err := p.buildMatroskaIframePlaylist(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // transient - never cached
		}
		p.iframeErr = err
		return nil, err
	}
	p.iframe = pl
	p.iframes = iframes
	return pl, nil
}

// peakSegmentIframe returns the segment of highest bit rate as a whole-segment I-frame, nil when no segment has a duration.
func peakSegmentIframe(segs []segInfo) []iframeRef {
	best, peak := -1, 0.0
	for k, s := range segs {
		if s.durSec > 0 {
			if bps := float64(s.bytes) / s.durSec; bps > peak {
				best, peak = k, bps
			}
		}
	}
	if best < 0 {
		return nil
	}
	return []iframeRef{{seg: best, length: segs[best].bytes}}
}

// buildMatroskaIframePlaylist performs the one-time structure-only walk: for
// every segment it collects the video track's samples (size/pts/blockPts/
// sync, never the bytes) and feeds them to timeSegmentWindow - the very
// derivation Segment(n) uses - so each I-frame fragment carries the timing
// RemuxToHLS writes, and where its keyframe lies in the source.
func (p *HLSPlan) buildMatroskaIframePlaylist(ctx context.Context) ([]byte, []iframeRef, error) {
	vi := p.videoIndex()
	pt := p.tracks[vi]
	windows := make([][]fragSample, p.segCount)

	stored := make([]segSample, p.segCount) // each window's first frame, where it lies in the source
	k := 0
	err := p.walkVideoHeadersOnly(ctx, func(b mkv.Block) (bool, error) {
		for k+1 < p.segCount && b.BlockTimecode >= p.bounds[k+1] {
			k++
		}
		size := uint32(int64(len(pt.ft.outTrack.mkv.HeaderStripping)) + b.Size)
		if b.Data != nil {
			data, err := pt.ft.outTrack.mkv.DecodePayload(b.Data)
			if err != nil {
				return false, err
			}
			size = uint32(len(data))
		}
		if len(windows[k]) == 0 {
			stored[k] = segSample{off: b.DataOffset, end: b.DataOffset + b.Size}
		}
		windows[k] = append(windows[k], fragSample{
			size:       size,
			ptsMs:      b.Timecode,
			blockPtsMs: b.BlockTimecode,
			sync:       b.Keyframe,
		})
		return true, nil
	})
	if err != nil {
		return nil, nil, err
	}

	var iframes []iframeRef
	for seg := range windows {
		window := windows[seg]
		nextPts := int64(-1)
		for next := seg + 1; next < len(windows); next++ {
			if len(windows[next]) > 0 {
				nextPts = windowNextPts(windows[next])
				break
			}
		}
		ts := trackSegment{trackID: pt.ft.outTrack.mp4ID}
		ts.baseDecodeTS, ts.hasCTS, _ = timeSegmentWindow(window, pt, nextPts, -1)
		if len(window) > 0 {
			ts.samples = window
			for _, s := range window {
				ts.dataLen += int64(s.size)
			}
		}
		if len(window) > 0 && window[0].sync {
			head := buildSegmentFile(uint32(seg+1), iframeSegment(ts))
			iframes = append(iframes, iframeRef{seg: seg, length: int64(len(head)) + int64(window[0].size),
				head: head, off: stored[seg].off, raw: uint32(stored[seg].end - stored[seg].off)})
		}
	}
	if len(iframes) == 0 {
		return nil, nil, errf("no I-frame available for this presentation")
	}
	fts := p.fts()
	return buildIFramePlaylist(&p.opts, fts, p.durs, iframes), iframes, nil
}

// iframeFragment builds the I-frame fragment name serves: the leading keyframe of its segment alone, behind a moof
// listing it alone, read straight from the source.
func (p *HLSPlan) iframeFragment(ctx context.Context, name string) ([]byte, error) {
	var n int
	if _, err := fmt.Sscanf(name, "iframe%d.m4s", &n); err != nil || name != iframeName(n-1) {
		return nil, noResource("unknown HLS resource %q (see Resources())", name)
	}
	if _, err := p.iframePlaylist(ctx); err != nil {
		return nil, err
	}
	x := sort.Search(len(p.iframes), func(i int) bool { return p.iframes[i].seg >= n-1 })
	if x == len(p.iframes) || p.iframes[x].seg != n-1 {
		return nil, noResource("no I-frame %d: segment %d does not open on a keyframe or is out of range (see iframe.m3u8)", n, n)
	}
	if err := p.checkSource(); err != nil {
		return nil, err
	}
	ifr := p.iframes[x]
	var prefix []byte
	t := &p.tracks[p.videoIndex()].ft.outTrack.mkv
	if p.mp4src {
		seg, offs := p.mp4Segment(p.videoIndex(), ifr.seg)
		ifr.head, ifr.off, ifr.raw = buildSegmentFile(uint32(ifr.seg+1), iframeSegment(seg)), offs[0], seg.samples[0].size
	} else if t.Compression.Verbatim() {
		prefix = t.HeaderStripping
	}
	src, err := p.openSource(ctx)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	raw, err := readSample(countedSource{src, &p.copyRead}, ifr.off, ifr.raw)
	if err != nil {
		return nil, errf("read keyframe: %w", err)
	}
	if !p.mp4src && !t.Compression.Verbatim() {
		if raw, err = t.DecodePayload(raw); err != nil {
			return nil, err
		}
	}
	out := make([]byte, 0, int(ifr.length))
	out = append(append(append(out, ifr.head...), prefix...), raw...)
	if int64(len(out)) != ifr.length {
		return nil, errf("I-frame %d: %d bytes, the playlist's walk counted %d (the source changed? plan it again)", n, len(out), ifr.length)
	}
	return out, nil
}
