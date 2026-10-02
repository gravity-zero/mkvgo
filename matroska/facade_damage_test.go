package matroska

import (
	"errors"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestErrDamagedRegionReExport: the facade's sentinel is the reader's, so a
// caller that only imports the facade can test a remux refusal with errors.Is.
func TestErrDamagedRegionReExport(t *testing.T) {
	err := error(&reader.DamageError{Offset: 10, Resume: 20, Cause: errors.New("unexpected EOF")})
	if !errors.Is(err, ErrDamagedRegion) {
		t.Fatal("matroska.ErrDamagedRegion does not match a reader.DamageError")
	}
}
