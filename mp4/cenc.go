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
// Clear/protected split (subsample encryption), video (H.264/HEVC, whose MKV
// samples are already length-prefixed NALs, matching AVCC/HVCC): per NAL unit,
// the clear region is the 4-byte length field plus the NAL header (1 byte for
// H.264, 2 for HEVC); everything after is protected. Audio (and any non-NAL
// codec) is encrypted whole-sample, no subsamples. AV1/VP9 are refused for
// CENC in this version - their subsample conventions differ and are not
// implemented.
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
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// CENCOptions configures Common Encryption packaging (Options.CENC).
//
// Key/IV reuse is the caller's responsibility. mkvgo validates the key, key ID
// and IV lengths, not their global uniqueness. Within a single packaging pass
// mkvgo derives a distinct per-sample IV for "cenc" so a track never reuses a
// keystream; "cbcs" uses the scheme-mandated constant IV. But across separate
// encodings the pair (Key, base IV) must be unique: encrypting two different
// files with the same Key and the same base IV under "cenc" makes their
// per-sample IVs - and therefore their AES-CTR keystreams - collide wherever
// the decode timestamps overlap, which leaks plaintext. Use a fresh Key or a
// fresh base IV per encoding.
type CENCOptions struct {
	// Scheme selects the encryption scheme: "cenc" (AES-CTR) or "cbcs"
	// (AES-CBC, 1:9 pattern on video). Required.
	Scheme string
	// Key is the 16-byte AES key. Never written to the output - only KeyID
	// and (optionally) PSSH boxes are.
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
	// EME-capable player's license request ultimately needs to resolve. Left
	// empty, it defaults to a data: URI embedding Key directly - convenient
	// for local testing, but it puts the raw key in the playlist text, so
	// production deployments should always set a real KeyURI (an
	// authenticated endpoint, or the player's license-server URL).
	KeyURI string
	// PSSH holds zero or more complete, caller-built 'pssh' boxes (Protection
	// System Specific Header, one per DRM system) copied verbatim into the
	// init segment's moov. mkvgo does not build these itself - it has no DRM
	// integration - it only carries what the caller supplies. Init segment
	// only in this version (not repeated per fragment).
	PSSH [][]byte
}

// validate checks the scheme/key/IV lengths. Codec compatibility (h264/hevc
// only) is checked once the video track is known - see cencPreflight.
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
	switch c.Scheme {
	case "cenc":
		if len(c.IV) != 8 && len(c.IV) != 16 {
			return errf("cenc IV must be 8 or 16 bytes, got %d", len(c.IV))
		}
	case "cbcs":
		if len(c.IV) != aes.BlockSize {
			return errf("cbcs IV must be %d bytes (a full AES block), got %d", aes.BlockSize, len(c.IV))
		}
	}
	return nil
}

// resolvedKeyURI returns KeyURI, or (unset) a data: URI embedding Key - see
// the KeyURI doc comment for the production caveat.
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
	return fmt.Sprintf("#EXT-X-KEY:METHOD=%s,URI=%q,KEYFORMAT=\"identity\",KEYFORMATVERSIONS=\"1\"\n", method, c.resolvedKeyURI())
}

// cencPreflight validates Options.CENC against the sibling options and the
// selected video codec, nil when CENC is unset. videoCodec is the mkvgo short
// codec name ("h264", "hevc", …) of the presentation's video track, or "" for
// an audio-only presentation.
func cencPreflight(o *Options, videoCodec string) error {
	if o.CENC == nil {
		return nil
	}
	if o.Encrypt != nil {
		return errf("Options.CENC and Options.Encrypt cannot be combined (pick one encryption scheme)")
	}
	if o.SingleFile {
		return errf("Options.CENC and Options.SingleFile cannot be combined (not supported in this version)")
	}
	if err := o.CENC.validate(); err != nil {
		return err
	}
	if videoCodec != "" && videoNALHeaderLen(videoCodec) == 0 {
		return errf("Options.CENC: video codec %q has no subsample encryption rule in this version (h264/hevc only; AV1/VP9 are refused)", videoCodec)
	}
	return nil
}

// videoNALHeaderLen returns the length-prefixed NAL unit header size CENC
// subsample encryption must leave clear (with the 4-byte length field) for a
// video codec, or 0 for a codec CENC does not support (AV1/VP9: their
// subsample conventions differ and are not implemented here).
func videoNALHeaderLen(codec string) int {
	switch codec {
	case "h264":
		return 1
	case "hevc":
		return 2
	default:
		return 0
	}
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

// splitNALSubsamples parses one video sample's length-prefixed NAL units
// (AVCC/HVCC style - exactly how mkvgo's fMP4/MP4 samples are already laid
// out) into their clear (length field + NAL header) / protected (the rest)
// regions.
func splitNALSubsamples(data []byte, nalHeaderLen int) ([]cencSubsample, error) {
	var subs []cencSubsample
	i := 0
	for i < len(data) {
		if i+4 > len(data) {
			return nil, errf("cenc: truncated NAL length prefix in sample")
		}
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		if n < 0 || i+4+n > len(data) {
			return nil, errf("cenc: NAL length %d exceeds sample bounds", n)
		}
		clear := 4 + nalHeaderLen
		if clear > 4+n {
			clear = 4 + n
		}
		protected := 4 + n - clear
		subs = append(subs, cencSubsample{clear: uint16(clear), protected: uint32(protected)})
		i += 4 + n
	}
	return subs, nil
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
func cencSampleIV(base []byte, dtsTS int64) []byte {
	iv := append([]byte(nil), base...)
	addUint64BE(iv[:8], uint64(dtsTS))
	return iv
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
	chain := append([]byte(nil), iv...)
	pos := 0
	for pos < full {
		n := cryptBlocks * aes.BlockSize
		if pos+n > full {
			n = full - pos
		}
		if n > 0 {
			cipher.NewCBCEncrypter(block, chain).CryptBlocks(region[pos:pos+n], region[pos:pos+n])
			chain = append(chain[:0:0], region[pos+n-aes.BlockSize:pos+n]...)
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
func prepareCENCSegment(c *CENCOptions, isVideo bool, codec string, samples []fragSample, data []byte) (*cencTrafData, []byte, error) {
	block, err := aes.NewCipher(c.Key)
	if err != nil {
		return nil, nil, err
	}
	nalHeaderLen := 0
	if isVideo {
		nalHeaderLen = videoNALHeaderLen(codec)
		if nalHeaderLen == 0 {
			return nil, nil, errf("cenc: video codec %q does not support subsample encryption (h264/hevc only)", codec)
		}
	}
	out := append([]byte(nil), data...)
	td := &cencTrafData{hasSubsample: isVideo}
	pos := 0
	for i := range samples {
		size := int(samples[i].size)
		sample := out[pos : pos+size]
		var subs []cencSubsample
		if isVideo {
			subs, err = splitNALSubsamples(sample, nalHeaderLen)
			if err != nil {
				return nil, nil, err
			}
		}
		var ivField []byte
		switch c.Scheme {
		case "cenc":
			ivField = cencSampleIV(c.IV, samples[i].dtsTS)
			cencCTREncrypt(block, cencCounterBlock(ivField), sample, subs)
		default: // cbcs
			if isVideo {
				cbcsEncryptVideoSubsamples(block, c.IV, sample, subs)
			} else {
				cbcsEncryptWholeSample(block, c.IV, sample)
			}
		}
		td.entries = append(td.entries, sencEntry{iv: ivField, subs: subs})
		pos += size
	}
	return td, out, nil
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
