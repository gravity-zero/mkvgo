package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
)

// cliCtx is the context the commands run their operations under: an Interrupt (Ctrl-C) cancels it, so an operation
// stops where it is and removes the output it was writing, as a failed one does, instead of the process dying with a
// truncated file behind it. A second Interrupt ends the process at once.
var cliCtx = context.Background()

// interrupted is set once an Interrupt cancelled cliCtx.
var interrupted atomic.Bool

// dieOfInterrupt ends the process as an Interrupt does, so a shell running it in a loop stops too (see
// interrupt_unix.go); a variable so tests do not end theirs.
var dieOfInterrupt = raiseInterrupt

// WatchInterrupt makes an Interrupt cancel the commands' context, and a second one end the process at once; main
// calls it once. The returned stop undoes it.
func WatchInterrupt() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	cliCtx = ctx
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			interrupted.Store(true)
			cancel()
		case <-done:
			return
		}
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr, "interrupted")
			dieOfInterrupt()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		cancel()
		cliCtx = context.Background()
	}
}

// interruptedBy reports whether err is the cancellation an Interrupt caused.
func interruptedBy(err error) bool {
	return interrupted.Load() && errors.Is(err, context.Canceled)
}
