package mp4

// encrypt.go — HLS AES-128 segment encryption (EXT-X-KEY METHOD=AES-128):
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

// HLSEncryption configures AES-128 segment encryption.
type HLSEncryption struct {
	// Key is the 16-byte AES-128 key. The packager never writes it anywhere —
	// serving it (KeyURI) and access control around it are the server's job.
	Key []byte
	// KeyURI is what the playlists advertise in EXT-X-KEY URI="…" — typically
	// an authenticated endpoint returning the 16 key bytes.
	KeyURI string
	// IV, when set (16 bytes), is used for every segment and advertised as the
	// IV attribute. Leave nil for the spec default: the IV is each segment's
	// media sequence number, and no IV attribute is written.
	IV []byte
}

func (e *HLSEncryption) validate() error {
	if len(e.Key) != aes.BlockSize {
		return errf("AES-128 key must be %d bytes, got %d", aes.BlockSize, len(e.Key))
	}
	if e.KeyURI == "" {
		return errf("AES-128 encryption needs a KeyURI for the EXT-X-KEY line")
	}
	if e.IV != nil && len(e.IV) != aes.BlockSize {
		return errf("AES-128 IV must be %d bytes, got %d", aes.BlockSize, len(e.IV))
	}
	return nil
}

// keyLine renders the playlist's EXT-X-KEY tag.
func (e *HLSEncryption) keyLine() string {
	line := fmt.Sprintf("#EXT-X-KEY:METHOD=AES-128,URI=%q", e.KeyURI)
	if e.IV != nil {
		line += ",IV=0x" + hex.EncodeToString(e.IV)
	}
	return line + "\n"
}

// segmentIV returns the IV for the seq-th (0-based) segment.
func (e *HLSEncryption) segmentIV(seq uint32) []byte {
	if e.IV != nil {
		return e.IV
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint32(iv[12:], seq)
	return iv
}

// encryptSegment returns the AES-128-CBC + PKCS#7 encryption of one whole
// media segment.
func (e *HLSEncryption) encryptSegment(data []byte, seq uint32) ([]byte, error) {
	block, err := aes.NewCipher(e.Key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(data)%aes.BlockSize
	padded := make([]byte, len(data)+pad)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, e.segmentIV(seq)).CryptBlocks(padded, padded)
	return padded, nil
}
