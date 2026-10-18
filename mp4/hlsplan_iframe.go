package mp4

import (
	"context"

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
		return nil, errf("no I-frame playlist for this plan (Options.NoIFrames is set)")
	}
	if p.mp4src {
		if p.iframe == nil {
			return nil, errf("no I-frame playlist for this plan (an encrypted presentation does not expose one)")
		}
		return p.iframe, nil
	}
	if !p.hasIframe {
		return nil, errf("no I-frame playlist for this plan (an encrypted presentation does not expose one)")
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
// derivation Segment(n) uses - to build the SAME trun/moof RemuxToHLS and
// Segment(n) would write, byte for byte, since that derivation never touches
// sample content. Each segment's I-frame byte range is then the moof's exact
// length plus its leading (keyframe) sample's exact size.
func (p *HLSPlan) buildMatroskaIframePlaylist(ctx context.Context) ([]byte, []iframeRef, error) {
	vi := p.videoIndex()
	pt := p.tracks[vi]
	windows := make([][]fragSample, p.segCount)

	k := 0
	err := p.walkVideoHeadersOnly(ctx, func(b mkv.Block) (bool, error) {
		for k+1 < p.segCount && b.BlockTimecode >= p.bounds[k+1] {
			k++
		}
		windows[k] = append(windows[k], fragSample{
			size:       uint32(int64(len(pt.ft.outTrack.mkv.HeaderStripping)) + b.Size),
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
		head := buildSegmentFile(uint32(seg+1), ts)
		if len(window) > 0 && window[0].sync {
			iframes = append(iframes, iframeRef{seg: seg, length: int64(len(head)) + int64(window[0].size)})
		}
	}
	if len(iframes) == 0 {
		return nil, nil, errf("no I-frame available for this presentation")
	}
	fts := p.fts()
	return buildIFramePlaylist(&p.opts, fts, p.durs, iframes), iframes, nil
}
