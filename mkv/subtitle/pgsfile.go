package subtitle

// pgsfile.go - PGS as the .sup file that sits beside a video: the segments of the stream one after another, each
// behind "PG" and its presentation and decoding times on the 90 kHz clock.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// supMaxDisplaySet bounds the segments gathered for one display set: what an epoch's objects may hold.
const supMaxDisplaySet = pgsMaxEpochBytes

// ForEachSUPDisplaySet reads a .sup file and calls fn with each display set: its time, the presentation time of its
// composition segment, and its segments as a Matroska block holds them, which is what PGSDecoder.Decode takes. A
// file cut short inside a segment gives the display sets ahead of it, then an error; one that ends between two
// segments gives its last display set as it stands, without its end segment. An error from fn stops the read and is
// returned as is.
func ForEachSUPDisplaySet(r io.Reader, fn func(timeMs int64, displaySet []byte) error) error {
	br := bufio.NewReader(r)
	var set []byte
	var timeMs int64
	timed := false
	for n := 0; ; n++ {
		var head [13]byte
		if _, err := io.ReadFull(br, head[:]); err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("PGS file: segment %d is cut short in its header: %w", n, err)
			}
			if len(set) == 0 {
				return nil
			}
			return fn(timeMs, set) // a last display set without its end segment
		}
		if head[0] != 'P' || head[1] != 'G' {
			return fmt.Errorf("PGS file: segment %d starts with %q, not \"PG\": the file is not a .sup", n, head[:2])
		}
		typ, size := head[10], int(binary.BigEndian.Uint16(head[11:13]))
		if len(set)+3+size > supMaxDisplaySet {
			return fmt.Errorf("PGS file: the display set at %d ms holds more than %d bytes", timeMs, supMaxDisplaySet)
		}
		set = append(set, head[10:13]...)
		set = append(set, make([]byte, size)...)
		if _, err := io.ReadFull(br, set[len(set)-size:]); err != nil {
			return fmt.Errorf("PGS file: segment %d is cut short in its %d bytes: %w", n, size, err)
		}
		if typ == pgsSegPCS || !timed {
			timeMs, timed = int64(binary.BigEndian.Uint32(head[2:6]))/90, true
		}
		if typ != pgsSegEND {
			continue
		}
		if err := fn(timeMs, set); err != nil {
			return err
		}
		set, timed = nil, false
	}
}
