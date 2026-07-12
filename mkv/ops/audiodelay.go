package ops

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// audiodelay.go - AudioStartDelays measures the repack defect ("audio content
// starts late") natively: the first block timecode of every audio track
// against the first video keyframe, from the head and the first cluster(s)
// only. Track numbers and delays come from the SAME parse, so a caller
// feeding RetimeTracks never has to correlate an external prober's
// order-based stream listing with Matroska track numbers.

// audioDelayScanCap bounds the cluster scan: a track that has not produced a
// block within this many bytes of cluster data is reported absent rather
// than walking a whole movie for it. A var so tests can shrink it.
var audioDelayScanCap int64 = 64 << 20 // 64 MiB

// AudioStartDelays returns, for each AUDIO track (by Matroska track number),
// how late its content starts relative to the video, in nanoseconds -
// positive means the audio begins after the first video keyframe (the value
// to hand RetimeTracks, negated, to cancel the delay). On a file with no
// video track the anchor is the earliest block of any track. Tracks that
// produce no block within the bounded scan are absent from the map.
//
// Head-mostly: the track list is read head-only, then clusters are walked
// just long enough to see every track's first block (typically the first
// cluster or two).
func AudioStartDelays(ctx context.Context, path string, opts ...mkv.Options) (map[uint64]int64, error) {
	fs := mkv.FSFrom(opts)
	meta, err := reader.OpenMetaWithFS(ctx, path, fs)
	if err != nil {
		return nil, fmt.Errorf("audio delays: %w", err)
	}
	scale := meta.Info.TimecodeScale
	if scale <= 0 {
		scale = 1_000_000
	}
	audio := make(map[uint64]bool)
	video := videoTrackSet(meta.Tracks)
	for _, t := range meta.Tracks {
		if t.Type == mkv.AudioTrack {
			audio[t.ID] = true
		}
	}
	if len(audio) == 0 {
		return map[uint64]int64{}, nil
	}

	firstTC, kfTC, err := scanFirstBlockTimes(ctx, path, fs, audio, video)
	if err != nil {
		return nil, err
	}

	// Anchor: the first video keyframe; without video, the earliest block.
	anchor, haveAnchor := kfTC, kfTC != nil
	if !haveAnchor {
		for _, tc := range firstTC {
			if anchor == nil || tc < *anchor {
				v := tc
				anchor = &v
			}
		}
		haveAnchor = anchor != nil
	}
	delays := make(map[uint64]int64, len(audio))
	if !haveAnchor {
		return delays, nil
	}
	for track := range audio {
		tc, ok := firstTC[track]
		if !ok {
			continue
		}
		delays[track] = (tc - *anchor) * scale
	}
	return delays, nil
}

// scanFirstBlockTimes walks clusters from the head, bounded by
// audioDelayScanCap, recording the first block timecode (ticks) of every
// track in `want` and the first video keyframe timecode. It stops as soon as
// both are satisfied.
func scanFirstBlockTimes(ctx context.Context, path string, fs *mkv.FS, want, video map[uint64]bool) (firstTC map[uint64]int64, videoKF *int64, err error) {
	raw, err := fs.DoOpen(path)
	if err != nil {
		return nil, nil, fmt.Errorf("audio delays: open: %w", err)
	}
	defer raw.Close()
	r := bufio.NewReaderSize(raw, reindexBufSize)

	ebmlHdr, _, err := ebml.ReadElementHeader(r)
	if err != nil || ebmlHdr.ID != ebml.IDEBMLHeader || ebmlHdr.Size < 0 {
		return nil, nil, fmt.Errorf("audio delays: not a Matroska file")
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		return nil, nil, fmt.Errorf("audio delays: skip EBML header: %w", err)
	}
	if segHdr, _, err := ebml.ReadElementHeader(r); err != nil || segHdr.ID != mkv.IDSegment {
		return nil, nil, fmt.Errorf("audio delays: expected Segment")
	}

	firstTC = make(map[uint64]int64)
	needKF := len(video) > 0
	done := func() bool {
		if needKF && videoKF == nil {
			return false
		}
		for t := range want {
			if _, ok := firstTC[t]; !ok {
				return false
			}
		}
		return true
	}

	// The loop is tolerant past this point: a truncated or damaged tail does
	// not invalidate the first-block times already measured - the scan stops
	// and reports what it saw (Diagnose runs this on broken files by design).
	var buf []byte
	var scanned int64
	for scanned < audioDelayScanCap && !done() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		h, _, err := ebml.ReadElementHeader(r)
		if err != nil {
			break
		}
		if h.Size < 0 || h.Size > maxReindexClusterSize {
			break
		}
		if h.ID != mkv.IDCluster {
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				break
			}
			continue
		}
		if int(h.Size) > len(buf) {
			buf = make([]byte, h.Size)
		}
		body := buf[:h.Size]
		n, err := io.ReadFull(r, body)
		// A cut-off cluster still carries blocks up to the cut: scan the
		// prefix that was read, then stop.
		scanClusterFirstTimes(body[:n], want, video, firstTC, &videoKF)
		if err != nil {
			break
		}
		scanned += h.Size
	}
	return firstTC, videoKF, nil
}

// scanClusterFirstTimes updates firstTC/videoKF from one cluster body.
func scanClusterFirstTimes(body []byte, want, video map[uint64]bool, firstTC map[uint64]int64, videoKF **int64) {
	br := bytes.NewReader(body)
	var clusterTS int64
	record := func(track uint64, relTC int16, keyframe bool) {
		abs := clusterTS + int64(relTC)
		if cur, ok := firstTC[track]; !ok || abs < cur {
			firstTC[track] = abs
		}
		if keyframe && video[track] {
			if *videoKF == nil || abs < **videoKF {
				v := abs
				*videoKF = &v
			}
		}
	}
	for {
		h, _, err := ebml.ReadElementHeader(br)
		if err != nil || h.Size < 0 || h.Size > int64(br.Len()) {
			return
		}
		switch h.ID {
		case mkv.IDTimestamp:
			v, err := ebml.ReadUint(br, h.Size)
			if err != nil {
				return
			}
			clusterTS = int64(v)
		case mkv.IDSimpleBlock:
			track, relTC, keyframe, err := readBlockHeader(br, h.Size)
			if err != nil {
				return
			}
			record(track, relTC, keyframe)
		case mkv.IDBlockGroup:
			track, relTC, isKey, err := scanBlockGroup(br, h.Size)
			if err != nil {
				return
			}
			record(track, relTC, isKey)
		default:
			if _, err := io.CopyN(io.Discard, br, h.Size); err != nil {
				return
			}
		}
	}
}
