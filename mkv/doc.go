// Package mkv holds the core Matroska model (Container, Track, Block, …) shared
// by its subpackages reader, writer, ops and subtitle, which implement the
// parsing, serialisation and high-level operations.
//
// STABILITY: mkv and its subpackages are lower-level building blocks and are
// considered EXPERIMENTAL — their exported APIs may change between minor
// versions. For a stable, backward-compatible surface, use the top-level
// matroska package; reach for these packages directly only when you need
// capabilities matroska does not expose (streaming, custom operations).
package mkv
