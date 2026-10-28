//go:build !windows

package commands

import (
	"syscall"
	"testing"
	"time"
)

// TestInterruptCancelsTheCommandsContext: Ctrl-C killed the process in the middle of a write, leaving a truncated
// output; it cancels the context the operations run under, which stop and remove what they wrote.
func TestInterruptCancelsTheCommandsContext(t *testing.T) {
	stop := WatchInterrupt()
	defer stop()
	ctx := cliCtx
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an Interrupt left the commands' context running")
	}
}
