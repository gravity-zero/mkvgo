//go:build !unix

// Package fileid names an OS file beyond its size and mtime.
package fileid

import "os"

// Of has no portable file identity here; os.SameFile still tells two files apart.
func Of(os.FileInfo) string { return "" }
