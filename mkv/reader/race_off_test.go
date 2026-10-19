//go:build !race

package reader

// raceBuild reports a -race build, whose instrumentation allocates on top of what the code does.
const raceBuild = false
