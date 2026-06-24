package commands

// export_test.go — test-only hooks into unexported internals, visible to the
// external commands_test package.

// SetExit overrides the process-exit hook Fatal uses and returns a function that
// restores the previous one. Tests use it (with recover) to drive the CLI error
// paths in-process instead of terminating the test binary.
func SetExit(f func(int)) (restore func()) {
	old := osExit
	osExit = f
	return func() { osExit = old }
}
