package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWatermarkVariant(t *testing.T) {
	// Explicit --variant.
	if b, _ := watermarkVariant("A", "", 3); b {
		t.Error("variant A must select false (A)")
	}
	if b, _ := watermarkVariant("B", "", 3); !b {
		t.Error("variant B must select true (B)")
	}
	if b, _ := watermarkVariant("", "", 0); b {
		t.Error("default variant must be A")
	}
	if _, err := watermarkVariant("C", "", 0); err == nil {
		t.Error("an invalid variant must error")
	}
	// --pattern hex routes by bit n (LSB-first): 0x02 = bit 1 set.
	if b, _ := watermarkVariant("", "02", 1); !b {
		t.Error("pattern 0x02 bit 1 must select B")
	}
	if b, _ := watermarkVariant("", "02", 0); b {
		t.Error("pattern 0x02 bit 0 must select A")
	}
	if b, _ := watermarkVariant("", "02", 99); b {
		t.Error("pattern bit past the end must default to A")
	}
	if _, err := watermarkVariant("", "zz", 0); err == nil {
		t.Error("a non-hex pattern must error")
	}
}

func TestParseHLSFlagsAESRotation(t *testing.T) {
	f := parseHLSFlags([]string{
		"--aes-rotate-segments", "5",
		"--aes-key", "00112233445566778899aabbccddeeff,ffeeddccbbaa99887766554433221100",
		"--aes-key-uri", "https://k/a,https://k/b",
	})
	if f.encrypt == nil || f.encrypt.RotateEverySegments != 5 || len(f.encrypt.Keys) != 2 {
		t.Fatalf("AES rotation not parsed into Keys: %+v", f.encrypt)
	}
	if f.encrypt.Keys[1].KeyURI != "https://k/b" || len(f.encrypt.Keys[0].Key) != 16 {
		t.Errorf("rotation keys parsed wrong: %+v", f.encrypt.Keys)
	}
	// Single key (no rotation) stays the simple form.
	f2 := parseHLSFlags([]string{"--aes-key", "00112233445566778899aabbccddeeff", "--aes-key-uri", "https://k/x"})
	if f2.encrypt == nil || f2.encrypt.RotateEverySegments != 0 || len(f2.encrypt.Key) != 16 {
		t.Errorf("single AES key parsed wrong: %+v", f2.encrypt)
	}
}

func TestCmdWatermarkSegment(t *testing.T) {
	src := filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv")
	if _, err := os.Stat(src); err != nil {
		t.Skip("sample.mkv missing")
	}
	dir := t.TempDir()
	// a == b: alignment trivially holds; exercises the command's resource paths.
	for _, what := range []string{"master", "playlist", "init", "1"} {
		out := filepath.Join(dir, what+".bin")
		args := []string{src, src, what, "-segment", "0.5", "-o", out}
		if what == "1" {
			args = append(args, "--variant", "B")
		}
		CmdWatermarkSegment(args)
		info, err := os.Stat(out)
		if err != nil || info.Size() == 0 {
			t.Errorf("watermark-segment %s produced no output", what)
		}
	}
}
