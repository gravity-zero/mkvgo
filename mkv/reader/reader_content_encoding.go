package reader

import (
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func (p *parser) parseContentEncodings(size int64, t *mkv.Track) error {
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
		if eh.ID == mkv.IDContentEncoding {
			if err := p.parseContentEncoding(eh.Size, t); err != nil {
				return err
			}
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseContentEncoding(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var encType uint64
	scope := uint64(1)
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
		case mkv.IDContentCompression:
			err = p.parseContentCompression(eh.Size, t)
		case mkv.IDContentEncryption:
			err = p.parseContentEncryption(eh.Size, t)
		case mkv.IDContentEncodingType:
			encType, err = readUintOf(p.r, eh)
		case mkv.IDContentEncodingScope:
			scope, err = readUintOf(p.r, eh)
		default:
			err = p.skip(eh.Size)
		}
		if err != nil {
			return err
		}
	}
	if encType == 1 && t.Encryption == "" {
		t.Encryption = encryptionName(0, 0)
	}
	if encType == 0 && t.Compression != mkv.CompressionNone {
		// The scope says what the compression covers: the blocks, the CodecPrivate, or both.
		if scope&2 != 0 {
			p.privateCompression, p.privateStripped = t.Compression, t.HeaderStripping
		}
		if scope&1 == 0 {
			t.Compression, t.HeaderStripping = mkv.CompressionNone, nil
		}
	}
	return nil
}

// parseContentEncryption names the track's encryption from ContentEncAlgo and the AES cipher mode.
func (p *parser) parseContentEncryption(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var algo, mode uint64
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
		case mkv.IDContentEncAlgo:
			algo, err = readUintOf(p.r, eh)
		case mkv.IDContentEncAESSettings:
			mode, err = p.parseAESCipherMode(eh.Size)
		default:
			err = p.skip(eh.Size)
		}
		if err != nil {
			return err
		}
	}
	t.Encryption = encryptionName(algo, mode)
	return nil
}

// parseAESCipherMode reads the AESSettingsCipherMode of a ContentEncAESSettings element.
func (p *parser) parseAESCipherMode(size int64) (uint64, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var mode uint64
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			return mode, nil
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return 0, err
		}
		if p.pastParent(eh, end) {
			return mode, nil
		}
		if eh.ID == mkv.IDAESSettingsCipherMode {
			mode, err = readUintOf(p.r, eh)
		} else {
			err = p.skip(eh.Size)
		}
		if err != nil {
			return 0, err
		}
	}
}

func (p *parser) parseContentCompression(size int64, t *mkv.Track) error {
	// ContentCompAlgo defaults to 0 (zlib) when the element is absent, so the
	// presence of ContentCompression is itself the declaration. A common muxer writes
	// a compressed subtitle track as an EMPTY ContentCompression element - no
	// children at all - which is why "no child parsed" must not be read as "no
	// compression": that is how a whole PGS track came back as raw zlib.
	t.Compression = mkv.CompressionZlib
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
		case mkv.IDContentCompAlgo:
			algo, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.Compression = mkv.CompressionFromAlgo(algo)
		case mkv.IDContentCompSettings:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			t.HeaderStripping = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}
