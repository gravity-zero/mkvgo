package commands_test

import (
	"io"
	"net/http"
	"os"
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
