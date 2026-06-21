package writer

import (
	"bytes"
	"testing"
)

func TestWriteEBMLHeaderWebM(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEBMLHeaderWebM(&buf); err != nil {
		t.Fatalf("WriteEBMLHeaderWebM: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("wrote nothing")
	}
	// The header must declare the "webm" DocType.
	if !bytes.Contains(buf.Bytes(), []byte("webm")) {
		t.Errorf("EBML header does not declare the webm DocType: % x", buf.Bytes())
	}
}
