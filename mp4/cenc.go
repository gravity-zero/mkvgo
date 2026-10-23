package mp4

// cenc.go - Common Encryption (CENC, ISO/IEC 23001-7) packaging for the
// fMP4/HLS/DASH pipeline: sample-level encryption with caller-supplied keys,
// for either scheme mp4 knows how to build:
//
//   - "cenc" - AES-CTR, a per-sample Initialization Vector, no pattern (every
//     protected byte is encrypted).
//   - "cbcs" - AES-CBC, a constant IV, and (video only) a 1-encrypted:9-clear
//     16-byte-block pattern within each protected region.
//
// This is packaging only: no license server, no DRM handshake. The caller
// owns the key and its delivery (KeyURI / a real license server for a
// EME-capable player); mkvgo only produces spec-correct boxes and ciphertext.
//
// Clear/protected split (subsample encryption), video: for H.264/HEVC (MKV
// samples are length-prefixed NALs, matching AVCC/HVCC) a NAL unit other than
// a coded slice stays clear, and a slice keeps its length field, NAL header
// and slice header clear - see cenc_slice.go. AV1 (see
// cenc_av1.go) and VP9 (cenc_vp9.go) follow their own codec conventions and
// handle the tractable frame structures, failing loud on the cases that need a
// stateful parser rather than mis-protecting. Audio (and any non-NAL codec) is
// encrypted whole-sample, no subsamples.
//
// IV derivation (the part that must hold plan ↔ full-pass byte parity, see
// hlsplan.go's doc comment): the per-sample 'cenc' IV is the caller's base IV
// plus the sample's dtsTS - its absolute decode timestamp in the track's
// media timescale, rebased to 0 at the track's first sample. dtsTS is exactly
// the value fillFragTiming (full pass) and segmentTrack/mp4SegmentTrack (the
// on-demand plan) already compute identically for the trun - that identity is
// what makes the derived ciphertext byte-identical between the two paths
// without needing a literal running sample counter (which the on-demand plan
// cannot derive without walking every prior segment). 'cbcs' has no per-sample
// IV at all - every sample reuses the track's constant IV (from tenc), so its
// ciphertext trivially agrees between the two paths.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
)

// CENCOptions configures Common Encryption packaging (Options.CENC).
//
// Key/IV reuse is the caller's responsibility. mkvgo validates the key, key ID
// and IV lengths, not their global uniqueness. Within a single packaging pass
// mkvgo derives a distinct per-sample IV for "cenc" so no two samples share a
// keystream, across the tracks of a presentation and the variants of an ABR
// one; "cbcs" uses the scheme-mandated constant IV. But across separate
// encodings the pair (Key, base IV) must be unique: encrypting two different
// files with the same Key and the same base IV under "cenc" makes their
// per-sample IVs - and therefore their AES-CTR keystreams - collide wherever
// the decode timestamps overlap, which leaks plaintext. Use a fresh Key or a
// fresh base IV per encoding.
type CENCOptions struct {
	// Scheme selects the encryption scheme: "cenc" (AES-CTR) or "cbcs"
	// (AES-CBC, 1:9 pattern on video). Required.
	Scheme string
	// Key is the 16-byte AES key. The media carry only KeyID and the PSSH
	// boxes; the key itself reaches the output only through InlineKey.
	Key []byte
	// KeyID is the 16-byte key identifier (tenc's default_KID / the DASH
	// ContentProtection cenc:default_KID).
	KeyID []byte
	// IV is the base Initialization Vector. "cenc" accepts 8 or 16 bytes (the
	// per-sample IV size written to tenc/senc); "cbcs" requires 16 (a full
	// AES block - its IV is used directly as CBC's IV, which must be
	// block-sized).
	IV []byte
	// KeyURI is what the HLS EXT-X-KEY line advertises as URI="…" and what an
	// EME-capable player's license request ultimately needs to resolve (an
	// authenticated endpoint, or the player's license-server URL). Required
	// unless InlineKey is set.
	KeyURI string
	// InlineKey, with KeyURI empty, advertises a data: URI embedding Key: the
	// raw key in the playlist text, for local testing only.
	InlineKey bool
	// PSSH holds zero or more complete, caller-built 'pssh' boxes (Protection
	// System Specific Header, one per DRM system) copied verbatim into the
	// init segment's moov. mkvgo does not build these itself - it has no DRM
	// integration - it only carries what the caller supplies. Init segment
	// only in this version (not repeated per fragment).
	PSSH [][]byte
}

// validate checks the scheme/key/IV lengths. Codec compatibility (h264, hevc,
// av1, vp9) is checked once the video track is known - see cencPreflight.
func (c *CENCOptions) validate() error {
	switch c.Scheme {
	case "cenc", "cbcs":
	default:
		return errf("CENC scheme must be \"cenc\" or \"cbcs\", got %q", c.Scheme)
	}
	if len(c.Key) != aes.BlockSize {
		return errf("CENC key must be %d bytes, got %d", aes.BlockSize, len(c.Key))
	}
	if len(c.KeyID) != 16 {
		return errf("CENC key ID must be 16 bytes, got %d", len(c.KeyID))
	}
	if c.KeyURI == "" && !c.InlineKey {
		return errf("CENC needs a KeyURI for the EXT-X-KEY line: without it the playlists would carry the content key in clear. Set CENCOptions.KeyURI to your key or license server, or CENCOptions.InlineKey to embed the key on purpose (testing only)")
	}
	switch c.Scheme {
	case "cenc":
		if len(c.IV) != 8 && len(c.IV) != 16 {
			return errf("cenc IV must be 8 or 16 bytes, got %d", len(c.IV))
		}
		// AES-CTR counts a sample's blocks in a 16-byte IV's last 8 bytes: a count carrying out of them would reach
		// the next sample's IV. 2^32 blocks of room covers any sample a segment holds (maxSegmentBytes).
		if len(c.IV) == 16 && binary.BigEndian.Uint64(c.IV[8:]) > math.MaxUint64-1<<32+1 {
			return errf("cenc 16-byte IV: its last 8 bytes leave the AES-CTR block counter too little room before it carries into the sample number; use an IV whose last 8 bytes are at most ffffffff00000000")
		}
	case "cbcs":
		if len(c.IV) != aes.BlockSize {
			return errf("cbcs IV must be %d bytes (a full AES block), got %d", aes.BlockSize, len(c.IV))
		}
	}
	return nil
}

// resolvedKeyURI returns KeyURI, or (unset, InlineKey) a data: URI embedding Key.
func (c *CENCOptions) resolvedKeyURI() string {
	if c.KeyURI != "" {
		return c.KeyURI
	}
	return "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(c.Key)
}

// keyLine renders the HLS EXT-X-KEY tag for this scheme: METHOD=SAMPLE-AES
// for cbcs, METHOD=SAMPLE-AES-CTR for cenc; KEYFORMAT identity is the form an
// EME-capable player resolves through its own DRM CDM path.
func (c *CENCOptions) keyLine() string {
	method := "SAMPLE-AES-CTR"
	if c.Scheme == "cbcs" {
		method = "SAMPLE-AES"
	}
	return fmt.Sprintf("#EXT-X-KEY:METHOD=%s,URI=%s,KEYFORMAT=\"identity\",KEYFORMATVERSIONS=\"1\"\n", method, hlsURI(c.resolvedKeyURI()))
}

// cencPreflight validates Options.CENC against the sibling options and the
// selected video codec, nil when CENC is unset. videoCodec is the mkvgo short
// codec name ("h264", "hevc", …) of the presentation's video track, or "" for
// an audio-only presentation.
func cencPreflight(o *Options, media []*outTrack) error {
	if o.CENC == nil {
		return nil
	}
	videoCodec := videoCodecOf(media)
	if o.Encrypt != nil {
		return errf("Options.CENC and Options.Encrypt cannot be combined (pick one encryption scheme)")
	}
	if o.SingleFile {
		return errf("Options.CENC and Options.SingleFile cannot be combined (not supported in this version)")
	}
	if err := o.CENC.validate(); err != nil {
		return err
	}
	if videoCodec != "" && !cencSupportsCodec(videoCodec) {
		return errf("Options.CENC: video codec %q has no subsample encryption rule in this version (h264, hevc, av1, vp9)", videoCodec)
	}
	if o.CENC.Scheme == "cenc" {
		owner := map[uint64]uint64{}
		for _, t := range media {
			d := cencIVDomain(t.spec.video, t.mkv.ID)
			if id, ok := owner[d]; ok {
				return errf("Options.CENC: tracks %d and %d would share their sample IVs under the key; keep one of them (Options.KeepTracks)", id, t.mkv.ID)
			}
			owner[d] = t.mkv.ID
		}
	}
	return nil
}

// cencSupportsCodec reports whether CENC subsample encryption has a rule for a
// video codec. h264/hevc are complete; av1/vp9 handle the tractable frame
// structures (AV1 split OBU_FRAME_HEADER+OBU_TILE_GROUP; VP9 keyframes and
// intra-resolvable frames) and fail loud on the rest (combined OBU_FRAME, VP9
// inter frames that reuse a reference frame's size) rather than mis-protect.
func cencSupportsCodec(codec string) bool {
	switch codec {
	case "h264", "hevc", "av1", "vp9":
		return true
	}
	return false
}

// videoSubsampleSplitter computes the clear/protected subsample layout for the
// successive video samples of ONE segment, in decode order. It is created per
// segment (newVideoSubsampleSplitter) so a codec that needs cross-sample state
// - VP9's reference-frame dimensions, AV1's active sequence header - can carry
// it: the segment opens on a keyframe, so that state is self-contained within
// the segment. H.264/HEVC carry their parameter sets, which slice headers are
// read against.
type videoSubsampleSplitter interface {
	split(sample []byte) ([]cencSubsample, error)
}

func newVideoSubsampleSplitter(codec string, private []byte, scheme string) (videoSubsampleSplitter, error) {
	switch codec {
	case "h264", "hevc":
		return newH26xSplitter(codec, private, scheme)
	case "av1":
		return &av1Splitter{}, nil
	case "vp9":
		return &vp9Splitter{}, nil
	}
	return nil, errf("cenc: video codec %q not supported for subsample encryption", codec)
}

// --- init segment: encv/enca sample entry + sinf (frma/schm/schi>tenc) -----

// wrapProtectedSampleEntry renames a plain sample entry box (avc1/hvc1/mp4a/…)
// to its protected form (encv for video, enca for audio) and appends the CENC
// 'sinf' box as an extra child, per ISO/IEC 23001-7 §3. original is always a
// small-form box (size fits 32 bits - true of every sample entry mkvgo
// builds, at most a few hundred bytes), so no 64-bit largesize case exists.
func wrapProtectedSampleEntry(original []byte, isVideo bool, c *CENCOptions) []byte {
	origType := string(original[4:8])
	payload := original[8:]
	sinf := buildSinf(origType, isVideo, c)
	newType := "enca"
	if isVideo {
		newType = "encv"
	}
	body := make([]byte, 0, len(payload)+len(sinf))
	body = append(body, payload...)
	body = append(body, sinf...)
	return box(newType, body)
}

// buildSinf assembles the Protection Scheme Information Box: frma (the
// original sample entry fourcc), schm (scheme type/version) and schi>tenc
// (the track's default protection parameters).
func buildSinf(origType string, isVideo bool, c *CENCOptions) []byte {
	frma := box("frma", []byte(origType))
	schm := fullBox("schm", 0, 0, func(w *bw) {
		w.fourcc(c.Scheme)
		w.u32(0x00010000) // scheme_version 1.0
	})
	schi := container("schi", buildTenc(isVideo, c))
	return container("sinf", frma, schm, schi)
}

// buildTenc builds the Track Encryption Box. cenc uses version 0 (no
// pattern; Per_Sample_IV_Size = len(IV), the per-sample IV rides in senc).
// cbcs uses version 1 (Per_Sample_IV_Size = 0, a constant IV instead) with
// the video pattern 1 crypt : 9 skip; the audio track's own tenc (each
// track's sinf/tenc is independent, embedded in its own sample entry) carries
// 0:0 - no pattern, whole sample encrypted - per CMAF's audio rule.
func buildTenc(isVideo bool, c *CENCOptions) []byte {
	if c.Scheme == "cenc" {
		return fullBox("tenc", 0, 0, func(w *bw) {
			w.u8(0) // reserved
			w.u8(0) // reserved (version 0 has no crypt/skip fields)
			w.u8(1) // default_isProtected
			w.u8(uint8(len(c.IV)))
			w.bytes(c.KeyID)
		})
	}
	var cryptBlk, skipBlk byte
	if isVideo {
		cryptBlk, skipBlk = 1, 9
	}
	return fullBox("tenc", 1, 0, func(w *bw) {
		w.u8(0) // reserved
		w.u8(cryptBlk<<4 | skipBlk)
		w.u8(1) // default_isProtected
		w.u8(0) // default_Per_Sample_IV_Size = 0 (constant IV below)
		w.bytes(c.KeyID)
		w.u8(uint8(len(c.IV))) // default_constant_IV_size
		w.bytes(c.IV)
	})
}

// buildPsshBoxes returns the caller's raw pssh boxes verbatim, ready to append
// into the init segment's moov.
func buildPsshBoxes(c *CENCOptions) [][]byte {
	if c == nil {
		return nil
	}
	return c.PSSH
}

// --- fragments: senc / saiz / saio, subsample split, encryption ------------

// cencSubsample is one NAL unit's clear/protected split within a video
// sample, per ISO/IEC 23001-7's SubSampleEncryption entry.
type cencSubsample struct {
	clear     uint16
	protected uint32
}

// sencEntry is one sample's CENC auxiliary information: its per-sample IV
// (empty for cbcs - Per_Sample_IV_Size 0, the constant IV in tenc applies)
// and its subsample list (nil for audio / non-NAL codecs - whole sample).
type sencEntry struct {
	iv   []byte
	subs []cencSubsample
}

// cencTrafData is one track-fragment's worth of senc/saiz data, built from
// the segment's plaintext samples before the segment's moof is framed - the
// same "know the size before the size is needed" two-pass principle buildMoof
// already applies to trun's data_offset.
type cencTrafData struct {
	hasSubsample bool
	entries      []sencEntry
}

// sencAuxInfoHeaderLen is senc's FullBox header up to (and including)
// sample_count: size(4) + type(4) + version(1) + flags(3) + sample_count(4).
const sencAuxInfoHeaderLen = 16

// smallBoxHeaderLen is the 8-byte box header (size(4)+type(4)) every box this
// package frames uses in practice (segments are seconds of media, nowhere
// near the 4GB threshold that would force the 64-bit largesize form).
const smallBoxHeaderLen = 8

// buildSenc renders the SampleEncryptionBox: per sample, its IV (if any) and
// (video) its subsample list. Flags bit 0x2 signals the subsample structure.
func buildSenc(td *cencTrafData) []byte {
	var flags uint32
	if td.hasSubsample {
		flags = 0x000002
	}
	return fullBox("senc", 0, flags, func(w *bw) {
		w.u32(uint32(len(td.entries)))
		for _, e := range td.entries {
			w.bytes(e.iv)
			if td.hasSubsample {
				w.u16(uint16(len(e.subs)))
				for _, s := range e.subs {
					w.u16(s.clear)
					w.u32(s.protected)
				}
			}
		}
	})
}

// buildSaiz renders the SampleAuxiliaryInformationSizesBox: the byte size of
// each sample's auxiliary info in senc (explicit per-sample sizes, since the
// IV/subsample-count mix makes them non-uniform in general).
func buildSaiz(td *cencTrafData) []byte {
	return fullBox("saiz", 0, 0, func(w *bw) {
		w.u8(0) // default_sample_info_size = 0: explicit sizes follow
		w.u32(uint32(len(td.entries)))
		for _, e := range td.entries {
			size := len(e.iv)
			if td.hasSubsample {
				size += 2 + len(e.subs)*6 // subsample_count(2) + (clear(2)+protected(4)) each
			}
			w.u8(uint8(size))
		}
	})
}

// buildSaio renders the SampleAuxiliaryInformationOffsetsBox: one entry, the
// byte offset (from the moof box's own first byte, matching trun's
// default-base-is-moof data_offset convention) of the first byte of senc's
// auxiliary data for sample 0 - the first IV byte when the track carries
// per-sample IVs (cenc), or the first subsample_count field otherwise (cbcs
// video, whose senc carries no IV at all - see buildTenc).
func buildSaio(offset int64) []byte {
	return fullBox("saio", 0, 0, func(w *bw) {
		w.u32(1) // entry_count
		w.u32(uint32(offset))
	})
}

// sencAuxOffset returns the byte offset (from the moof box's first byte) of
// the first byte of aux info for sample 0 inside the senc this traf will
// hold, given trafStart (the offset of this traf's own box, header
// included) and the sibling boxes (tfhd/tfdt/trun) that precede senc within
// it, in build order.
func sencAuxOffset(trafStart int64, tfhd, tfdt, trun []byte) int64 {
	return trafStart + smallBoxHeaderLen + int64(len(tfhd)+len(tfdt)+len(trun)) + sencAuxInfoHeaderLen
}

// addUint64BE adds v to the big-endian unsigned integer held in b, in place,
// carrying into the higher-order bytes (silently wrapping past b's width -
// never reached by a real presentation's sample count).
func addUint64BE(b []byte, v uint64) {
	carry := v
	for i := len(b) - 1; i >= 0 && carry != 0; i-- {
		sum := uint64(b[i]) + carry&0xff
		b[i] = byte(sum)
		carry = carry>>8 + sum>>8
	}
}

// cencSampleIV returns the scheme-'cenc' per-sample Initialization Vector:
// base (the caller's IV, 8 or 16 bytes) plus dtsTS - see this file's doc
// comment for why dtsTS (rather than a literal enumerated counter) is what
// keeps the plan and the full pass byte-identical.
//
// dtsTS is added to the HIGH-order 8 bytes only. For a 16-byte IV the low 8
// bytes are the AES-CTR block counter (incremented once per 16-byte block
// within a sample): perturbing them per sample would let a large sample's
// block counter run into the next sample's starting counter and reuse the
// keystream - a catastrophic CTR failure (e.g. a 50 KB keyframe spans ~3125
// blocks while dtsTS advances by far less between frames). Varying only the
// high 8 bytes keeps every sample's counter range disjoint. For an 8-byte IV
// the whole IV IS the high 8 bytes (the counter block's low 8 are zero), so
// the same code is correct and unchanged there.
//
// domain sets the track apart: every track shares the key, so two tracks'
// samples at the same dtsTS would otherwise share an IV and a keystream.
func cencSampleIV(base []byte, dtsTS int64, domain uint64) []byte {
	iv := append([]byte(nil), base...)
	addUint64BE(iv[:8], uint64(dtsTS)+domain)
	return iv
}

// maxCENCVariants is how many ABR variants share a 'cenc' key: variant i's IVs start at i<<48 (cencForVariant), and
// the 257th would reach 2^56, the first variant's track domains (cencIVDomain).
const maxCENCVariants = 256

// cencVariantsFit refuses an ABR ladder of n variants that would reuse a 'cenc' IV under the shared key.
func cencVariantsFit(c *CENCOptions, n int) error {
	if c == nil || c.Scheme != "cenc" || n <= maxCENCVariants {
		return nil
	}
	return errf("Options.CENC: %d variants, but at most %d variants share a 'cenc' key without reusing its IVs; package the ladder in parts under different keys", n, maxCENCVariants)
}

// cencForVariant is c for ABR variant i (0-based): 'cenc' variants share the
// key, so each past the first takes the base IV plus i·2^48, below the track
// domains (cencIVDomain) and past any dtsTS a variant reaches; the first is c.
func cencForVariant(c *CENCOptions, i int) *CENCOptions {
	if c == nil || c.Scheme != "cenc" || i == 0 {
		return c
	}
	v := *c
	v.IV = append([]byte(nil), c.IV...)
	addUint64BE(v.IV[:8], uint64(i)<<48)
	return &v
}

// cencIVDomain is what a track adds to its samples' IVs: 0 for the video, so
// its IVs are what they always were, and for any other track its ID's own
// value in the top byte, never 0, past any dtsTS a presentation reaches (2^56).
func cencIVDomain(isVideo bool, trackID uint64) uint64 {
	if isVideo {
		return 0
	}
	return (trackID%255 + 1) << 56
}

// cencCounterBlock expands a 'cenc' per-sample IV to the 16-byte AES-CTR
// counter block: used directly when 16 bytes, or as the high-order 8 bytes
// with a zero low-order (per-sample) block counter otherwise - the standard
// CENC 8-byte-IV construction, and exactly what crypto/cipher's CTR stream
// then increments as a plain 128-bit big-endian counter per 16-byte block.
func cencCounterBlock(sampleIV []byte) []byte {
	if len(sampleIV) == aes.BlockSize {
		return sampleIV
	}
	block := make([]byte, aes.BlockSize)
	copy(block, sampleIV)
	return block
}

// cencCTREncrypt XORs the keystream over sample's protected bytes only,
// running the CTR counter continuously across every subsample's protected
// region in order - the standard 'cenc' subsample rule ("the protected
// bytes of the sample form one continuous stream for CTR purposes",
// regardless of the clear bytes interleaved between them). subs nil means
// "no subsamples": the whole sample is protected (audio).
func cencCTREncrypt(block cipher.Block, counterBlock []byte, sample []byte, subs []cencSubsample) {
	stream := cipher.NewCTR(block, counterBlock)
	if subs == nil {
		stream.XORKeyStream(sample, sample)
		return
	}
	pos := 0
	for _, s := range subs {
		pos += int(s.clear)
		if s.protected > 0 {
			stream.XORKeyStream(sample[pos:pos+int(s.protected)], sample[pos:pos+int(s.protected)])
			pos += int(s.protected)
		}
	}
}

// cbcsPatternEncrypt CBC-encrypts region in 16-byte blocks under the
// crypt:skip pattern: cryptBlocks are encrypted (their CBC chain carried
// across crypt runs, skip runs excluded from it - the encrypted blocks
// behave as one continuous CBC stream with the skip blocks transparently
// passed through in the clear), then skipBlocks are left untouched, and so
// on; any trailing partial block (< 16 bytes) is left clear (CBC needs whole
// blocks). chain starts at iv - cbcs resets CBC state at the start of every
// region this is called on (once per subsample for video, once for a whole
// audio sample), never carrying it across NAL boundaries within a sample.
func cbcsPatternEncrypt(block cipher.Block, iv []byte, region []byte, cryptBlocks, skipBlocks int) {
	full := len(region) / aes.BlockSize * aes.BlockSize
	var chain [aes.BlockSize]byte
	copy(chain[:], iv)
	pos := 0
	for pos < full {
		n := cryptBlocks * aes.BlockSize
		if pos+n > full {
			n = full - pos
		}
		if n > 0 {
			// CBC by hand, block after block: a crypt run of the 1:9 pattern is one block, an encrypter per run is an allocation per block.
			for b := pos; b < pos+n; b += aes.BlockSize {
				blk := region[b : b+aes.BlockSize]
				subtle.XORBytes(blk, blk, chain[:])
				block.Encrypt(blk, blk)
				copy(chain[:], blk)
			}
			pos += n
		}
		if skipBlocks <= 0 {
			if n == 0 {
				break
			}
			continue
		}
		skip := skipBlocks * aes.BlockSize
		if pos+skip > full {
			skip = full - pos
		}
		pos += skip
	}
}

// cbcsEncryptVideoSubsamples applies the 1:9 crypt:skip pattern to each of
// the sample's protected (NAL payload) regions independently.
func cbcsEncryptVideoSubsamples(block cipher.Block, iv []byte, sample []byte, subs []cencSubsample) {
	pos := 0
	for _, s := range subs {
		pos += int(s.clear)
		if s.protected > 0 {
			cbcsPatternEncrypt(block, iv, sample[pos:pos+int(s.protected)], 1, 9)
			pos += int(s.protected)
		}
	}
}

// cbcsEncryptWholeSample encrypts every full 16-byte block of sample with no
// pattern (skip = 0, so cbcsPatternEncrypt's single crypt run covers the
// whole sample as continuous CBC) - CMAF's audio rule; any trailing partial
// block stays clear.
func cbcsEncryptWholeSample(block cipher.Block, iv []byte, sample []byte) {
	cbcsPatternEncrypt(block, iv, sample, len(sample)/aes.BlockSize, 0)
}

// prepareCENCSegment encrypts one track's segment in place (over a copy of
// data) and returns the traf's CENC auxiliary data alongside the ciphertext.
// data holds every sample in samples back-to-back, in decode order, exactly
// as segmentWindow/segmentTrack lay them out; samples' dtsTS values are what
// the per-sample 'cenc' IV derives from (see this file's doc comment).
func prepareCENCSegment(c *CENCOptions, isVideo bool, codec string, private []byte, trackID uint64, samples []fragSample, data []byte) (*cencTrafData, []byte, error) {
	k, err := newCENCCipher(c, isVideo, codec, private, trackID)
	if err != nil {
		return nil, nil, err
	}
	out := append([]byte(nil), data...)
	td := &cencTrafData{hasSubsample: isVideo}
	pos := 0
	for i := range samples {
		size := int(samples[i].size)
		sample := out[pos : pos+size]
		e, err := k.sampleEntry(sample, samples[i])
		if err != nil {
			return nil, nil, err
		}
		k.encrypt(sample, e)
		td.entries = append(td.entries, e)
		pos += size
	}
	return td, out, nil
}

// cencCipher encrypts one track's samples: the key's cipher and, for video,
// the codec's subsample splitter. Every sample is encrypted on its own, so a
// segment can be encrypted one sample at a time.
type cencCipher struct {
	c        *CENCOptions
	block    cipher.Block
	isVideo  bool
	domain   uint64 // added to every 'cenc' IV of the track (cencIVDomain)
	splitter videoSubsampleSplitter
	trackID  uint64
	lastDTS  int64 // the previous sample's decode time, past the first ('cenc' IVs must not repeat)
	started  bool
}

func newCENCCipher(c *CENCOptions, isVideo bool, codec string, private []byte, trackID uint64) (*cencCipher, error) {
	block, err := aes.NewCipher(c.Key)
	if err != nil {
		return nil, err
	}
	if isVideo && !cencSupportsCodec(codec) {
		return nil, errf("cenc: video codec %q does not support subsample encryption (h264, hevc, av1, vp9)", codec)
	}
	k := &cencCipher{c: c, block: block, isVideo: isVideo, domain: cencIVDomain(isVideo, trackID), trackID: trackID}
	if isVideo {
		if k.splitter, err = newVideoSubsampleSplitter(codec, private, c.Scheme); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// sampleEntry is entry for a sample of a segment, refusing one whose 'cenc' IV the next segment repeats.
func (k *cencCipher) sampleEntry(sample []byte, s fragSample) (sencEntry, error) {
	if s.reachesNext && k.c.Scheme == "cenc" {
		return sencEntry{}, errf("Options.CENC: track %d has samples decoded at the same time on both sides of a segment boundary, which the 'cenc' scheme would encrypt with one IV; package it with the cbcs scheme", k.trackID)
	}
	return k.entry(sample, s.dtsTS)
}

// entry is a plaintext sample's senc entry: its IV ('cenc') and, for video, its clear/protected subsamples.
func (k *cencCipher) entry(sample []byte, dtsTS int64) (sencEntry, error) {
	var e sencEntry
	if k.isVideo {
		subs, err := k.splitter.split(sample)
		if err != nil {
			return e, err
		}
		e.subs = subs
	}
	if k.c.Scheme == "cenc" {
		if dtsTS < 0 || dtsTS >= 1<<48 {
			// The track domains (2^56 up) and the variant offsets (2^48 up) stay apart only below 2^48 ticks.
			return e, errf("Options.CENC: track %d has a decode time of %d ticks, past the 2^48 the 'cenc' IV domains keep apart; package it with the cbcs scheme", k.trackID, dtsTS)
		}
		if k.started && dtsTS <= k.lastDTS {
			return e, errf("Options.CENC: track %d has two samples decoded at the same time, which the 'cenc' scheme would encrypt with one IV; package it with the cbcs scheme", k.trackID)
		}
		k.lastDTS, k.started = dtsTS, true
		e.iv = cencSampleIV(k.c.IV, dtsTS, k.domain)
	}
	if k.isVideo {
		subs, err := fitSaiz(e.subs, len(e.iv))
		if err != nil {
			return e, errf("Options.CENC: track %d: %w", k.trackID, err)
		}
		e.subs = subs
	}
	return e, nil
}

// fitSaiz folds the subsamples protecting least into the next one's clear bytes until the entry's size fits saiz's one byte.
func fitSaiz(subs []cencSubsample, ivLen int) ([]cencSubsample, error) {
	maxSubs := (255 - ivLen - 2) / 6
	for len(subs) > maxSubs {
		best := -1
		for i := 0; i+1 < len(subs); i++ {
			if int(subs[i+1].clear)+int(subs[i].clear)+int(subs[i].protected) > 0xFFFF {
				continue
			}
			if best < 0 || subs[i].protected < subs[best].protected {
				best = i
			}
		}
		if best < 0 {
			return nil, errf("a picture of %d NAL units too large to describe in one CENC entry: re-encode it with fewer slices", len(subs))
		}
		subs[best+1].clear += subs[best].clear + uint16(subs[best].protected)
		subs = append(subs[:best], subs[best+1:]...)
	}
	return subs, nil
}

// encrypt encrypts a plaintext sample in place as its entry says.
func (k *cencCipher) encrypt(sample []byte, e sencEntry) {
	switch k.c.Scheme {
	case "cenc":
		cencCTREncrypt(k.block, cencCounterBlock(e.iv), sample, e.subs)
	default: // cbcs
		if k.isVideo {
			cbcsEncryptVideoSubsamples(k.block, k.c.IV, sample, e.subs)
		} else {
			cbcsEncryptWholeSample(k.block, k.c.IV, sample)
		}
	}
}

// --- HLS/DASH signaling ------------------------------------------------------

// formatKeyIDUUID renders a 16-byte key ID as an RFC 4122 UUID string, the
// form DASH's cenc:default_KID expects.
func formatKeyIDUUID(kid []byte) string {
	h := hex.EncodeToString(kid)
	if len(h) != 32 {
		return h
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// cencContentProtection renders the DASH ContentProtection element an
// AdaptationSet needs to signal CENC (mp4protection:2011, the scheme-neutral
// descriptor every DASH/CENC player recognises, carrying the default key ID).
func cencContentProtection(c *CENCOptions) string {
	return fmt.Sprintf("      <ContentProtection schemeIdUri=\"urn:mpeg:dash:mp4protection:2011\" value=%q cenc:default_KID=%q/>\n",
		c.Scheme, formatKeyIDUUID(c.KeyID))
}
