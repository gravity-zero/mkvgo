package ops

import "testing"

// FuzzParseVorbisBlocks pins that any pair of headers is read without a panic into at most 64 modes, block sizes a
// decoder accepts, and a mode field wide enough for every mode.
func FuzzParseVorbisBlocks(f *testing.F) {
	ident := append([]byte("\x01vorbis\x00\x00\x00\x00\x01\x44\xac\x00\x00"), make([]byte, 12)...)
	ident = append(ident, 0xB8, 0x01)
	f.Add(ident, vorbisSetupTail([]bool{false, true}))
	f.Add(ident, vorbisSetupTail([]bool{true}))
	f.Add(ident, []byte("\x05vorbis-setup-header"))
	f.Fuzz(func(t *testing.T, ident, setup []byte) {
		vb, ok := parseVorbisBlocks(ident, setup)
		if !ok {
			return
		}
		if n := len(vb.long); n == 0 || n > 64 || 1<<vb.modeBits < n || vb.size[0] < 64 || vb.size[1] > 8192 || vb.size[0] > vb.size[1] {
			t.Fatalf("modes %d (%d bits), blocks %v", len(vb.long), vb.modeBits, vb.size)
		}
		for b := range 256 {
			if s := vb.blockSize([]byte{byte(b)}); s != 0 && s != vb.size[0] && s != vb.size[1] {
				t.Fatalf("packet %#x: block %d", b, s)
			}
		}
	})
}
