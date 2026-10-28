package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

// cliCtx is the context the commands run their operations under: an Interrupt (Ctrl-C) cancels it, so an operation
// stops where it is and removes the output it was writing, as a failed one does, instead of the process dying with a
// truncated file behind it. A second Interrupt ends the process at once.
var cliCtx = context.Background()

// WatchInterrupt makes an Interrupt cancel the commands' context, and a second one exit with status 130; main calls
// it once. The returned stop undoes it.
func WatchInterrupt() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	cliCtx = ctx
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			cancel()
		case <-done:
			return
		}
		select {
		case <-sig:
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
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
