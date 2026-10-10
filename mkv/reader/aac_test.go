package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// bitWriter is the test-side MSB-first bit packer (mirrors the mp4 package's).
type bitWriter struct {
	b     []byte
	cur   byte
	nbits uint
}

func (w *bitWriter) write(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.cur |= byte((v>>uint(i))&1) << (7 - w.nbits)
		w.nbits++
		if w.nbits == 8 {
			w.b = append(w.b, w.cur)
			w.cur, w.nbits = 0, 0
		}
	}
}

func (w *bitWriter) bytes() []byte {
	if w.nbits > 0 {
		w.b = append(w.b, w.cur)
		w.cur, w.nbits = 0, 0
	}
	return w.b
}

// aacMKV is a one-track AAC file whose Audio element states only what the
// caller passes (sampleRate 0 = no SamplingFrequency element).
func aacMKV(cp []byte, channels uint64, sampleRate uint64) []byte {
	audio := []([]byte){}
	if channels > 0 {
		audio = append(audio, uintElem(mkv.IDChannels, channels, 1))
	}
	if sampleRate > 0 {
		var f bytes.Buffer
		ebml.WriteFloat(&f, float64(sampleRate))
		audio = append(audio, append(append([]byte{}, mustHeader(mkv.IDSamplingFreq, int64(f.Len()))...), f.Bytes()...))
	}
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AAC"),
		bytesElem(mkv.IDCodecPrivate, cp),
		masterElem(mkv.IDAudio, audio...),
	)
	info := masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4))
	tracks := masterElem(mkv.IDTracks, te)
	var seg bytes.Buffer
	seg.Write(info)
	seg.Write(tracks)
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

func mustHeader(id uint32, size int64) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, size)
	return b.Bytes()
}

// TestAACProfileFromASC pins real AudioSpecificConfigs (bytes from real
// files): the profile the container never states, the SBR output rate, and
// that container values win per field.
func TestAACProfileFromASC(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		asc         []byte
		wantProfile string
		wantCh      uint8
		wantRate    float64
		wantOut     float64 // 0 = OutputSampleRate stays nil
		wantAOT     uint32
	}{
		{"AAC-LC 44100 stereo", []byte{0x12, 0x10}, "LC", 2, 44100, 0, 2},
		{"AAC-LC 48000 stereo (implicit SBR invisible)", []byte{0x11, 0x90}, "LC", 2, 48000, 0, 2},
		{"HE-AAC explicit SBR, mono core, ext 44100", []byte{0x2b, 0x8a, 0x08, 0x00}, "HE-AAC", 1, 22050, 44100, 5},
		{"HE-AACv2 explicit PS", []byte{0xeb, 0x8a, 0x08, 0x00}, "HE-AACv2", 2, 22050, 44100, 29},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseAACConfig(tc.asc)
			if c.Profile != tc.wantProfile || c.Channels != tc.wantCh || c.SampleRate != tc.wantRate || c.OutputRate != tc.wantOut || c.ObjectType != tc.wantAOT {
				t.Fatalf("ParseAACConfig(% x) = %+v, want profile %q ch %d rate %v out %v aot %d", tc.asc, c, tc.wantProfile, tc.wantCh, tc.wantRate, tc.wantOut, tc.wantAOT)
			}
			// Through the reader: container Channels/SampleRate given, the ASC
			// fills Profile and the SBR output rate only.
			c2, err := ReadMeta(ctx, bytes.NewReader(aacMKV(tc.asc, 6, 48000)), "x.mkv")
			if err != nil {
				t.Fatal(err)
			}
			tr := c2.Tracks[0]
			if tr.Profile != tc.wantProfile || tr.Channels == nil || *tr.Channels != 6 || tr.SampleRate == nil || *tr.SampleRate != 48000 {
				t.Errorf("reader: profile=%q channels=%v rate=%v, want %q / 6 / 48000 (container wins)", tr.Profile, tr.Channels, tr.SampleRate, tc.wantProfile)
			}
			if (tc.wantOut == 0) != (tr.OutputSampleRate == nil) || (tr.OutputSampleRate != nil && *tr.OutputSampleRate != tc.wantOut) {
				t.Errorf("reader: OutputSampleRate=%v, want %v", tr.OutputSampleRate, tc.wantOut)
			}
			// No container audio fields: the ASC fills them.
			c3, err := ReadMeta(ctx, bytes.NewReader(aacMKV(tc.asc, 0, 0)), "x.mkv")
			if err != nil {
				t.Fatal(err)
			}
			tr = c3.Tracks[0]
			if tr.Channels == nil || *tr.Channels != tc.wantCh || tr.SampleRate == nil || *tr.SampleRate != tc.wantRate {
				t.Errorf("reader without container fields: channels=%v rate=%v, want %d / %v", tr.Channels, tr.SampleRate, tc.wantCh, tc.wantRate)
			}
		})
	}
}

// ── aac.go - getAudioObjectType escape code ────────────────────────────

// TestGetAudioObjectTypeEscape covers the aot==31 escape path (reads 6 extra bits).
func TestGetAudioObjectTypeEscape(t *testing.T) {
	var bw bitWriter
	bw.write(31, 5) // escape: 5 bits all-ones
	bw.write(0, 6)  // extension: 0 → aot = 32
	r := &bitReader{data: bw.bytes()}
	if got := getAudioObjectType(r); got != 32 {
		t.Errorf("escape aot = %d, want 32", got)
	}
	// Without escape: 5 bits is the final value.
	var bw2 bitWriter
	bw2.write(2, 5) // AAC-LC
	r2 := &bitReader{data: bw2.bytes()}
	if got := getAudioObjectType(r2); got != 2 {
		t.Errorf("non-escape aot = %d, want 2", got)
	}
}

// ── aac.go - readSamplingFrequency explicit (idx==0xF) ─────────────────

// TestReadSamplingFrequencyExplicit covers the 24-bit explicit frequency branch.
func TestReadSamplingFrequencyExplicit(t *testing.T) {
	var bw bitWriter
	bw.write(0xF, 4)    // explicit indicator
	bw.write(44100, 24) // frequency in Hz
	r := &bitReader{data: bw.bytes()}
	if got := readSamplingFrequency(r); got != 44100 {
		t.Errorf("explicit freq = %v, want 44100", got)
	}
}

// ── aac.go - skipGASpecificConfig paths ─────────────────────────────────

// TestSkipGASpecificConfigPaths covers the dependsOnCoreCoder, aot-specific
// layerNr, extensionFlag+aot=22 (numOfSubFrame/layer_length), and
// extensionFlag+aot=17 (resilience flags) branches.
func TestSkipGASpecificConfigPaths(t *testing.T) {
	// cc=0 → false immediately (program_config_element).
	{
		r := &bitReader{data: []byte{}}
		if got := skipGASpecificConfig(r, 2, 0, new(uint32)); got {
			t.Error("cc=0: want false")
		}
	}
	// dependsOnCoreCoder=1 → reads 14 extra bits.
	{
		var bw bitWriter
		bw.write(0, 1)  // frameLengthFlag
		bw.write(1, 1)  // dependsOnCoreCoder=1
		bw.write(0, 14) // coreCoderDelay
		bw.write(0, 1)  // extensionFlag=0
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 2, 2, new(uint32)); !got {
			t.Error("dependsOnCoreCoder=1: want true")
		}
	}
	// aot=6 → reads layerNr (3 bits).
	{
		var bw bitWriter
		bw.write(0, 1) // frameLengthFlag
		bw.write(0, 1) // dependsOnCoreCoder=0
		bw.write(0, 1) // extensionFlag=0
		bw.write(0, 3) // layerNr
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 6, 2, new(uint32)); !got {
			t.Error("aot=6 (layerNr): want true")
		}
	}
	// extensionFlag=1, aot=22 → reads numOfSubFrame(5) + layer_length(11) + extensionFlag3(1).
	{
		var bw bitWriter
		bw.write(0, 1)  // frameLengthFlag
		bw.write(0, 1)  // dependsOnCoreCoder=0
		bw.write(1, 1)  // extensionFlag=1
		bw.write(0, 5)  // numOfSubFrame
		bw.write(0, 11) // layer_length
		bw.write(0, 1)  // extensionFlag3
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 22, 2, new(uint32)); !got {
			t.Error("aot=22 extensionFlag=1: want true")
		}
	}
	// extensionFlag=1, aot=17 → reads 3 resilience flags + extensionFlag3(1).
	{
		var bw bitWriter
		bw.write(0, 1) // frameLengthFlag
		bw.write(0, 1) // dependsOnCoreCoder=0
		bw.write(1, 1) // extensionFlag=1
		bw.write(0, 3) // section/scalefactor/spectral data resilience flags
		bw.write(0, 1) // extensionFlag3
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 17, 2, new(uint32)); !got {
			t.Error("aot=17 extensionFlag=1: want true")
		}
	}
	// extensionFlag=1, aot=20 → layerNr (aot 6/20) then the same resilience
	// flags as 17/19/23. The old fixture omitted layerNr and passed only
	// because the bit reader of the day did not flag the 3-bit overrun.
	{
		var bw bitWriter
		bw.write(0, 1) // frameLengthFlag
		bw.write(0, 1) // dependsOnCoreCoder=0
		bw.write(1, 1) // extensionFlag=1
		bw.write(0, 3) // layerNr
		bw.write(0, 3) // resilience flags
		bw.write(0, 1) // extensionFlag3
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 20, 2, new(uint32)); !got {
			t.Error("aot=20 extensionFlag=1: want true")
		}
	}
}

// ── aac.go: aacChannelsFrom boundary ─────────────────────────────────────

// TestAACChannelsFromBoundary kills the cc >= uint32(len(aacConfigChannels))
// CONDITIONALS_BOUNDARY mutant.  cc==7 (last valid index, len-1) must return
// the table value 8; cc==8 (== len) must return 0.
func TestAACChannelsFromBoundary(t *testing.T) {
	// aacConfigChannels = [8]uint8{0,1,2,3,4,5,6,8}, len=8
	if got := aacChannelsFrom(7, false); got != 8 {
		t.Errorf("cc=7 (last valid): %d, want 8", got)
	}
	if got := aacChannelsFrom(8, false); got != 0 {
		t.Errorf("cc=8 (== len): %d, want 0", got)
	}
	if got := aacChannelsFrom(0, false); got != 0 {
		t.Errorf("cc=0 (program config): %d, want 0", got)
	}
}

// ── aac.go: bitsLeft arithmetic ──────────────────────────────────────────

// TestBitsLeftArithmetic kills ARITHMETIC_BASE mutants on len(data)*8 - pos.
func TestBitsLeftArithmetic(t *testing.T) {
	// 5 bytes × 8 bits − 8 consumed = 32.
	r := &bitReader{data: make([]byte, 5), pos: 8}
	if got := bitsLeft(r); got != 32 {
		t.Errorf("5B pos=8: bitsLeft = %d, want 32", got)
	}
	// 3 bytes × 8 − 0 = 24.
	r2 := &bitReader{data: make([]byte, 3), pos: 0}
	if got := bitsLeft(r2); got != 24 {
		t.Errorf("3B pos=0: bitsLeft = %d, want 24", got)
	}
	// exactly exhausted: 2 bytes − 16 bits = 0.
	r3 := &bitReader{data: make([]byte, 2), pos: 16}
	if got := bitsLeft(r3); got != 0 {
		t.Errorf("2B pos=16: bitsLeft = %d, want 0", got)
	}
}
