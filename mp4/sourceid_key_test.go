package mp4

import (
	"bytes"
	"context"
	"testing"
)

// TestSourceIDChangesWithTheKey: the weak ETag of a streamed segment is its size, the plan's SourceID and its name,
// and SourceID hashed the playlists and inits, which carry the key's URI and ID but not the key: the same source
// packaged under another key, same URI, gave other bytes under one ETag, and a cache served the old ones.
func TestSourceIDChangesWithTheKey(t *testing.T) {
	src := buildCENCFixture(t)
	ctx := context.Background()
	id := func(o Options) string {
		p, err := PlanHLS(ctx, src, o)
		if err != nil {
			t.Fatal(err)
		}
		return p.SourceID()
	}
	other := bytes.Repeat([]byte{0x42}, 16)
	aes := func(key []byte) Options {
		return Options{SegmentMs: 1000, Encrypt: &HLSEncryption{Key: key, KeyURI: "https://k/key"}}
	}
	if id(aes(cencKey)) == id(aes(other)) {
		t.Error("AES-128: two keys under one URI gave one SourceID")
	}
	cenc := func(key []byte) Options {
		return Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: "cenc", Key: key, KeyID: cencKID, IV: cencIV8, KeyURI: "https://k/"}}
	}
	if id(cenc(cencKey)) == id(cenc(other)) {
		t.Error("CENC: two keys under one key ID gave one SourceID")
	}
	if first, again := id(cenc(cencKey)), id(cenc(cencKey)); first != again {
		t.Error("one key gave two SourceIDs")
	}
}
