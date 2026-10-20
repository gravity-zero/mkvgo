package reader

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"unsafe"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/trackchild"
	"github.com/gravity-zero/mkvgo/mkv"
)

func (p *parser) parseTracks(size int64, c *mkv.Container) error {
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
		if eh.ID == mkv.IDTrackEntry {
			track, err := p.parseTrackEntry(eh.Size)
			if err != nil {
				return err
			}
			if err := p.chargeMeta(int64(unsafe.Sizeof(track))); err != nil {
				return err
			}
			if track.ID == 0 { // TrackNumber is mandatory and never 0: no block can address this entry
				continue
			}
			c.Tracks = append(c.Tracks, track)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseTrackEntry(size int64) (mkv.Track, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	t := mkv.Track{}
	p.rateRejected, p.channelsRejected = false, false
	p.privateCompression, p.privateStripped = mkv.CompressionNone, nil

	for {
		p.unhold() // the child kept last was parsed from its copy: back to the source
		if err := p.checkCtx(); err != nil {
			return t, err
		}
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return t, err
		}
		if p.pastParent(eh, end) {
			break
		}
		if err := p.keepUnmodeled(&t, mkv.IDTrackEntry, eh); err != nil {
			return t, err
		}
		switch eh.ID {
		case mkv.IDTrackNumber:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.ID = v
		case mkv.IDTrackUID:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.UID = v
		case mkv.IDTrackType:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.Type = mkv.TrackTypeOf(v)
		case mkv.IDCodecID:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return t, err
			}
			if short, ok := mkv.CodecShortName[v]; ok {
				t.Codec = short
			} else {
				t.Codec = v
			}
		case mkv.IDCodecPrivate:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.CodecPrivate = v
		case mkv.IDLanguage:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.Language = v
			t.LanguagePresent = true
		case mkv.IDLanguageBCP47:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.LanguageBCP47 = v
			t.LanguagePresent = true
		case mkv.IDName:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.Name = v
		case mkv.IDFlagDefault:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.IsDefault = v == 1
			t.DefaultPresent = true
		case mkv.IDFlagForced:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			t.IsForced = v == 1
			t.ForcedPresent = true
		case mkv.IDFlagHearingImpaired, mkv.IDFlagVisualImpaired, mkv.IDFlagTextDescriptions,
			mkv.IDFlagOriginal, mkv.IDFlagCommentary:
			b, e := p.readFlag(eh.Size)
			if e != nil {
				return t, e
			}
			switch eh.ID {
			case mkv.IDFlagHearingImpaired:
				t.HearingImpaired = b
			case mkv.IDFlagVisualImpaired:
				t.VisualImpaired = b
			case mkv.IDFlagTextDescriptions:
				t.TextDescriptions = b
			case mkv.IDFlagOriginal:
				t.Original = b
			case mkv.IDFlagCommentary:
				t.Commentary = b
			}
		case mkv.IDDefaultDuration:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			if v > 0 && v <= math.MaxInt64 {
				fps := 1e9 / float64(v)
				t.FrameRate = &fps
				t.DefaultDurationNs = int64(v)
			}
		case mkv.IDCodecDelay:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			if n, ok := fitInt64(v); ok {
				t.CodecDelay = n
			}
		case mkv.IDSeekPreRoll:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return t, err
			}
			if n, ok := fitInt64(v); ok {
				t.SeekPreRoll = n
			}
		case mkv.IDVideo:
			if err := p.parseVideoSettings(eh.Size, &t); err != nil {
				return t, err
			}
		case mkv.IDAudio:
			if err := p.parseAudioSettings(eh.Size, &t); err != nil {
				return t, err
			}
		case mkv.IDContentEncodings:
			if err := p.parseContentEncodings(eh.Size, &t); err != nil {
				return t, err
			}
		case mkv.IDBlockAdditionMapping:
			if err := p.parseBlockAdditionMapping(eh.Size, &t); err != nil {
				return t, err
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return t, err
			}
		}
	}
	// FlagDefault defaults to 1 per the Matroska spec when absent: keep that for
	// IsDefault, but DefaultPresent stays false so a consumer can tell an explicit
	// flag from the applied default. Language is intentionally NOT defaulted to
	// "eng" (v0.4.0 behaviour change): an absent language stays "" with
	// LanguagePresent=false, matching what probers report.
	if !t.DefaultPresent {
		t.IsDefault = true
	}
	// DefaultDuration exists on audio tracks too (block duration), but exposing it
	// as FrameRate is only meaningful for video - and matches external probers, which only
	// reports r_frame_rate for video streams.
	if t.Type != mkv.VideoTrack {
		t.FrameRate = nil
	}
	if p.privateCompression != mkv.CompressionNone && len(t.CodecPrivate) > 0 {
		enc := mkv.Track{ID: t.ID, Compression: p.privateCompression, HeaderStripping: p.privateStripped}
		cp, err := enc.DecodePayload(t.CodecPrivate)
		if err != nil {
			return t, fmt.Errorf("track %d: its CodecPrivate is declared compressed: %w", t.ID, err)
		}
		t.CodecPrivate = cp
	}
	// Fill colour/bit-depth/profile from the codec bitstream when the container
	// Colour element didn't (container values, parsed above, still win per field).
	stated := t
	fillColourFromCodecPrivate(&t)
	recordDerived(&t, &stated)
	fillCodecDerived(&t)
	audioDefaults(&t, p.rateRejected, p.channelsRejected)
	return t, nil
}

// recordDerived notes in t.Derived the written fields the bitstream filled where the container (stated) said nothing.
func recordDerived(t, stated *mkv.Track) {
	d := mkv.DerivedFields{}
	if t.Derived != nil {
		d = *t.Derived
	}
	pick16 := func(had, was, now *uint16) *uint16 {
		if was == nil && now != nil {
			return now
		}
		return had
	}
	pick32 := func(had, was, now *uint32) *uint32 {
		if was == nil && now != nil {
			return now
		}
		return had
	}
	d.ColorPrimaries = pick16(d.ColorPrimaries, stated.ColorPrimaries, t.ColorPrimaries)
	d.ColorTransfer = pick16(d.ColorTransfer, stated.ColorTransfer, t.ColorTransfer)
	d.ColorSpace = pick16(d.ColorSpace, stated.ColorSpace, t.ColorSpace)
	d.ColorRange = pick16(d.ColorRange, stated.ColorRange, t.ColorRange)
	d.DisplayWidth = pick32(d.DisplayWidth, stated.DisplayWidth, t.DisplayWidth)
	d.DisplayHeight = pick32(d.DisplayHeight, stated.DisplayHeight, t.DisplayHeight)
	if d != (mkv.DerivedFields{}) {
		t.Derived = &d
	}
}

// scanFromVideo records what the Video element says about scanning. FlagInterlaced
// (0 undetermined, 1 interlaced, 2 progressive) gives the scan type. FieldOrder
// (nil when absent) gives the order only for interlaced video - the spec says to
// ignore it otherwise - and only when it names an interlaced order: "progressive"
// under an interlaced flag is a contradiction, so the order stays unknown rather
// than invented. Progressive video gets the one order it can have.
func scanFromVideo(t *mkv.Track, flagInterlaced uint64, fieldOrder *uint64) {
	switch flagInterlaced {
	case 1:
		t.ScanType = "interlaced"
		if fieldOrder != nil {
			if o := mkv.FieldOrderName(*fieldOrder); mkv.ScanTypeOf(o) == "interlaced" {
				t.FieldOrder = o
			}
		}
	case 2:
		t.ScanType = "progressive"
		t.FieldOrder = "progressive"
	}
}

// parseProjection reads the Video>Projection element: its ProjectionType as the
// track's Projection name and a flat video's pose roll as its Rotation.
func (p *parser) parseProjection(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var typ *uint64
	var pose [3]float64 // yaw, pitch, roll
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
		case mkv.IDProjectionType:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			typ = &v
			t.Projection = mkv.ProjectionTypeName(v)
		case mkv.IDProjectionPoseYaw, mkv.IDProjectionPosePitch, mkv.IDProjectionPoseRoll:
			v, err := readFloatOf(p.r, eh)
			if err != nil {
				return err
			}
			pose[eh.ID-mkv.IDProjectionPoseYaw] = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	t.Rotation, t.HFlip = mkv.OrientationFromProjection(typ, pose[0], pose[1], pose[2])
	return nil
}

func (p *parser) parseVideoSettings(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	// FlagInterlaced and FieldOrder are resolved together after the loop: the
	// order only counts under an interlaced flag, whichever comes first.
	var flagInterlaced uint64
	var fieldOrder *uint64
	defer func() { scanFromVideo(t, flagInterlaced, fieldOrder) }()
	for {
		p.unhold() // the child kept last was parsed from its copy: back to the source
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
		if err := p.keepUnmodeled(t, mkv.IDVideo, eh); err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDPixelWidth:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.Width = fit32(v)
		case mkv.IDPixelHeight:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.Height = fit32(v)
		case mkv.IDFlagInterlaced:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			flagInterlaced = v
		case mkv.IDFieldOrder:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			fieldOrder = &v
		case mkv.IDDisplayWidth:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			if v > 0 { // 0 or empty: the default, the pixel size
				t.DisplayWidth = fit32(v)
			}
		case mkv.IDDisplayHeight:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			if v > 0 { // 0 or empty: the default, the pixel size
				t.DisplayHeight = fit32(v)
			}
		case mkv.IDColour:
			if err := p.parseColour(eh.Size, t); err != nil {
				return err
			}
		case mkv.IDStereoMode:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			if v != 0 { // 0 = mono: leave nil (no stereo)
				t.StereoMode = fit16(v)
			}
		case mkv.IDProjection:
			if err := p.parseProjection(eh.Size, t); err != nil {
				return err
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseColour reads the Video>Colour element (0x55B0), populating the track's
// CICP colour code points (matrix/transfer/primaries/range) and video bit depth.
// Each field stays nil when its sub-element is absent.
func (p *parser) parseColour(size int64, t *mkv.Track) error {
	t.ColourDetermined = true // the container carries an explicit Colour element
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
		case mkv.IDColourMatrix:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.ColorSpace = fit16(v)
		case mkv.IDColourTransfer:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.ColorTransfer = fit16(v)
		case mkv.IDColourPrimaries:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.ColorPrimaries = fit16(v)
		case mkv.IDColourRange:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.ColorRange = fit16(v)
		case mkv.IDColourBitsPerChannel:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.VideoBitDepth = fit16(v)
			colourLayout(t).BitsPerChannel = fit16(v)
		case mkv.IDChromaSubsamplingHorz, mkv.IDChromaSubsamplingVert, mkv.IDCbSubsamplingHorz, mkv.IDCbSubsamplingVert,
			mkv.IDChromaSitingHorz, mkv.IDChromaSitingVert:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			*colourLayoutField(colourLayout(t), eh.ID) = fit16(v)
		case mkv.IDColourMaxCLL:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			if x := fit32(v); x != nil {
				ensureHDR(t).MaxCLL = *x
			}
		case mkv.IDColourMaxFALL:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			if x := fit32(v); x != nil {
				ensureHDR(t).MaxFALL = *x
			}
		case mkv.IDMasteringMetadata:
			if err := p.parseMasteringMetadata(eh.Size, t); err != nil {
				return err
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureHDR lazily allocates the track's HDR10 static-metadata holder.
func ensureHDR(t *mkv.Track) *mkv.HDRStaticMetadata {
	if t.HDR == nil {
		t.HDR = &mkv.HDRStaticMetadata{}
	}
	return t.HDR
}

// parseMasteringMetadata reads the MasteringMetadata element (SMPTE ST 2086): the
// R/G/B primary and white-point chromaticities and the luminance range, all EBML
// floats already in the units MasteringDisplay stores (chromaticity 0..1, cd/m²).
func (p *parser) parseMasteringMetadata(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	md := &mkv.MasteringDisplay{}
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
		set := func(dst *float64) error {
			v, e := readFloatOf(p.r, eh)
			if e == nil {
				*dst = v
			}
			return e
		}
		switch eh.ID {
		case mkv.IDPrimaryRChromaX:
			err = set(&md.RedX)
		case mkv.IDPrimaryRChromaY:
			err = set(&md.RedY)
		case mkv.IDPrimaryGChromaX:
			err = set(&md.GreenX)
		case mkv.IDPrimaryGChromaY:
			err = set(&md.GreenY)
		case mkv.IDPrimaryBChromaX:
			err = set(&md.BlueX)
		case mkv.IDPrimaryBChromaY:
			err = set(&md.BlueY)
		case mkv.IDWhitePointChromaX:
			err = set(&md.WhiteX)
		case mkv.IDWhitePointChromaY:
			err = set(&md.WhiteY)
		case mkv.IDLuminanceMax:
			err = set(&md.LuminanceMax)
		case mkv.IDLuminanceMin:
			err = set(&md.LuminanceMin)
		default:
			err = p.skip(eh.Size)
		}
		if err != nil {
			return err
		}
	}
	ensureHDR(t).MasteringDisplay = md
	return nil
}

// parseBlockAdditionMapping reads a BlockAdditionMapping (0x41E4). When its
// BlockAddIDType marks Dolby Vision (dvcC/dvvC), the BlockAddIDExtraData holds the
// DOVIDecoderConfigurationRecord, which is decoded onto the track.
func (p *parser) parseBlockAdditionMapping(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var addType uint64
	var extra []byte
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
		case mkv.IDBlockAddIDType:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			addType = v
		case mkv.IDBlockAddIDExtraData:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			extra = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	if addType == mkv.BlockAddIDTypeDVCC || addType == mkv.BlockAddIDTypeDVVC {
		if dv := mkv.ParseDolbyVisionConfig(extra); dv != nil {
			t.DolbyVision = dv
		}
	}
	return nil
}

func (p *parser) parseAudioSettings(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		p.unhold() // the child kept last was parsed from its copy: back to the source
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
		if err := p.keepUnmodeled(t, mkv.IDAudio, eh); err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDSamplingFreq:
			v, err := readFloatOf(p.r, eh)
			if err != nil {
				return err
			}
			if validSampleRate(v) {
				t.SampleRate = &v
			} else {
				p.rateRejected = true
			}
		case mkv.IDOutputSamplingFreq:
			v, err := readFloatOf(p.r, eh)
			if err != nil {
				return err
			}
			if validSampleRate(v) {
				t.OutputSampleRate = &v
			}
		case mkv.IDChannels:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.Channels = fit8(v)
			p.channelsRejected = t.Channels == nil
		case mkv.IDBitDepth:
			v, err := readUintOf(p.r, eh)
			if err != nil {
				return err
			}
			t.BitDepth = fit8(v)
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// colourLayout returns the track's Colour layout, allocating it on first use.
func colourLayout(t *mkv.Track) *mkv.ColourLayout {
	if t.ColourLayout == nil {
		t.ColourLayout = &mkv.ColourLayout{}
	}
	return t.ColourLayout
}

// colourLayoutField is the layout field a subsampling or siting element fills.
func colourLayoutField(l *mkv.ColourLayout, id uint32) **uint16 {
	switch id {
	case mkv.IDChromaSubsamplingHorz:
		return &l.ChromaSubsamplingHorz
	case mkv.IDChromaSubsamplingVert:
		return &l.ChromaSubsamplingVert
	case mkv.IDCbSubsamplingHorz:
		return &l.CbSubsamplingHorz
	case mkv.IDCbSubsamplingVert:
		return &l.CbSubsamplingVert
	case mkv.IDChromaSitingHorz:
		return &l.ChromaSitingHorz
	}
	return &l.ChromaSitingVert
}

// maxUnmodeledChild bounds one kept child: the elements kept are flags and small numbers, a large one is no such.
const maxUnmodeledChild = 4096

// keepUnmodeled records a child of parent the writer has no field for as read, and has the parse that follows read
// it from that copy (unhold puts the source back), so the source is read once, forward.
func (p *parser) keepUnmodeled(t *mkv.Track, parent uint32, eh ebml.ElementHeader) error {
	if _, streamed := p.r.(*bodyReader); streamed || eh.Size < 0 || eh.Size > maxUnmodeledChild || trackchild.Written(parent, eh.ID) {
		return nil // a streamed element cannot be read twice; ReadStream feeds no rewrite
	}
	if err := p.chargeMeta(eh.Size); err != nil {
		return err
	}
	pos, err := p.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	data := make([]byte, eh.Size)
	if _, err := io.ReadFull(p.r, data); err != nil {
		return err
	}
	t.Unmodeled = append(t.Unmodeled, mkv.RawElement{Parent: parent, ID: eh.ID, Data: data})
	p.held, p.r = p.r, &heldChild{Reader: bytes.NewReader(data), base: pos}
	return nil
}

// unhold puts the source back after a child parsed from its kept copy.
func (p *parser) unhold() {
	if p.held != nil {
		p.r, p.held = p.held, nil
	}
}

// heldChild reads a kept child from memory at the file positions it came from.
type heldChild struct {
	*bytes.Reader
	base int64
}

func (h *heldChild) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekStart {
		off -= h.base
	}
	n, err := h.Reader.Seek(off, whence)
	return n + h.base, err
}
