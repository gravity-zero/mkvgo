package subtitle

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// FuzzParseSRT: any text read as SRT gives entries or an error, never a panic, within the input's time budget.
func FuzzParseSRT(f *testing.F) {
	f.Add([]byte("1\n00:00:01,000 --> 00:00:02,500\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nWorld\n"))
	f.Add([]byte("\xef\xbb\xbf1\r\n00:00:00,000 --> 99:59:59,999\r\n\r\n"))
	f.Add([]byte("x\n--> \n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			entries, err := ParseSRTReader(bytes.NewReader(data))
			if err != nil {
				return
			}
			cues := SRTToCues(entries)
			ResolveCueEnds(cues, 2000)
			SettleCueEnds(cues, 1<<40)
			_ = WriteWebVTT(&bytes.Buffer{}, ShiftCues(cues, -1000))
		})
	})
}

// FuzzASSBlock: any Matroska ASS block flattens to text, never a panic.
func FuzzASSBlock(f *testing.F) {
	f.Add([]byte("0,0,Default,,0,0,0,,{\\i1}Hello{\\i0}\\Nworld"))
	f.Add([]byte(",,,,,,,,,"))
	f.Add([]byte("{"))
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			_ = FlattenASSBlock(data)
			_, _ = parseASSDialogue(string(data))
		})
	})
}

// FuzzPGSDecode: any bytes handed to a PGS decoder, in one block or split in two, give display sets or an error,
// never a panic, within the input's time budget.
func FuzzPGSDecode(f *testing.F) {
	f.Add([]byte{0x16, 0x00, 0x13, 0x07, 0x80, 0x04, 0x38, 0x10, 0x00, 0x00, 0x80, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00}, 5)
	f.Add([]byte{0x14, 0x00, 0x07, 0x00, 0x00, 0x00, 0x10, 0x80, 0x80, 0x80}, 3)
	f.Add([]byte{0x80, 0x00, 0x00}, 0)
	f.Fuzz(func(t *testing.T, data []byte, cut int) {
		costguard.Fuzz(t, len(data), func() {
			d := NewPGSDecoder()
			if cut > 0 && cut < len(data) {
				_, _ = d.Decode(data[:cut])
				_, _ = d.Decode(data[cut:])
				return
			}
			_, _ = d.Decode(data)
		})
	})
}
