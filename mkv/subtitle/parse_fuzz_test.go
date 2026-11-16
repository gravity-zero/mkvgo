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

// FuzzVobSubDecode: any bytes handed to a VobSub decoder, in one block or split in two, give display sets or an
// error, never a panic, within the input's time budget.
func FuzzVobSubDecode(f *testing.F) {
	f.Add(vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)}, spuSeq{date: 300, cmds: []byte{spuStop}}), 7)
	f.Add(vobsubSPU(nil, nil, spuSeq{date: 45, cmds: []byte{spuStop}}), 0)
	f.Add([]byte{0x00, 0x08, 0x00, 0x04, 0x00, 0x00, 0x00, 0x04}, 0)
	f.Fuzz(func(t *testing.T, data []byte, cut int) {
		costguard.Fuzz(t, len(data), func() {
			d, err := NewVobSubDecoder([]byte(vobsubIdx))
			if err != nil {
				t.Fatal(err)
			}
			if cut > 0 && cut < len(data) {
				_, _ = d.Decode(data[:cut])
				_, _ = d.Decode(data[cut:])
				return
			}
			_, _ = d.Decode(data)
		})
	})
}

// FuzzVobSubHeader: any CodecPrivate gives a decoder or an error, never a panic.
func FuzzVobSubHeader(f *testing.F) {
	f.Add([]byte(vobsubIdx))
	f.Add([]byte("size: 0x0\npalette: ,\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() { _, _ = NewVobSubDecoder(data) })
	})
}

// FuzzDVBSubDecode: any bytes handed to a DVB subtitle decoder, in one block or split in two, with or without the
// page filter, give display sets or an error, never a panic, within the input's time budget.
func FuzzDVBSubDecode(f *testing.F) {
	f.Add(dvbShow4x2(), 0, true)
	f.Add(concat(dvbShow4x2(), dvbPCS(0), dvbEDS()), len(dvbShow4x2()), false)
	f.Add(concat(dvbPCS(2, dvbPlacement{}), dvbRCS(0, true, 720, 36, 3, 0, 1, dvbObjectRef{id: 1}),
		dvbODS(1, true, []byte{dvbMap2To8, 1, 2, 3, 4, dvbData2Bit, 0x6c, 0x00, dvbDataEndRow, dvbData8Bit, 0x41, 0x00, 0x00}, nil)), 9, true)
	f.Fuzz(func(t *testing.T, data []byte, cut int, filter bool) {
		costguard.Fuzz(t, len(data), func() {
			var private []byte
			if filter {
				private = dvbTestPrivate
			}
			d, err := NewDVBSubDecoder(private)
			if err != nil {
				t.Fatal(err)
			}
			if cut > 0 && cut < len(data) {
				_, _ = d.Decode(data[:cut])
				_, _ = d.Decode(data[cut:])
				return
			}
			_, _ = d.Decode(data)
		})
	})
}

// FuzzVobSubFiles: any .idx and any .sub give units or an error, never a panic, within the input's time budget.
func FuzzVobSubFiles(f *testing.F) {
	unit := vobsubSPU(spuTop4x2, spuBottom4x2, spuSeq{cmds: spuShow4x2(spuStart)})
	f.Add([]byte(vobsubIdxFile), concat(subPack(0x20, true, unit[:9]), subPadding(5), subPack(0x20, false, unit[9:])))
	f.Add([]byte("id: en, index: 0\ntimestamp: 00:00:00:000, filepos: 0\n"), []byte{0, 0, 1, 0xbd, 0, 4, 0x80, 0, 0, 0x20})
	f.Fuzz(func(t *testing.T, idx, sub []byte) {
		costguard.Fuzz(t, len(idx)+len(sub), func() {
			ix, err := ParseVobSubIndex(bytes.NewReader(idx))
			if err != nil {
				return
			}
			for i := range ix.Streams {
				_ = ix.ForEachUnit(bytes.NewReader(sub), i, func(int64, []byte) error { return nil })
			}
		})
	})
}

// FuzzSUPFile: any bytes read as a .sup give display sets or an error, never a panic.
func FuzzSUPFile(f *testing.F) {
	f.Add(supFile(concat(pgsPCS(1920, 1080, pgsEpochStart, 0), pgsEND()), pgsPCS(1920, 1080, 0, 0)))
	f.Add([]byte("PG"))
	f.Fuzz(func(t *testing.T, data []byte) {
		costguard.Fuzz(t, len(data), func() {
			d := NewPGSDecoder()
			_ = ForEachSUPDisplaySet(bytes.NewReader(data), func(_ int64, set []byte) error {
				_, _ = d.Decode(set)
				return nil
			})
		})
	})
}
