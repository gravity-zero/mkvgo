package reader

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// withEBMLHeader replaces the empty EBML header buildStreamMKV writes (4 ID
// bytes + a 1-byte zero size) with header.
func withEBMLHeader(t *testing.T, header []byte) []byte {
	t.Helper()
	file := buildStreamMKV(t, false, [][]mkv.Block{{{TrackNumber: 1, Keyframe: true, Data: []byte{1}}}}, 1_000_000)
	const emptyHeaderLen = 5
	if !bytes.Equal(file[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) || file[4] != 0x80 {
		t.Fatalf("fixture does not start with an empty EBML header: % X", file[:emptyHeaderLen])
	}
	return append(append([]byte{}, header...), file[emptyHeaderLen:]...)
}

// rawEBMLHeader wraps body in an EBML header element.
func rawEBMLHeader(body []byte) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, ebml.IDEBMLHeader, int64(len(body)))
	b.Write(body)
	return b.Bytes()
}

func docTypeBody(docType string, version, readVersion uint64) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, ebml.IDDocType, int64(len(docType)))
	ebml.WriteString(&b, docType)
	ebml.WriteElementHeader(&b, ebml.IDDocTypeVersion, 1)
	ebml.WriteUint(&b, version, 1)
	ebml.WriteElementHeader(&b, ebml.IDDocTypeReadVersion, 1)
	ebml.WriteUint(&b, readVersion, 1)
	return b.Bytes()
}

type docTypeWant struct {
	docType      string
	version      uint64
	readVersion  uint64
	isWebM       bool
	pathContains string // "" = must open
}

// readAllPaths opens data through the three Matroska readers and checks the
// DocType declaration each one reports.
func readAllPaths(t *testing.T, data []byte, want docTypeWant) {
	t.Helper()
	ctx := context.Background()
	paths := []struct {
		name string
		open func() (*mkv.Container, error)
	}{
		{"Read", func() (*mkv.Container, error) { return Read(ctx, bytes.NewReader(data), "x.mkv") }},
		{"ReadMeta", func() (*mkv.Container, error) { return ReadMeta(ctx, bytes.NewReader(data), "x.mkv") }},
		{"ReadStream", func() (*mkv.Container, error) {
			c, _, err := ReadStream(ctx, &readerOnly{r: bytes.NewReader(data)})
			return c, err
		}},
	}
	for _, p := range paths {
		c, err := p.open()
		if want.pathContains != "" {
			if err == nil || !strings.Contains(err.Error(), want.pathContains) {
				t.Errorf("%s: err = %v, want containing %q", p.name, err, want.pathContains)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		if c.DocType != want.docType || c.DocTypeVersion != want.version || c.DocTypeReadVersion != want.readVersion {
			t.Errorf("%s: DocType = %q/%d/%d, want %q/%d/%d", p.name,
				c.DocType, c.DocTypeVersion, c.DocTypeReadVersion, want.docType, want.version, want.readVersion)
		}
		if c.IsWebM() != want.isWebM {
			t.Errorf("%s: IsWebM = %v, want %v", p.name, c.IsWebM(), want.isWebM)
		}
		// The header parse must leave the reader on the Segment: the metadata
		// behind it is intact.
		if len(c.Tracks) != 1 || c.Info.TimecodeScale != 1_000_000 {
			t.Errorf("%s: metadata after the header lost: tracks=%d scale=%d", p.name, len(c.Tracks), c.Info.TimecodeScale)
		}
	}
}

func TestDocTypeFromWriterHeaders(t *testing.T) {
	var webm, mkvHdr bytes.Buffer
	if err := writer.WriteEBMLHeaderWebM(&webm); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteEBMLHeader(&mkvHdr); err != nil {
		t.Fatal(err)
	}
	t.Run("webm", func(t *testing.T) {
		readAllPaths(t, withEBMLHeader(t, webm.Bytes()), docTypeWant{docType: "webm", version: 2, readVersion: 2, isWebM: true})
	})
	t.Run("matroska", func(t *testing.T) {
		readAllPaths(t, withEBMLHeader(t, mkvHdr.Bytes()), docTypeWant{docType: "matroska", version: 4, readVersion: 2})
	})
}

func TestDocTypeHeaderVariants(t *testing.T) {
	// A DocType past the cap is not a DocType: skipped, the versions behind it
	// are still read and the file still opens.
	huge := docTypeBody(strings.Repeat("w", maxDocTypeLen+1), 4, 2)
	// A child declaring more bytes than the header holds ends the walk; what
	// came before it is kept and the file still opens.
	var overrun bytes.Buffer
	overrun.Write(docTypeBody("webm", 4, 2))
	ebml.WriteElementHeader(&overrun, ebml.IDEBMLVersion, 1000)
	// A version wider than 8 bytes is skipped, not an error.
	var wideVersion bytes.Buffer
	ebml.WriteElementHeader(&wideVersion, ebml.IDDocTypeVersion, 9)
	wideVersion.Write(make([]byte, 9))
	wideVersion.Write(docTypeBody("webm", 0, 0)[:7]) // DocType element only: 2-byte ID + size + "webm"

	tests := []struct {
		name string
		body []byte
		want docTypeWant
	}{
		{"empty header", nil, docTypeWant{}},
		{"declared av1 webm", docTypeBody("webm", 4, 2), docTypeWant{docType: "webm", version: 4, readVersion: 2, isWebM: true}},
		{"unknown doctype kept verbatim", docTypeBody("mkvgo-test", 1, 1), docTypeWant{docType: "mkvgo-test", version: 1, readVersion: 1}},
		{"oversized doctype skipped", huge, docTypeWant{version: 4, readVersion: 2}},
		{"child overrun keeps what precedes it", overrun.Bytes(), docTypeWant{docType: "webm", version: 4, readVersion: 2, isWebM: true}},
		{"wide version skipped", wideVersion.Bytes(), docTypeWant{docType: "webm", isWebM: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			readAllPaths(t, withEBMLHeader(t, rawEBMLHeader(tc.body)), tc.want)
		})
	}
}

func TestDocTypeHeaderTruncated(t *testing.T) {
	// The file ends inside the header body: an error, never a panic.
	data := rawEBMLHeader(docTypeBody("webm", 2, 2))[:9]
	readAllPaths(t, data, docTypeWant{pathContains: "ebml header"})
}

func TestDocTypeUnknownSizeHeaderRefused(t *testing.T) {
	var b bytes.Buffer
	ebml.WriteElementID(&b, ebml.IDEBMLHeader)
	ebml.WriteDataSize(&b, -1)
	b.Write(docTypeBody("webm", 2, 2))
	readAllPaths(t, withEBMLHeader(t, b.Bytes()), docTypeWant{pathContains: "unknown size"})
}
