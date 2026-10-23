package mp4

import "context"

// Segment builds the n-th (0-based) VIDEO media segment (segNNNNN.m4s)  -
// audio renditions' segments are served through Resource by name. The bytes
// equal the corresponding segment RemuxToHLS writes.
func (p *HLSPlan) Segment(ctx context.Context, n int) ([]byte, error) {
	return p.segmentTrack(ctx, p.videoIndex(), n)
}

// buildTrackSegment frames one rendition's window as a media segment - styp +
// moof + mdat - from the samples the walk read for it.
//
// arena, when not nil, is the buffer the walk read this rendition's blocks
// into, back to back behind windowHeadroom bytes of room (windowInPlace): the
// head is then written in front of them and that buffer is the segment.
func (p *HLSPlan) buildTrackSegment(ti, n int, window []segSample, nextPts, startK int64, arena []byte) ([]byte, error) {
	pt := p.tracks[ti]
	seg := p.timeWindow(ti, n, window, nextPts, startK)

	var cipherData []byte
	if p.opts.CENC != nil {
		// Same requirement as the full pass (hls.go's writeSegments): CENC needs
		// the plaintext bytes before the head is built, so the moof's senc/saiz
		// (sized from the samples) are ready when buildSegmentFile frames it.
		plain := make([]byte, 0, seg.dataLen)
		for x := range window {
			plain = append(plain, window[x].data...)
		}
		td, cipher, err := prepareCENCSegment(p.opts.CENC, pt.ft.outTrack.spec.video, pt.ft.outTrack.mkv.Codec, pt.ft.outTrack.mkv.CodecPrivate, pt.ft.outTrack.mkv.ID, seg.samples, plain)
		if err != nil {
			return nil, err
		}
		seg.cenc = td
		cipherData = cipher
	}
	head := buildSegmentFile(uint32(n+1), seg)
	var out []byte
	switch {
	case p.opts.CENC != nil:
		out = make([]byte, 0, int64(len(head))+seg.dataLen)
		out = append(out, head...)
		out = append(out, cipherData...)
	case len(arena) > windowHeadroom && int64(len(arena)-windowHeadroom) == seg.dataLen && len(head) <= windowHeadroom:
		// Every sample of the window sits in the arena, in order: the head
		// goes right before them.
		out = arena[windowHeadroom-len(head):]
		copy(out, head)
	default:
		out = make([]byte, 0, int64(len(head))+seg.dataLen)
		out = append(out, head...)
		for x := range window {
			out = append(out, window[x].data...)
		}
	}
	if p.opts.Encrypt != nil {
		return p.opts.Encrypt.encryptSegment(out, uint32(n))
	}
	return out, nil
}

// segmentTrack builds the n-th segment of the ti-th track and counts it served.
func (p *HLSPlan) segmentTrack(ctx context.Context, ti, n int) ([]byte, error) {
	data, err := p.buildSegmentTrack(ctx, ti, n)
	if err == nil {
		p.winMu.Lock()
		p.stats.ServedSegments++
		p.winMu.Unlock()
	}
	return data, err
}

// buildSegmentTrack builds the n-th segment of the ti-th track, reading only that
// segment's window from the source. The walk that reads the window builds every
// rendition of it (it had to read all their bytes to reach its own), so the
// sibling segments of the same instant - the audio a viewer pulls beside its
// video - are already built when they are asked for, and cost no I/O at all.
func (p *HLSPlan) buildSegmentTrack(ctx context.Context, ti, n int) ([]byte, error) {
	if n < 0 || n >= p.segCount {
		return nil, noResource("segment %d out of range (0..%d)", n, p.segCount-1)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.checkSource(); err != nil {
		return nil, err
	}
	if p.mp4src {
		release, _, err := acquireWalk(ctx) // the segment is held whole while it is built: the walk cap bounds them
		if err != nil {
			return nil, err
		}
		data, err := p.mp4SegmentTrack(ctx, ti, n)
		release()
		if err == nil {
			p.winMu.Lock()
			p.stats.BuiltBytes += int64(len(data))
			p.stats.ServedBytes += int64(len(data))
			p.winMu.Unlock()
		}
		return data, err
	}
	b, built, err := p.window(ctx, n)
	if err != nil {
		return nil, err
	}
	if data := p.takeRendition(n, ti, b, !built); data != nil {
		return data, nil
	}
	// Another request collected this rendition and its bytes were released with
	// it. Rebuild the window: correctness never rests on the cache being there.
	fresh, err := p.buildWindow(ctx, n)
	if err != nil {
		return nil, err
	}
	data := fresh.segs[ti]
	p.winMu.Lock()
	p.noteBuild(n, fresh)
	p.stats.ServedBytes += int64(len(data))
	p.stats.DroppedBytes += fresh.bytes - int64(len(data)) // the rest of this walk is kept for nobody
	p.winMu.Unlock()
	return data, nil
}
