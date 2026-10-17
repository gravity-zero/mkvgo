package commands_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

const sec06Key = "00112233445566778899aabbccddeeff"

// hlsDir runs to-hls on the sample with args and returns every output file by name.
func hlsDir(t *testing.T, args ...string) map[string][]byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hls")
	commands.CmdToHLS(append([]string{"../../../internal/testdata/sample.mkv", "-o", out}, args...))
	files := map[string][]byte{}
	ents, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(out, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = b
	}
	return files
}

func sameFiles(t *testing.T, a, b map[string][]byte) {
	t.Helper()
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("%d files against %d", len(a), len(b))
	}
	for name, x := range a {
		if !bytes.Equal(x, b[name]) {
			t.Fatalf("%s differs", name)
		}
	}
}

func TestSEC06_AESKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "aes.key")
	if err := os.WriteFile(keyFile, []byte("  0011223344556677\n 8899aabbccddeeff \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := hlsDir(t, "--aes-key", sec06Key, "--aes-key-uri", "https://k/a")
	file := hlsDir(t, "--aes-key-file", keyFile, "--aes-key-uri", "https://k/a")
	sameFiles(t, argv, file)
	t.Setenv("MKVGO_AES_KEY", sec06Key)
	sameFiles(t, argv, hlsDir(t, "--aes-key-uri", "https://k/a"))
}

func TestSEC06_EnvKeyAloneDoesNotEncrypt(t *testing.T) {
	plain := hlsDir(t)
	t.Setenv("MKVGO_AES_KEY", sec06Key)
	t.Setenv("MKVGO_CENC_KEY", sec06Key)
	sameFiles(t, plain, hlsDir(t))
}

func TestSEC06_CENCKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "cenc.key")
	if err := os.WriteFile(keyFile, []byte(sec06Key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := []string{"--cenc-scheme", "cenc", "--cenc-kid", "ffeeddccbbaa99887766554433221100", "--cenc-iv", "0102030405060708", "--cenc-key-uri", "skd://k"}
	argv := hlsDir(t, append([]string{"--cenc-key", sec06Key}, common...)...)
	sameFiles(t, argv, hlsDir(t, append([]string{"--cenc-key-file", keyFile}, common...)...))
	t.Setenv("MKVGO_CENC_KEY", sec06Key)
	sameFiles(t, argv, hlsDir(t, common...))
}

// fatalMessage runs fn, which must call Fatal, and returns what it wrote to stderr.
func fatalMessage(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := -1
	restore := commands.SetExit(func(c int) { code = c; panic("exit") })
	func() {
		defer func() { _ = recover() }()
		fn()
	}()
	restore()
	os.Stderr = old
	w.Close()
	msg, _ := io.ReadAll(r)
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (stderr %q)", code, msg)
	}
	return string(msg)
}

func TestSEC07_CENCNeedsKeyURI(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hls")
	msg := fatalMessage(t, func() {
		commands.CmdToHLS([]string{"../../../internal/testdata/sample.mkv", "-o", out, "--cenc-scheme", "cenc", "--cenc-key", sec06Key, "--cenc-kid", "ffeeddccbbaa99887766554433221100", "--cenc-iv", "0102030405060708"})
	})
	if !strings.Contains(msg, "--cenc-key-uri") || !strings.Contains(msg, "--cenc-inline-key") {
		t.Fatalf("the refusal does not say what to do: %q", msg)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("output written before the refusal")
	}
	inline := hlsDir(t, "--cenc-scheme", "cenc", "--cenc-key", sec06Key, "--cenc-kid", "ffeeddccbbaa99887766554433221100", "--cenc-iv", "0102030405060708", "--cenc-inline-key")
	for _, b := range inline {
		if bytes.Contains(b, []byte(`URI="data:`)) {
			return
		}
	}
	t.Fatal("--cenc-inline-key did not inline the key")
}
