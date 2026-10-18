package reader

import (
	"bytes"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

// maxCuesBuffer caps how large a Cues index parseCues pulls into memory in a
// single read before falling back to streaming. Real Cues indexes are at most a
// few MB even for multi-hour 4K files, so this leaves generous headroom while
// refusing to allocate for a pathological or malicious declared size.
const maxCuesBuffer = 64 << 20 // 64 MiB

// maxCuePoints caps the CuePoints a Cues element yields (a cue on every audio frame of 24 h is 4M); a var for tests.
var maxCuePoints = 1 << 23

// parseCues reads the whole Cues index in ONE io.ReadFull and parses it from
// memory, instead of letting the nested CuePoint walk issue ~4 tiny reads per
// entry directly against p.r. A long file's index is tens of thousands of
// CuePoints: on local disk the per-read cost is negligible, but on a network FS
// (9p/SMB) every read is a round-trip and the index alone can take minutes. The
// in-memory bytes.Reader makes the nested seeks free. (ops/reindex.go already
// buffers cluster bodies the same way.) A declared size that is unknown (<0) or
// larger than maxCuesBuffer falls back to streaming off p.r.
func (p *parser) parseCues(size int64, c *mkv.Container) error {
	if size < 0 || size > maxCuesBuffer {
		return p.parseCuesFrom(size, c)
	}
	// readExact rather than make([]byte, size): size comes from the Cues
	// element header, which on a remote FS is only as trustworthy as the
	// server's reported length, so a hostile declared size must not be
	// allocated up front (it grows as bytes actually arrive).
	buf, err := readExact(p.r, size)
	if err != nil {
		return err
	}
	saved := p.r
	p.r = bytes.NewReader(buf)
	err = p.parseCuesFrom(int64(len(buf)), c)
	p.r = saved // the real reader is already positioned past the Cues body
	return err
}

func (p *parser) parseCuesFrom(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if p.pastParent(eh, end) {
			break
		}
		if eh.ID == mkv.IDCuePoint {
			if eh.Size == 0 {
				continue // an empty point: nothing to seek to
			}
			cp, usable, err := p.parseCuePoint(eh.Size)
			if err != nil {
				return err
			}
			if !usable {
				continue // no time or Cluster position a seek can use
			}
			if len(c.Cues) >= maxCuePoints {
				return fmt.Errorf("the Cues hold more than %d points: the file is damaged or forged", maxCuePoints)
			}
			c.Cues = append(c.Cues, cp)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseCuePoint(size int64) (mkv.CuePoint, bool, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	cp := mkv.CuePoint{}
	timed, placed := true, false
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return cp, false, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDCueTime:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return cp, false, err
			}
			cp.TimeMs, timed = fitInt64(v)
		case mkv.IDCueTrackPositions:
			ok, err := p.parseCueTrackPositions(eh.Size, &cp)
			if err != nil {
				return cp, false, err
			}
			placed = placed || ok
		default:
			if err := p.skip(eh.Size); err != nil {
				return cp, false, err
			}
		}
	}
	return cp, timed && placed, nil
}

func (p *parser) parseCueTrackPositions(size int64, cp *mkv.CuePoint) (placed bool, err error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return placed, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDCueTrack:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return placed, err
			}
			cp.Track = v
		case mkv.IDCueClusterPos:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return placed, err
			}
			if n, ok := fitInt64(v); ok {
				cp.ClusterPos, placed = n, true
			}
		case mkv.IDCueRelativePos:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return placed, err
			}
			if n, ok := fitInt64(v); ok {
				cp.RelativePos = n
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return placed, err
			}
		}
	}
	return placed, nil
}
