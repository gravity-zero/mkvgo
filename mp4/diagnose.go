package mp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

// diagnose.go - Diagnose for MP4/MOV: the same one-call triage the Matroska
// side offers (ops.Diagnose), returning the same mkv.Diagnosis shape so one
// scan loop covers a mixed library. MP4 needs no index triage (the sample
// table IS the index, mandatory and inline) and no tolerant walk: everything
// a triage needs is head-only - the top-level box layout tells truncation
// and trailing junk apart, and each track's edit list carries its
// presentation delay, the exact value `retime` cancels.

// diagnoseDelayThresholdNs mirrors the Matroska triage's threshold: an audio
// track presenting this late (or later) becomes a finding; the raw values
// are always in the report for callers with their own threshold.
const diagnoseDelayThresholdNs = 100_000_000 // 100ms

// Diagnose classifies an MP4/MOV in one call and names the remedy for every
// finding, head-only. Finding kinds: "truncated" (a declared box overruns
// the real end of file - an incomplete download; present X of Y declared
// bytes), "no-moov" (no movie header anywhere - nothing any tool can
// rebuild), "trailing-junk" (bytes after the last well-formed box), and
// "audio-delay" (per track, from the edit list, with the exact retime
// invocation). AudioDelaysNs reports every audio track's presentation delay
// relative to the video, fragmented sources excepted (fragment timelines do
// not honour edit lists consistently, so no delay is derived or judged).
func Diagnose(ctx context.Context, path string, opts ...Options) (*mkv.Diagnosis, error) {
	o := optionsFrom(opts)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := o.FS.DoOpen(path)
	if err != nil {
		return nil, errf("diagnose: %w", err)
	}
	defer f.Close()

	d := &mkv.Diagnosis{AudioDelaysNs: map[uint64]int64{}}
	lay := diagnoseScanBoxes(f, d)

	if lay.moovOff < 0 {
		d.Findings = append(d.Findings, mkv.Finding{
			Kind:   "no-moov",
			Detail: "no moov box found: the movie header (and with it the sample table) is missing",
			Remedy: "re-download the source (no tool can rebuild a missing sample table)",
		})
		d.Healthy = false
		return d, nil
	}

	moovRaw, err := readRange(f, lay.moovOff+lay.moovHdr, lay.moovSize-lay.moovHdr)
	if err != nil {
		return nil, errf("diagnose: read moov: %w", err)
	}
	if err := diagnoseTracks(moovRaw, d); err != nil {
		return nil, errf("diagnose: %s: %w", path, err)
	}
	d.Healthy = len(d.Findings) == 0
	return d, nil
}

// diagnoseScanBoxes walks the top-level boxes tolerantly: instead of failing
// on a malformed layout it records the finding (truncated / trailing junk)
// and still reports the moov when one is reachable.
func diagnoseScanBoxes(f mkv.ReadSeekCloser, d *mkv.Diagnosis) *topLayout {
	lay := &topLayout{moovOff: -1}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return lay
	}
	lay.fileSize = size
	var off int64
	hdr := make([]byte, 16)
	for off+8 <= size {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return lay
		}
		if _, err := io.ReadFull(f, hdr[:8]); err != nil {
			return lay
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hdrLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := io.ReadFull(f, hdr[8:16]); err != nil {
				return lay
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hdrLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < hdrLen || !plausibleBoxType(typ) {
			d.Findings = append(d.Findings, mkv.Finding{
				Kind:   "trailing-junk",
				Detail: fmt.Sprintf("%d byte(s) at offset %d are not a well-formed box", size-off, off),
				Remedy: "remux the file (a rewrite drops them)",
			})
			return lay
		}
		if off+boxSize > size {
			d.Findings = append(d.Findings, mkv.Finding{
				Kind: "truncated",
				Detail: fmt.Sprintf("box %q at offset %d declares %d bytes but only %d remain: the source ends early",
					typ, off, boxSize, size-off),
				Remedy: "re-download the source (the missing tail is not recoverable by any tool)",
			})
			return lay
		}
		if typ == "moov" {
			lay.moovOff, lay.moovSize, lay.moovHdr = off, boxSize, hdrLen
		}
		off += boxSize
	}
	if off < size {
		d.Findings = append(d.Findings, mkv.Finding{
			Kind:   "trailing-junk",
			Detail: fmt.Sprintf("%d byte(s) beyond the last box", size-off),
			Remedy: "remux the file (a rewrite drops them)",
		})
	}
	return lay
}

// plausibleBoxType accepts a fourcc of printable bytes - the cheap test that
// separates a real box header from spliced garbage.
func plausibleBoxType(typ string) bool {
	for i := 0; i < len(typ); i++ {
		if typ[i] < 0x20 || typ[i] > 0x7E {
			return false
		}
	}
	return true
}

// diagnoseTracks derives each track's presentation delay from its edit list
// and turns audio delays beyond the threshold into findings. Fragmented
// sources are skipped entirely (their timelines live in the fragments).
func diagnoseTracks(moovPayload []byte, d *mkv.Diagnosis) error {
	boxes, err := iterBoxes(moovPayload)
	if err != nil {
		return err
	}
	if _, fragmented := findMemBox(boxes, "mvex"); fragmented {
		return nil
	}
	mvhd, ok := findMemBox(boxes, "mvhd")
	if !ok {
		return fmt.Errorf("moov without mvhd")
	}
	movieTS, _ := parseMovieHeader(mvhd.payload)
	if movieTS == 0 {
		return fmt.Errorf("mvhd declares no timescale")
	}

	type trackDelay struct {
		num     uint64
		handler string
		ns      int64
	}
	var tracks []trackDelay
	var num uint64
	for _, b := range boxes {
		if b.typ != "trak" {
			continue
		}
		num++
		tb, err := iterBoxes(b.payload)
		if err != nil {
			continue // an unparseable foreign trak is not this triage's business
		}
		entries, err := trakElstEntries(tb)
		if err != nil {
			continue
		}
		var empty int64
		for _, e := range entries {
			if e.mediaTime < 0 {
				empty += e.segDur
			}
		}
		tracks = append(tracks, trackDelay{
			num:     num,
			handler: trakHandler(tb),
			ns:      empty * 1_000_000_000 / int64(movieTS),
		})
	}

	// Anchor on the earliest video presentation (0 without video), exactly
	// like the Matroska probe: the reported value is the A/V misalignment,
	// and its negation is the retime shift that cancels it.
	var videoAnchor int64
	haveVideo := false
	for _, t := range tracks {
		if t.handler == "vide" && (!haveVideo || t.ns < videoAnchor) {
			videoAnchor, haveVideo = t.ns, true
		}
	}
	for _, t := range tracks {
		if t.handler != "soun" {
			continue
		}
		delay := t.ns - videoAnchor
		d.AudioDelaysNs[t.num] = delay
		if delay >= diagnoseDelayThresholdNs {
			d.Findings = append(d.Findings, mkv.Finding{
				Kind:    "audio-delay",
				Detail:  fmt.Sprintf("audio track %d is presented %dms after the video (edit list)", t.num, delay/1_000_000),
				Remedy:  fmt.Sprintf("mkvgo retime --shift %d=-%d", t.num, delay/1_000_000),
				Track:   t.num,
				DelayNs: delay,
			})
		}
	}
	return nil
}

// trakHandler returns the trak's mdia/hdlr handler fourcc ("vide", "soun",
// "text"...), or "" when unreadable.
func trakHandler(trakBoxes []memBox) string {
	mdia, ok := findMemBox(trakBoxes, "mdia")
	if !ok {
		return ""
	}
	mb, err := iterBoxes(mdia.payload)
	if err != nil {
		return ""
	}
	hdlr, ok := findMemBox(mb, "hdlr")
	if !ok || len(hdlr.payload) < 12 {
		return ""
	}
	return string(hdlr.payload[8:12])
}
