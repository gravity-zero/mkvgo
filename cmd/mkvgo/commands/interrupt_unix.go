//go:build !windows

package commands

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// raiseInterrupt stops catching SIGINT and sends it to the process, which then dies of it: a shell sees a child
// killed by Ctrl-C, not one that exited, and stops its loop as well. Exit 130 if it is still alive.
func raiseInterrupt() {
	signal.Reset(os.Interrupt)
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	time.Sleep(time.Second)
	os.Exit(130)
}
