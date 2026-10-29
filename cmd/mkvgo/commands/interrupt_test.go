//go:build !windows

package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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

// TestInterruptedCommandDiesOfIt: the cancelled operation's error ended the process with exit 1, so a shell loop
// running mkvgo went on to the next file; it now ends the way an Interrupt does.
func TestInterruptedCommandDiesOfIt(t *testing.T) {
	stop := WatchInterrupt()
	defer stop()
	defer func() { interrupted.Store(false) }()
	died := false
	dieOfInterrupt = func() { died = true }
	defer func() { dieOfInterrupt = raiseInterrupt }()
	exited := -1
	osExit = func(code int) { exited = code }
	defer func() { osExit = os.Exit }()

	ctx := cliCtx
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	FatalErr(fmt.Errorf("remux: %w", ctx.Err()))
	if !died || exited != -1 {
		t.Errorf("died %v, exit %d: want the Interrupt's death, not an exit", died, exited)
	}
}

// TestRaiseInterruptKillsWithSIGINT: the process ends killed by SIGINT, what a shell reads as Ctrl-C.
func TestRaiseInterruptKillsWithSIGINT(t *testing.T) {
	if os.Getenv("MKVGO_RAISE_INTERRUPT") == "1" {
		signal.Notify(make(chan os.Signal, 1), os.Interrupt) // caught, as WatchInterrupt does
		raiseInterrupt()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRaiseInterruptKillsWithSIGINT$")
	cmd.Env = append(os.Environ(), "MKVGO_RAISE_INTERRUPT=1")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the child ended with %v", err)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Errorf("the child ended %v, want killed by SIGINT", ee)
	}
}
