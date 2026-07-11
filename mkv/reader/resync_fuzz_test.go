package reader

import (
	"bytes"
	"io"
	"testing"
)

// FuzzResyncToCluster hammers the bounded resync scanner (the recovery path
// of parseSegment, ops.Salvage and Reindex with Options.Resync) with
// arbitrary bytes and checks its invariants: it never errors on in-memory
// data, and any offset it returns is in bounds, within the limit, and points
// at a real Cluster ID.
func FuzzResyncToCluster(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x1F, 0x43, 0xB6, 0x75}) // bare magic, no valid first child
	// Magic followed by a plausible cluster: 1-byte size, Timestamp child.
	f.Add([]byte{0x1F, 0x43, 0xB6, 0x75, 0x83, 0xE7, 0x81, 0x00})
	// Garbage, then a false-positive magic, then a valid-looking cluster.
	f.Add(append(bytes.Repeat([]byte{0x00, 0x1F, 0x43, 0xB6}, 8),
		0x1F, 0x43, 0xB6, 0x75, 0x83, 0xE7, 0x81, 0x00))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, limit := range []int64{-1, int64(len(data)), int64(len(data)) / 2} {
			r := bytes.NewReader(data)
			if _, err := r.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			off, err := ResyncToCluster(r, limit)
			if err != nil {
				t.Fatalf("ResyncToCluster(limit=%d) returned an error on in-memory data: %v", limit, err)
			}
			if off == -1 {
				continue
			}
			if off < 0 || off+int64(len(clusterMagic)) > int64(len(data)) {
				t.Fatalf("offset %d out of bounds (len %d)", off, len(data))
			}
			if !bytes.Equal(data[off:off+int64(len(clusterMagic))], clusterMagic) {
				t.Fatalf("offset %d does not point at a Cluster ID", off)
			}
			if limit >= 0 && off+int64(len(clusterMagic)) > limit {
				t.Fatalf("match at %d crosses limit %d", off, limit)
			}
			pos, err := r.Seek(0, io.SeekCurrent)
			if err != nil {
				t.Fatal(err)
			}
			if pos != off {
				t.Fatalf("reader left at %d, want the returned offset %d", pos, off)
			}
		}
	})
}
