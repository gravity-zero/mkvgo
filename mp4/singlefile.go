package mp4

// singlefile.go - Options.SingleFile: each rendition becomes ONE progressive
// file (init + sidx + all CMAF fragments) served by byte ranges - the HLS
// playlists use EXT-X-BYTERANGE and the DASH manifest the on-demand profile's
// SegmentBase/indexRange. One file per rendition instead of hundreds of
// segments: friendlier to object storage and plain HTTP servers (which must
// only support Range requests). Fragment sizes are computed before anything
// is written (the builders are deterministic), so the sidx lands in one pass.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

// sfRendition describes one single-file rendition for the playlists/manifest.
type sfRendition struct {
	name     string     // the file name (stream.mp4, stream_a1.mp4)
	initLen  int64      // bytes 0..initLen-1 = ftyp+moov
	sidxEnd  int64      // initLen..sidxEnd-1 = the sidx (DASH indexRange)
	ranges   [][2]int64 // per segment: offset, length
	fileSize int64
}

// singleFileName names track i's single-file rendition.
func singleFileName(fts []*fragTrack, i int) string {
	if fts[i].outTrack.spec.video {
		return "stream.mp4"
	}
	return fmt.Sprintf("stream_a%d.mp4", audioIndex(fts, i))
}

// buildSidx renders a Segment Index box: one reference per fragment, each a
// media subsegment starting with a SAP (segments are keyframe-cut).
//
// earliestPTS is the first subsegment's earliest composition time in the track's
// MEDIA timeline - which is the composition shift when the track is reordered (its
// offsets were pushed up by it; the edit list, a different timeline, is not what
// this box speaks in). Zero would claim a presentation time no sample has.
func buildSidx(trackID, timescale uint32, earliestPTS int64, sizes []int64, durTS []int64) []byte {
	return fullBox("sidx", 0, 0, func(w *bw) {
		w.u32(trackID)
		w.u32(timescale)
		w.u32(uint32(earliestPTS)) // earliest_presentation_time
		w.u32(0)                   // first_offset: subsegments start right after the sidx
		w.u16(0)                   // reserved
		w.u16(uint16(len(sizes)))
		for i := range sizes {
			w.u32(uint32(sizes[i]))       // reference_type(0) + referenced_size
			w.u32(uint32(durTS[i]))       // subsegment_duration
			w.u32(0x80000000 | (1 << 28)) // starts_with_SAP, SAP type 1
		}
	})
}

// writeSingleFileRenditions writes one progressive file per rendition and
// returns the aggregate per-boundary segInfo plus each rendition's byte map.
func writeSingleFileRenditions(ctx context.Context, o *Options, fs *mkv.FS, dir string,
	fts []*fragTrack, bounds []int64, meta movieMeta) ([]segInfo, []sfRendition, error) {
	readers := make([]mkv.ReadSeekCloser, len(fts))
	for i, ft := range fts {
		r, err := fs.DoOpen(ft.tmpPath)
		if err != nil {
			return nil, nil, err
		}
		readers[i] = r
	}
	defer func() {
		for _, r := range readers {
			if r != nil {
				r.Close()
			}
		}
	}()

	// Pass 1 (in memory): per rendition, the fragment heads and sizes - the
	// builders are deterministic, so no media is read yet.
	type plan struct {
		heads [][]byte
		segs  []trackSegment
	}
	plans := make([]plan, len(fts))
	cursors := make([]int, len(fts))
	segDur := make([][]int64, len(fts)) // per rendition, per segment, track-timescale duration
	infos := make([]segInfo, 0, len(bounds))
	for k := 0; k < len(bounds); k++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		segStart := bounds[k]
		var segEnd int64 = 1<<63 - 1
		if k+1 < len(bounds) {
			segEnd = bounds[k+1]
		}
		endMs := segEndOrLast(bounds, k, fts)
		var segBytes int64
		for i, ft := range fts {
			seg := segmentWindow(ft, &cursors[i], segEnd)
			head := buildSegmentFile(uint32(k+1), seg)
			plans[i].heads = append(plans[i].heads, head)
			plans[i].segs = append(plans[i].segs, seg)
			scale := tsScale(ft.timescale)
			segDur[i] = append(segDur[i], scale(endMs)-scale(segStart))
			segBytes += int64(len(head)) + seg.dataLen
		}
		infos = append(infos, segInfo{durSec: float64(endMs-segStart) / 1000, bytes: segBytes})
	}

	// Pass 2: assemble each rendition file - init, sidx (sizes now known),
	// then the fragments with their media streamed from the temp files.
	rends := make([]sfRendition, len(fts))
	for i, ft := range fts {
		m := movieMeta{}
		if ft.outTrack.spec.video {
			m = meta
		}
		initData := buildInitSegment([]*fragTrack{ft}, m, nil) // CENC + SingleFile is refused up front
		sizes := make([]int64, len(plans[i].heads))
		for k := range plans[i].heads {
			sizes[k] = int64(len(plans[i].heads[k])) + plans[i].segs[k].dataLen
		}
		sidx := buildSidx(ft.outTrack.mp4ID, ft.timescale, ft.ctsShiftTS, sizes, segDur[i])

		r := &rends[i]
		r.name = singleFileName(fts, i)
		r.initLen = int64(len(initData))
		r.sidxEnd = r.initLen + int64(len(sidx))
		out, err := fs.DoCreate(filepath.Join(dir, r.name))
		if err != nil {
			return nil, nil, err
		}
		werr := func() error {
			if _, err := out.Write(initData); err != nil {
				return err
			}
			if _, err := out.Write(sidx); err != nil {
				return err
			}
			off := r.sidxEnd
			for k := range plans[i].heads {
				if err := writeSegmentFile(out, plans[i].heads[k], plans[i].segs[k].dataLen, readers[i]); err != nil {
					return err
				}
				r.ranges = append(r.ranges, [2]int64{off, sizes[k]})
				off += sizes[k]
			}
			r.fileSize = off
			return nil
		}()
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, nil, werr
		}
	}
	return infos, rends, nil
}

// buildByteRangePlaylist renders a media playlist over one single-file
// rendition: EXT-X-MAP with a BYTERANGE and one EXT-X-BYTERANGE per segment.
func buildByteRangePlaylist(o *Options, durs []float64, r *sfRendition) []byte {
	rw := urlRewriter(o)
	var max float64
	for _, d := range durs {
		if d > max {
			max = d
		}
	}
	var b []byte
	b = append(b, "#EXTM3U\n#EXT-X-VERSION:7\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int64(max+0.999))...)
	b = append(b, "#EXT-X-PLAYLIST-TYPE:VOD\n"...)
	b = append(b, fmt.Sprintf("#EXT-X-MAP:URI=%q,BYTERANGE=\"%d@0\"\n", rw(r.name), r.sidxEnd)...)
	for k, d := range durs {
		b = append(b, fmt.Sprintf("#EXTINF:%.3f,\n#EXT-X-BYTERANGE:%d@%d\n%s\n",
			d, r.ranges[k][1], r.ranges[k][0], rw(r.name))...)
	}
	b = append(b, "#EXT-X-ENDLIST\n"...)
	return b
}

// buildDASHManifestSingle renders the on-demand-profile MPD: one BaseURL +
// SegmentBase (the sidx's indexRange) per rendition.
func buildDASHManifestSingle(o *Options, fts []*fragTrack, subs []hlsSubTrack, durs []float64,
	bandwidth int64, rends []sfRendition) []byte {
	rw := urlRewriter(o)
	var totalSec, maxDur float64
	for _, d := range durs {
		totalSec += d
		if d > maxDur {
			maxDur = d
		}
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011" mediaPresentationDuration="%s" minBufferTime="%s">`+"\n",
		dashDuration(totalSec), dashDuration(maxDur))
	b.WriteString("  <Period>\n")
	for i, ft := range fts {
		t := &ft.outTrack.mkv
		var as, rep string
		if ft.outTrack.spec.video {
			as = `mimeType="video/mp4" contentType="video"`
			rep = fmt.Sprintf(`id="v" bandwidth="%d"`, bandwidth)
			if t.Width != nil && t.Height != nil && *t.Width > 0 && *t.Height > 0 {
				rep += fmt.Sprintf(` width="%d" height="%d"`, *t.Width, *t.Height)
			}
			if t.FrameRate != nil && *t.FrameRate > 0 {
				rep += fmt.Sprintf(` frameRate="%s"`, dashFrameRate(*t.FrameRate))
			}
		} else {
			as = `mimeType="audio/mp4" contentType="audio"`
			if t.Language != "" {
				as += fmt.Sprintf(" lang=%q", t.Language)
			}
			rep = fmt.Sprintf(`id="a%d" bandwidth="0"`, audioIndex(fts, i))
			if t.SampleRate != nil && *t.SampleRate > 0 {
				rep += fmt.Sprintf(` audioSamplingRate="%d"`, int64(*t.SampleRate))
			}
		}
		if codecs := rfc6381Codec(ft.outTrack); codecs != "" {
			rep += fmt.Sprintf(" codecs=%q", codecs)
		}
		r := &rends[i]
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		fmt.Fprintf(&b, "      <Representation %s>\n", rep)
		fmt.Fprintf(&b, "        <BaseURL>%s</BaseURL>\n", rw(r.name))
		fmt.Fprintf(&b, `        <SegmentBase indexRange="%d-%d"><Initialization range="0-%d"/></SegmentBase>`+"\n",
			r.initLen, r.sidxEnd-1, r.initLen-1)
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}
	for i := range subs {
		t := &subs[i].track
		as := `mimeType="text/vtt" contentType="text"`
		if t.Language != "" {
			as += fmt.Sprintf(" lang=%q", t.Language)
		}
		fmt.Fprintf(&b, "    <AdaptationSet %s>\n", as)
		fmt.Fprintf(&b, `      <Representation id="sub%d" bandwidth="0">`+"\n", i+1)
		fmt.Fprintf(&b, "        <BaseURL>%s</BaseURL>\n", rw(fmt.Sprintf("sub%d.vtt", i+1)))
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}
	b.WriteString("  </Period>\n</MPD>\n")
	return []byte(b.String())
}
