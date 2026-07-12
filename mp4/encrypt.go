package mp4

// encrypt.go - HLS AES-128 segment encryption (EXT-X-KEY METHOD=AES-128):
// each media segment is encrypted whole with AES-128-CBC + PKCS#7 padding,
// the IV being the segment's media sequence number (the HLS default when the
// playlist carries no IV attribute), or a fixed caller-supplied IV. The init
// segments and subtitle files stay clear, as players expect. AES-128 is an
// HLS mechanism: when encryption is enabled the DASH manifest is not emitted
// (DASH uses CENC, a different scheme mkvgo does not implement).

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// HLSKey is one AES-128 key and its advertised delivery, used both for the
// single-key case and for each period of a rotating schedule.
type HLSKey struct {
	// Key is the 16-byte AES-128 key. The packager never writes it anywhere -
	// serving it (KeyURI) and access control around it are the server's job.
	Key []byte
	// KeyURI is what the playlists advertise in EXT-X-KEY URI="..." - typically
	// an authenticated endpoint returning the 16 key bytes.
	KeyURI string
	// IV, when set (16 bytes), is used for every segment in this key's periods
	// and advertised as the IV attribute. Leave nil for the spec default: the
	// IV is each segment's media sequence number, and no IV attribute is written.
	IV []byte
}

// HLSEncryption configures AES-128 segment encryption. Set the single Key/
// KeyURI/IV for one key over the whole presentation, or RotateEverySegments
// with Keys to rotate the key across the presentation (forward secrecy: a
// leaked key decrypts only its own period, not the whole video).
type HLSEncryption struct {
	// Single-key case (used when RotateEverySegments == 0).
	Key    []byte
	KeyURI string
	IV     []byte

	// RotateEverySegments, when > 0, changes the key every N media segments,
	// stepping through Keys in order and cycling back to Keys[0] once the last
	// is used. The media playlist then carries a fresh EXT-X-KEY line at each
	// period boundary and each segment is encrypted with its period's key, so a
	// key captured from one period is useless a few segments later. The
	// schedule is a pure function of the segment index, so an on-demand plan and
	// the full write agree byte for byte. Supply as many Keys as you want
	// distinct keys before the cycle repeats.
	RotateEverySegments int
	Keys                []HLSKey
}

// rotating reports whether a rotating key schedule is configured.
func (e *HLSEncryption) rotating() bool {
	return e.RotateEverySegments > 0 && len(e.Keys) > 0
}

// periodKey returns the key governing the seg-th (0-based) media segment.
func (e *HLSEncryption) periodKey(seg int) HLSKey {
	if e.rotating() {
		return e.Keys[(seg/e.RotateEverySegments)%len(e.Keys)]
	}
	return HLSKey{Key: e.Key, KeyURI: e.KeyURI, IV: e.IV}
}

func validateHLSKey(k HLSKey, what string) error {
	if len(k.Key) != aes.BlockSize {
		return errf("%s: AES-128 key must be %d bytes, got %d", what, aes.BlockSize, len(k.Key))
	}
	if k.KeyURI == "" {
		return errf("%s: AES-128 encryption needs a KeyURI for the EXT-X-KEY line", what)
	}
	if k.IV != nil && len(k.IV) != aes.BlockSize {
		return errf("%s: AES-128 IV must be %d bytes, got %d", what, aes.BlockSize, len(k.IV))
	}
	return nil
}

func (e *HLSEncryption) validate() error {
	if e.RotateEverySegments < 0 {
		return errf("AES-128 RotateEverySegments must be >= 0, got %d", e.RotateEverySegments)
	}
	if e.rotating() {
		if len(e.Keys) < 2 {
			return errf("AES-128 key rotation needs at least 2 keys, got %d (use the single Key for one key)", len(e.Keys))
		}
		for i, k := range e.Keys {
			if err := validateHLSKey(k, fmt.Sprintf("AES-128 rotation key %d", i)); err != nil {
				return err
			}
		}
		return nil
	}
	if e.RotateEverySegments > 0 {
		return errf("AES-128 RotateEverySegments is set but Keys is empty")
	}
	return validateHLSKey(HLSKey{Key: e.Key, KeyURI: e.KeyURI, IV: e.IV}, "AES-128")
}

// keyLineForSegment renders the EXT-X-KEY tag governing the seg-th segment.
// buildMediaPlaylist emits it whenever it changes from the previous segment's,
// so a single key prints once and a rotating schedule prints at each boundary.
func (e *HLSEncryption) keyLineForSegment(seg int) string {
	k := e.periodKey(seg)
	line := fmt.Sprintf("#EXT-X-KEY:METHOD=AES-128,URI=%q", k.KeyURI)
	if k.IV != nil {
		line += ",IV=0x" + hex.EncodeToString(k.IV)
	}
	return line + "\n"
}

// segmentIV returns the IV for the seq-th (0-based) segment under key k: the
// explicit per-key IV if set, otherwise the segment's media sequence number.
func segmentIV(k HLSKey, seq uint32) []byte {
	if k.IV != nil {
		return k.IV
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint32(iv[12:], seq)
	return iv
}

// encryptSegment returns the AES-128-CBC + PKCS#7 encryption of one whole
// media segment, using the key of the segment's rotation period.
func (e *HLSEncryption) encryptSegment(data []byte, seq uint32) ([]byte, error) {
	k := e.periodKey(int(seq))
	block, err := aes.NewCipher(k.Key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(data)%aes.BlockSize
	padded := make([]byte, len(data)+pad)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, segmentIV(k, seq)).CryptBlocks(padded, padded)
	return padded, nil
}
