package commands_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// TestCmdServe_UsageError checks a missing source path exits via Fatal.
func TestCmdServe_UsageError(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdServe(nil)
	})
}

// TestCmdServe_EndToEnd starts the real CLI server on a fixed loopback
// address, fetches the master playlist over HTTP, then sends the process a
// SIGINT (what Ctrl-C delivers) and checks CmdServe shuts down gracefully
// instead of hanging.
func TestCmdServe_EndToEnd(t *testing.T) {
	src := sampleMKV(t)
	const addr = "127.0.0.1:18479"

	done := make(chan struct{})
	go func() {
		commands.CmdServe([]string{src, "-addr", addr})
		close(done)
	}()

	var resp *http.Response
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/master.m3u8")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not come up in time: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /master.m3u8 status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "#EXTM3U") {
		t.Errorf("unexpected master playlist body: %q", string(body))
	}

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatalf("send interrupt: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CmdServe did not shut down after SIGINT")
	}
}

// TestCmdServe_DirectEndToEnd checks --direct serves the raw file byte for
// byte (no HLS packaging) at a URL named after the source file, and still
// shuts down cleanly on SIGINT.
func TestCmdServe_DirectEndToEnd(t *testing.T) {
	src := sampleMKV(t)
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	const addr = "127.0.0.1:18480"

	done := make(chan struct{})
	go func() {
		commands.CmdServe([]string{src, "-addr", addr, "--direct"})
		close(done)
	}()

	url := "http://" + addr + "/" + filepath.Base(src)
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not come up in time: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/x-matroska" {
		t.Errorf("Content-Type = %q, want video/x-matroska", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("direct-served body does not match the source file (%d vs %d bytes)", len(body), len(want))
	}

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Fatalf("send interrupt: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CmdServe --direct did not shut down after SIGINT")
	}
}

// TestCmdServe_DirectAndAutoMutuallyExclusive checks --direct and --auto
// together is a usage error, not a silent pick of one.
func TestCmdServe_DirectAndAutoMutuallyExclusive(t *testing.T) {
	src := sampleMKV(t)
	mustFatal(t, func() {
		commands.CmdServe([]string{src, "--direct", "--auto"})
	})
}
