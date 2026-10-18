package reader

import (
	"fmt"
	"io"
	"unsafe"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func (p *parser) parseAttachments(size int64, c *mkv.Container) error {
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
		if eh.ID == mkv.IDAttachedFile {
			att, err := p.parseAttachedFile(eh.Size)
			if err != nil {
				return err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(att))); err != nil {
				return err
			}
			c.Attachments = append(c.Attachments, att)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// skipToExactReads is how much attachment payload a metadata read skips
// before it reads through exact windows: a smaller skip stays inside the
// window a remote reader is already streaming.
const skipToExactReads = 256 << 10

func (p *parser) parseAttachedFile(size int64) (mkv.Attachment, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	att := mkv.Attachment{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return att, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDFileUID:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return att, err
			}
			att.ID = v
		case mkv.IDFileName:
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return att, err
			}
			att.Name = v
		case mkv.IDFileMimeType:
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return att, err
			}
			att.MIMEType = v
		case mkv.IDFileData:
			att.Size = eh.Size
			if p.lazyAttachments {
				// Note where the bytes are and step over them: a font set is
				// unbounded user data, and an op that only copies it has no
				// reason to hold it. See reader.WithoutAttachmentData.
				at, _ := p.r.Seek(0, io.SeekCurrent)
				att.DataPath, att.DataOffset = p.path, p.posBase+at
				if err := p.skip(eh.Size); err != nil {
					return att, err
				}
				p.skippedPayload += eh.Size
				if br, ok := p.r.(*bufReadSeeker); ok && p.metaRead && p.skippedPayload >= skipToExactReads {
					br.readAtWindows()
				}
				continue
			}
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
			data, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.Data = data
		default:
			if err := p.skip(eh.Size); err != nil {
				return att, err
			}
		}
	}
	return att, nil
}

func (p *parser) parseTags(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		if err := p.checkCtx(); err != nil {
			return err
		}
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
		if eh.ID == mkv.IDTag {
			tag, err := p.parseTag(eh.Size)
			if err != nil {
				return err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(tag))); err != nil {
				return err
			}
			c.Tags = append(c.Tags, tag)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseTag(size int64) (mkv.Tag, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	tag := mkv.Tag{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return tag, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDTargets:
			if err := p.parseTargets(eh.Size, &tag); err != nil {
				return tag, err
			}
		case mkv.IDSimpleTag:
			st, err := p.parseSimpleTagDepth(eh.Size, 0)
			if err != nil {
				return tag, err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(st))); err != nil {
				return tag, err
			}
			tag.SimpleTags = append(tag.SimpleTags, st)
		default:
			if err := p.skip(eh.Size); err != nil {
				return tag, err
			}
		}
	}
	return tag, nil
}

func (p *parser) parseTargets(size int64, tag *mkv.Tag) error {
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
		switch eh.ID {
		case mkv.IDTargetType:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return err
			}
			tag.TargetType = v
		case mkv.IDTagTrackUID:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			tag.TargetID = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

const maxTagDepth = 64

func (p *parser) parseSimpleTagDepth(size int64, depth int) (mkv.SimpleTag, error) {
	if depth > maxTagDepth {
		return mkv.SimpleTag{}, fmt.Errorf("SimpleTag nesting exceeds %d levels", maxTagDepth)
	}
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	st := mkv.SimpleTag{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return st, err
		}
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDTagName:
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return st, err
			}
			st.Name = v
		case mkv.IDTagString:
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return st, err
			}
			st.Value = v
		case mkv.IDTagLanguage:
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return st, err
			}
			st.Language = v
		case mkv.IDTagBinary:
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Binary = v
		case mkv.IDSimpleTag:
			sub, err := p.parseSimpleTagDepth(eh.Size, depth+1)
			if err != nil {
				return st, err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(sub))); err != nil {
				return st, err
			}
			st.SubTags = append(st.SubTags, sub)
		default:
			if err := p.skip(eh.Size); err != nil {
				return st, err
			}
		}
	}
	return st, nil
}
