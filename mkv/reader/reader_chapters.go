package reader

import (
	"fmt"
	"io"
	"unsafe"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func (p *parser) parseChapters(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var eds []mkv.Edition
	defer func() { placeEditions(c, eds) }()
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
		if eh.ID == mkv.IDEditionEntry {
			ed, err := p.parseEditionEntry(eh.Size)
			if err != nil {
				return err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(ed))); err != nil {
				return err
			}
			eds = append(eds, ed)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseEditionEntry(size int64) (mkv.Edition, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var ed mkv.Edition
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return ed, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDEditionUID:
			if ed.UID, err = readUintOf(p.r, eh); err != nil {
				return ed, err
			}
		case mkv.IDEditionFlagDefault, mkv.IDEditionFlagHidden, mkv.IDEditionFlagOrdered:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return ed, err
			}
			switch eh.ID {
			case mkv.IDEditionFlagDefault:
				ed.Default = v == 1
			case mkv.IDEditionFlagHidden:
				ed.Hidden = v == 1
			default:
				ed.Ordered = v == 1
			}
		case mkv.IDChapterAtom:
			ch, err := p.parseChapterAtom(eh.Size, 0)
			if err != nil {
				return ed, err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(ch))); err != nil {
				return ed, err
			}
			ed.Chapters = append(ed.Chapters, ch)
		default:
			if err := p.skip(eh.Size); err != nil {
				return ed, err
			}
		}
	}
	return ed, nil
}

// placeEditions puts the default edition's chapters (EditionFlagDefault, else the first edition) in Chapters and the others in OtherEditions.
func placeEditions(c *mkv.Container, eds []mkv.Edition) {
	if len(eds) == 0 {
		return
	}
	def := 0
	for i, ed := range eds {
		if ed.Default {
			def = i
			break
		}
	}
	c.Chapters = append(c.Chapters, eds[def].Chapters...)
	for i, ed := range eds {
		if i != def {
			c.OtherEditions = append(c.OtherEditions, ed)
		}
	}
}

func (p *parser) parseChapterAtom(size int64, depth int) (mkv.Chapter, error) {
	if depth > maxChapterDepth {
		return mkv.Chapter{}, fmt.Errorf("chapter nesting exceeds %d levels", maxChapterDepth)
	}
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	ch := mkv.Chapter{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return ch, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDChapterUID:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return ch, err
			}
			ch.ID = v
		case mkv.IDChapterTimeStart:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return ch, err
			}
			ch.StartMs = int64(v / 1000000)
		case mkv.IDChapterTimeEnd:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return ch, err
			}
			ch.EndMs = int64(v / 1000000)
		case mkv.IDChapterDisplay:
			if err := p.parseChapterDisplay(eh.Size, &ch); err != nil {
				return ch, err
			}
		case mkv.IDChapterAtom:
			sub, err := p.parseChapterAtom(eh.Size, depth+1)
			if err != nil {
				return ch, err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(sub))); err != nil {
				return ch, err
			}
			ch.SubChapters = append(ch.SubChapters, sub)
		case mkv.IDChapterSegmentUID:
			if err := p.chargeMeta(eh.Size); err != nil {
				return ch, err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return ch, err
			}
			ch.SegmentUID = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return ch, err
			}
		}
	}
	return ch, nil
}

func (p *parser) parseChapterDisplay(size int64, ch *mkv.Chapter) error {
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
		if eh.ID == mkv.IDChapString {
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return err
			}
			ch.Title = v
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

const maxChapterDepth = 64
