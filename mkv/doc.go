// Package mkv holds the core Matroska model (Container, Track, Block, …) shared
// by its subpackages reader, writer, ops and subtitle, which implement the
// parsing, serialisation and high-level operations. The top-level matroska
// package is a smaller facade over them for most uses; reach for these packages
// directly for what it does not expose (streaming, custom operations).
package mkv
