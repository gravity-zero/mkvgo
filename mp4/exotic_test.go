package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// exotic_test.go - the sample entries and layouts of public test corpora a
// probe must describe and a remux must carry or refuse by name: protected
// entries, unknown fourccs, PCM, fragments without mehd, version 0 truns with
// negative offsets, compositions wider than a cluster, encrypted Matroska.

// stereo48k is the audio track whose fields audioSampleEntry writes.
var stereo48k = mkv.Track{Channels: u8p(2), SampleRate: f64p(48000)}

// soundTrak builds a trak of handler "soun" around one sample entry; table is the stbl's other boxes (empty = none).
func soundTrak(trackID uint32, timescale uint32, durTicks uint32, entry []byte, table ...[]byte) []byte {
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(timescale)
		w.u32(durTicks)
		w.u16(0x55C4) // "und"
		w.u16(0)
	})
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) {
		w.u32(0)
		w.fourcc("soun")
		w.zeros(12)
		w.u8(0)
	})
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	stbl := container("stbl", append([][]byte{stsd}, table...)...)
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(trackID)
		w.u32(0)
		w.u32(0)
	})
	return container("trak", tkhd, container("mdia", mdhd, hdlr, container("minf", stbl)))
}

// emptyTables are the sample tables of a fragmented track's moov.
func emptyTables() [][]byte {
	return [][]byte{
		fullBox("stts", 0, 0, func(w *bw) { w.u32(0) }),
		fullBox("stsc", 0, 0, func(w *bw) { w.u32(0) }),
		fullBox("stsz", 0, 0, func(w *bw) { w.u32(0); w.u32(0) }),
		fullBox("stco", 0, 0, func(w *bw) { w.u32(0) }),
	}
}

func mvhdBox(durMs uint32) []byte {
	return fullBox("mvhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(1000)
		w.u32(durMs)
		w.u32(0x00010000)
		w.u16(0x0100)
		w.zeros(10)
		w.matrix([9]uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000})
		w.zeros(24)
		w.u32(2)
	})
}

// sinfBox wraps an original format in a cenc protection scheme.
func sinfBox(original, scheme string) []byte {
	return container("sinf",
		box("frma", []byte(original)),
		fullBox("schm", 0, 0, func(w *bw) { w.fourcc(scheme); w.u32(0x00010000) }),
		container("schi", fullBox("tenc", 0, 0, func(w *bw) { w.u16(0); w.u8(1); w.u8(8); w.zeros(16) })))
}

// probeMovie parses a one-track movie head-only and returns its container.
func probeMovie(t *testing.T, data []byte, opts ...Options) (*mkv.Container, []DroppedTrack) {
	t.Helper()
	c, dropped, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mp4", opts...)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	return c, dropped
}

// TestProbeDescribesEveryTrack: a probe never depends on what a remux can
// carry - a protected entry is read through its original format and named
// encrypted, an unknown fourcc is reported as is with what its entry states.
func TestProbeDescribesEveryTrack(t *testing.T) {
	esds := esdsBox(0x40, fakeASC)
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 44100, 44100, audioSampleEntry("enca", &stereo48k, append(esds, sinfBox("mp4a", "cenc")...)), emptyTables()...),
		soundTrak(2, 48000, 48000, audioSampleEntry("mhm1", &stereo48k, nil), emptyTables()...),
		soundTrak(3, 48000, 48000, audioSampleEntry("encv", &stereo48k, nil), emptyTables()...),
		soundTrak(4, 8000, 8000, audioSampleEntry("samr", &stereo48k, nil), emptyTables()...),
		soundTrak(5, 48000, 48000, audioSampleEntry("mhm1", &mkv.Track{Channels: u8p(0), SampleRate: f64p(48000)},
			box("mhaC", []byte{1, 0x0D, 6, 0, 0})), emptyTables()...),
	)...)
	c, dropped := probeMovie(t, data)
	if len(dropped) != 0 || len(c.Tracks) != 5 {
		t.Fatalf("tracks %d, dropped %v; want 5 tracks described and none dropped", len(c.Tracks), dropped)
	}
	if amr := c.Tracks[3]; amr.Codec != "samr" || amr.Channels == nil || *amr.Channels != 1 {
		t.Errorf("amr: codec %q channels %v, want samr mono (the entry's channelcount is a reserved 2)", amr.Codec, amr.Channels)
	}
	if mpegh := c.Tracks[4]; mpegh.Codec != "mhm1" || mpegh.Channels == nil || *mpegh.Channels != 6 {
		t.Errorf("mhm1: codec %q channels %v, want 6 from the mhaC's CICP layout", mpegh.Codec, mpegh.Channels)
	}
	aac := c.Tracks[0]
	if aac.Codec != "aac" || aac.Encryption != "cenc" || aac.Channels == nil || *aac.Channels != 2 || aac.SampleRate == nil || *aac.SampleRate != 44100 {
		t.Errorf("protected aac: codec %q encryption %q channels %v rate %v", aac.Codec, aac.Encryption, aac.Channels, aac.SampleRate)
	}
	mpegh := c.Tracks[1]
	if mpegh.Codec != "mhm1" || mpegh.Encryption != "" || mpegh.Channels == nil || *mpegh.Channels != 2 || mpegh.SampleRate == nil || *mpegh.SampleRate != 48000 || mpegh.DurationMs != 1000 {
		t.Errorf("mhm1: codec %q channels %v rate %v duration %d", mpegh.Codec, mpegh.Channels, mpegh.SampleRate, mpegh.DurationMs)
	}
	if bare := c.Tracks[2]; bare.Codec != "encv" || bare.Encryption != "unspecified" {
		t.Errorf("encv without sinf: codec %q encryption %q", bare.Codec, bare.Encryption)
	}
}

// TestRemuxRefusesUncarriableTracksByName: the remux names the track it
// cannot carry and why; Options.SkipUnsupported drops it with that reason.
func TestRemuxRefusesUncarriableTracksByName(t *testing.T) {
	esds := esdsBox(0x40, fakeASC)
	moof, mdat := oneFragment(1, 0, []uint32{10, 10}, []uint32{1024, 1024}, nil, 1)
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 44100, 0, audioSampleEntry("mp4a", &stereo48k, esds), emptyTables()...),
		soundTrak(2, 44100, 0, audioSampleEntry("enca", &stereo48k, append(esds, sinfBox("mp4a", "cbcs")...)), emptyTables()...),
		soundTrak(3, 48000, 0, audioSampleEntry("ac-4", &stereo48k, nil), emptyTables()...),
		container("mvex", trexBox(1), trexBox(2), trexBox(3)),
	)...)
	data = append(append(data, moof...), mdat...)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err := RemuxFromMP4(context.Background(), src, filepath.Join(dir, "out.mkv"))
	if err == nil || !strings.Contains(err.Error(), "track 2 (aac): encrypted (cbcs)") {
		t.Fatalf("remux with an encrypted track: err = %v, want the track named encrypted (cbcs)", err)
	}
	var drops []DroppedTrack
	err = RemuxFromMP4(context.Background(), src, filepath.Join(dir, "out.mkv"),
		Options{SkipUnsupported: true, OnDrop: func(d DroppedTrack) { drops = append(drops, d) }})
	if err != nil {
		t.Fatalf("remux skipping unsupported: %v", err)
	}
	if len(drops) != 2 || drops[0].ID != 2 || !strings.Contains(drops[0].Reason, "cbcs") || drops[1].ID != 3 || drops[1].Codec != "ac-4" {
		t.Errorf("drops = %+v, want the encrypted track 2 and the ac-4 track 3", drops)
	}
	c, _ := readMKV(t, filepath.Join(dir, "out.mkv"))
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "aac" {
		t.Errorf("output tracks = %+v, want the clear aac track alone", c.Tracks)
	}
	// The on-demand plan goes through the same track plan: refused by name, or dropped under SkipUnsupported.
	if _, err := PlanHLS(context.Background(), src); err == nil || !strings.Contains(err.Error(), "encrypted (cbcs)") {
		t.Errorf("PlanHLS: err = %v, want the encrypted track named", err)
	}
	drops = nil
	plan, err := PlanHLS(context.Background(), src, Options{SkipUnsupported: true, OnDrop: func(d DroppedTrack) { drops = append(drops, d) }})
	if err != nil {
		t.Fatalf("PlanHLS skipping unsupported: %v", err)
	}
	_ = plan
	if len(drops) != 2 {
		t.Errorf("PlanHLS drops = %+v, want the two uncarriable tracks", drops)
	}
}

// TestPCMEntriesMapToMatroska: every PCM sample entry maps to the Matroska
// PCM CodecID its layout has, or names the layout Matroska cannot hold.
func TestPCMEntriesMapToMatroska(t *testing.T) {
	mono := mkv.Track{Channels: u8p(1), SampleRate: f64p(44100)}
	pcmC := func(flags, bits uint8) []byte {
		return fullBox("pcmC", 0, 0, func(w *bw) { w.u8(flags); w.u8(bits) })
	}
	lpcm := func(bits, flags uint32) []byte {
		return boxf("lpcm", func(w *bw) {
			w.zeros(6)
			w.u16(1)
			w.u16(2) // version 2
			w.u16(0)
			w.u32(0)
			w.u16(3)
			w.u16(16)
			w.u16(0)
			w.u16(0)
			w.u32(0x00010000)
			w.u32(72)
			w.u64(math.Float64bits(48000))
			w.u32(2)
			w.u32(0x7F000000)
			w.u32(bits)
			w.u32(flags)
			w.u32(2 * bits / 8)
			w.u32(1)
		})
	}
	for _, tc := range []struct {
		name  string
		entry []byte
		codec string
		bits  uint8
		frame uint32
		bad   string
	}{
		{"twos", audioSampleEntry("twos", &mono, nil), "A_PCM/INT/BIG", 16, 2, ""},
		{"sowt", audioSampleEntry("sowt", &stereo48k, nil), "pcm", 16, 4, ""},
		{"ipcm le 24", audioSampleEntry("ipcm", &mono, pcmC(1, 24)), "pcm", 24, 3, ""},
		{"ipcm be 16", audioSampleEntry("ipcm", &mono, pcmC(0, 16)), "A_PCM/INT/BIG", 16, 2, ""},
		{"fpcm le 32", audioSampleEntry("fpcm", &mono, pcmC(1, 32)), "A_PCM/FLOAT/IEEE", 32, 4, ""},
		{"fpcm be 32", audioSampleEntry("fpcm", &mono, pcmC(0, 32)), "", 0, 0, "big-endian float"},
		{"fl64 quicktime be", audioSampleEntry("fl64", &mono, nil), "", 0, 0, "big-endian float"},
		{"sowt 8-bit signed", audioSampleEntry8("sowt", &mono), "", 0, 0, "signed 8-bit"},
		{"raw 8-bit unsigned", audioSampleEntry8("raw ", &mono), "pcm", 8, 1, ""},
		{"ipcm without pcmC", audioSampleEntry("ipcm", &mono, nil), "", 0, 0, "pcmC"},
		{"lpcm float le", lpcm(32, lpcmFloat), "A_PCM/FLOAT/IEEE", 32, 8, ""},
		{"lpcm float be", lpcm(64, lpcmFloat|lpcmBigEndian), "", 0, 0, "big-endian float"},
		{"lpcm int le 16", lpcm(16, lpcmSignedInteger), "pcm", 16, 4, ""},
		{"lpcm signed 8", lpcm(8, lpcmSignedInteger), "", 0, 0, "signed 8-bit"},
		{"lpcm int be 24", lpcm(24, lpcmBigEndian|lpcmSignedInteger), "A_PCM/INT/BIG", 24, 6, ""},
		{"lpcm non-interleaved", lpcm(16, lpcmSignedInteger|lpcmNonInterleaved), "", 0, 0, "non-interleaved"},
		{"lpcm unsigned 16", lpcm(16, 0), "", 0, 0, "unsigned"},
	} {
		var tr inTrack
		tr.trackType = mkv.AudioTrack
		described, _, err := parseSampleEntry(&tr, fullBox("stsd", 0, 0, func(w *bw) { w.u32(1); w.bytes(tc.entry) })[8:])
		if err != nil || !described {
			t.Fatalf("%s: described=%v err=%v", tc.name, described, err)
		}
		if tc.bad != "" {
			if !strings.Contains(tr.unsupported, tc.bad) {
				t.Errorf("%s: unsupported = %q, want it to mention %q", tc.name, tr.unsupported, tc.bad)
			}
			continue
		}
		if tr.unsupported != "" || tr.codec != tc.codec || tr.bitDepth != tc.bits || tr.pcmFrame != tc.frame {
			t.Errorf("%s: codec %q bits %d frame %d unsupported %q; want %q %d %d", tc.name, tr.codec, tr.bitDepth, tr.pcmFrame, tr.unsupported, tc.codec, tc.bits, tc.frame)
		}
	}
}

// audioSampleEntry8 is audioSampleEntry with an 8-bit samplesize.
func audioSampleEntry8(typ string, t *mkv.Track) []byte {
	e := audioSampleEntry(typ, t, nil)
	binary.BigEndian.PutUint16(e[8+18:], 8)
	return e
}

// TestCttsVersion0NegativeOffsetIsSigned: a version 0 ctts offset reads signed, like a trun's.
func TestCttsVersion0NegativeOffsetIsSigned(t *testing.T) {
	ctts := fullBox("ctts", 0, 0, func(w *bw) { w.u32(2); w.u32(1); w.u32(0xFFFFFE70); w.u32(1); w.u32(400) })
	got := parseCtts([]memBox{{typ: "ctts", payload: ctts[8:]}}, 2)
	if got[0] != -400 || got[1] != 400 {
		t.Errorf("ctts offsets = %v, want [-400 400]", got)
	}
}

// twoTrafMoof builds a moof with a traf for track 1 then one for track 2 whose tfhd states no anchor: its trun's
// data_offset is relative to the end of the first traf's data when chained, to the moof start otherwise.
func twoTrafMoof(sizes1, sizes2 []uint32, chained bool) ([]byte, []byte) {
	var payload []byte
	for _, s := range append(append([]uint32{}, sizes1...), sizes2...) {
		payload = append(payload, make([]byte, s)...)
	}
	var total1 int32
	for _, s := range sizes1 {
		total1 += int32(s)
	}
	traf := func(trackID uint32, sizes []uint32, dataOffset int32) []byte {
		tfhd := fullBox("tfhd", 0, 0, func(w *bw) { w.u32(trackID) })
		tfdt := fullBox("tfdt", 1, 0, func(w *bw) { w.u64(0) })
		trun := fullBox("trun", 1, trunDataOffset|trunSampleDuration|trunSampleSize, func(w *bw) {
			w.u32(uint32(len(sizes)))
			w.i32(dataOffset)
			for _, s := range sizes {
				w.u32(1024)
				w.u32(s)
			}
		})
		return container("traf", tfhd, tfdt, trun)
	}
	build := func(moofLen int32) []byte {
		second := moofLen + 8 + total1 // moof-relative: right after the first traf's data
		if chained {
			second = 0 // relative to the end of the first traf's data
		}
		return container("moof", fullBox("mfhd", 0, 0, func(w *bw) { w.u32(1) }), traf(1, sizes1, moofLen+8), traf(2, sizes2, second))
	}
	moof := build(0)
	moof = build(int32(len(moof)))
	return moof, box("mdat", payload)
}

// twoTrackAAC is fragmentedAAC with two audio tracks.
func twoTrackAAC(frags ...[]byte) []byte {
	entry := audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC))
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 48000, 0, entry, emptyTables()...),
		soundTrak(2, 48000, 0, entry, emptyTables()...),
		container("mvex", trexBox(1), trexBox(2)))...)
	for _, f := range frags {
		data = append(data, f...)
	}
	return data
}

// TestTrafDataAnchor: a traf after the first that states no anchor is read
// per ISO 14496-12, from the previous traf's data end; one whose samples would
// then fall past the fragment's data is anchored on the moof, as its muxer meant.
func TestTrafDataAnchor(t *testing.T) {
	for _, chained := range []bool{true, false} {
		moof, mdat := twoTrafMoof([]uint32{100, 50}, []uint32{30, 20}, chained)
		data := twoTrackAAC(moof, mdat)
		mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
		if err != nil {
			t.Fatalf("chained=%v: %v", chained, err)
		}
		mdatStart := int64(bytes.Index(data, []byte("mdat"))) + 4
		s := mv.tracks[1].samples
		if len(s) != 2 || s[0].offset != mdatStart+150 || s[1].offset != mdatStart+180 {
			t.Errorf("chained=%v: track 2 samples %+v, want offsets %d and %d", chained, s, mdatStart+150, mdatStart+180)
		}
	}
}

// pcmMP4 builds a progressive MP4 holding one twos track of one-frame samples laid out in chunks of the given frame counts.
func pcmMP4(t *testing.T, chunkFrames []int) ([]byte, []byte) {
	t.Helper()
	const frameBytes = 2
	var total int
	for _, n := range chunkFrames {
		total += n
	}
	pcm := make([]byte, total*frameBytes)
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	mono := mkv.Track{Channels: u8p(1), SampleRate: f64p(44100)}
	build := func(firstChunk uint32) []byte {
		stts := fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(uint32(total)); w.u32(1) })
		stsc := fullBox("stsc", 0, 0, func(w *bw) {
			w.u32(uint32(len(chunkFrames)))
			for i, n := range chunkFrames {
				w.u32(uint32(i + 1))
				w.u32(uint32(n))
				w.u32(1)
			}
		})
		stsz := fullBox("stsz", 0, 0, func(w *bw) { w.u32(frameBytes); w.u32(uint32(total)) })
		stco := fullBox("stco", 0, 0, func(w *bw) {
			w.u32(uint32(len(chunkFrames)))
			off := firstChunk
			for _, n := range chunkFrames {
				w.u32(off)
				off += uint32(n * frameBytes)
			}
		})
		moov := container("moov", mvhdBox(uint32(total*1000/44100)),
			soundTrak(1, 44100, uint32(total), audioSampleEntry("twos", &mono, nil), stts, stsc, stsz, stco))
		return append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), moov...)
	}
	head := build(0)
	head = build(uint32(len(head)) + 8)
	return append(head, box("mdat", pcm)...), pcm
}

// TestRemuxPCMPacketsMatchTheSource: a PCM track's one-frame samples become
// blocks of at most 1024 frames that never cross a chunk boundary - the
// packets the mainstream demuxer reads the source in - carrying every byte.
func TestRemuxPCMPacketsMatchTheSource(t *testing.T) {
	data, pcm := pcmMP4(t, []int{1500, 600, 2048})
	dir := t.TempDir()
	src := filepath.Join(dir, "pcm.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "pcm.mkv")
	if err := RemuxFromMP4(context.Background(), src, out); err != nil {
		t.Fatalf("RemuxFromMP4(pcm): %v", err)
	}
	c, blocks := readMKV(t, out)
	tr := c.Tracks[0]
	if tr.Codec != "A_PCM/INT/BIG" || tr.BitDepth == nil || *tr.BitDepth != 16 || tr.Channels == nil || *tr.Channels != 1 {
		t.Errorf("track = codec %q bits %v channels %v, want A_PCM/INT/BIG 16 1", tr.Codec, tr.BitDepth, tr.Channels)
	}
	var sizes []int
	var got []byte
	for _, b := range blocks {
		sizes = append(sizes, len(b.Data)/2)
		got = append(got, b.Data...)
	}
	want := []int{1024, 476, 600, 1024, 1024}
	if len(sizes) != len(want) || !equalInts(sizes, want) {
		t.Errorf("block frames = %v, want %v", sizes, want)
	}
	if !bytes.Equal(got, pcm) {
		t.Error("the blocks do not carry the source's PCM bytes")
	}
	if blocks[1].Timecode != 1024*1000/44100 {
		t.Errorf("second block at %d ms, want %d", blocks[1].Timecode, 1024*1000/44100)
	}
}

func equalInts(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trexBox(trackID uint32) []byte {
	return fullBox("trex", 0, 0, func(w *bw) { w.u32(trackID); w.u32(1); w.u32(0); w.u32(0); w.u32(0) })
}

// oneFragment builds a moof (tfhd + tfdt + trun of the given sizes, durations
// and composition offsets, trun version as given) and its mdat.
func oneFragment(trackID uint32, tfdt uint64, sizes, durs []uint32, cts []uint32, trunVersion uint8) ([]byte, []byte) {
	var payload []byte
	for _, s := range sizes {
		payload = append(payload, make([]byte, s)...)
	}
	build := func(dataOffset int32) []byte {
		tfhd := fullBox("tfhd", 0, tfhdDefaultBaseIsMoof, func(w *bw) { w.u32(trackID) })
		tfdtB := fullBox("tfdt", 1, 0, func(w *bw) { w.u64(tfdt) })
		flags := uint32(trunDataOffset | trunSampleDuration | trunSampleSize)
		if cts != nil {
			flags |= trunSampleCTS
		}
		trun := fullBox("trun", trunVersion, flags, func(w *bw) {
			w.u32(uint32(len(sizes)))
			w.i32(dataOffset)
			for i := range sizes {
				w.u32(durs[i])
				w.u32(sizes[i])
				if cts != nil {
					w.u32(cts[i])
				}
			}
		})
		return container("moof", fullBox("mfhd", 0, 0, func(w *bw) { w.u32(1) }), container("traf", tfhd, tfdtB, trun))
	}
	moof := build(0)
	moof = build(int32(len(moof)) + 8)
	return moof, box("mdat", payload)
}

// fragmentedAAC builds a fragmented one-track AAC file: a moov (mehd when
// mehdMs > 0) followed by the given fragments.
func fragmentedAAC(mehdMs uint32, frags ...[]byte) []byte {
	mvex := [][]byte{}
	if mehdMs > 0 {
		mvex = append(mvex, fullBox("mehd", 0, 0, func(w *bw) { w.u32(mehdMs) }))
	}
	mvex = append(mvex, trexBox(1))
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 48000, 0, audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC)), emptyTables()...),
		container("mvex", mvex...))...)
	for _, f := range frags {
		data = append(data, f...)
	}
	return data
}

// TestFragmentedDurationFromFragments: a fragmented file's duration is the
// mehd's, else the end of its fragments (last tfdt plus its trun's durations),
// on the head-only probe as on the keyframe probe.
func TestFragmentedDurationFromFragments(t *testing.T) {
	moof1, mdat1 := oneFragment(1, 0, []uint32{10, 10, 10}, []uint32{1024, 1024, 1024}, nil, 1)
	moof2, mdat2 := oneFragment(1, 3072, []uint32{10, 10}, []uint32{1024, 1024}, nil, 1)
	walked := fragmentedAAC(0, moof1, mdat1, moof2, mdat2)
	stated := fragmentedAAC(250, moof1, mdat1, moof2, mdat2)
	for _, tc := range []struct {
		name string
		data []byte
		opts Options
		want int64
	}{
		{"walked", walked, Options{}, 5120 * 1000 / 48000},
		{"walked, keyframes", walked, Options{Keyframes: true}, 5120 * 1000 / 48000},
		{"mehd", stated, Options{}, 250},
	} {
		c, _ := probeMovie(t, tc.data, tc.opts)
		if c.DurationMs != tc.want {
			t.Errorf("%s: DurationMs = %d, want %d", tc.name, c.DurationMs, tc.want)
		}
		if tc.name == "walked" && c.Tracks[0].DurationMs != tc.want {
			t.Errorf("%s: track DurationMs = %d, want %d", tc.name, c.Tracks[0].DurationMs, tc.want)
		}
	}
	// A moov that holds samples and fragments that run past them: the fragments' end wins.
	data, _ := pcmMP4(t, []int{441})
	moof, mdat := oneFragment(1, 441, []uint32{2, 2}, []uint32{22050, 22050}, nil, 1)
	partial := withMvex(t, append(append(append([]byte{}, data...), moof...), mdat...))
	if c, _ := probeMovie(t, partial); c.DurationMs != 1010 {
		t.Errorf("partially fragmented: DurationMs = %d, want 1010 (441 moov frames then two 0.5 s fragments)", c.DurationMs)
	}
}

// withMvex appends an mvex (trex for track 1) to the file's moov so its fragments are read.
func withMvex(t *testing.T, data []byte) []byte {
	t.Helper()
	off := bytes.Index(data, []byte("moov")) - 4
	size := int(binary.BigEndian.Uint32(data[off:]))
	mvex := container("mvex", trexBox(1))
	out := append([]byte{}, data[:off+size]...)
	out = append(out, mvex...)
	out = append(out, data[off+size:]...)
	binary.BigEndian.PutUint32(out[off:], uint32(size+len(mvex)))
	// the stco offsets moved by len(mvex)
	i := bytes.Index(out, []byte("stco"))
	n := int(binary.BigEndian.Uint32(out[i+8:]))
	for k := 0; k < n; k++ {
		p := i + 12 + 4*k
		binary.BigEndian.PutUint32(out[p:], binary.BigEndian.Uint32(out[p:])+uint32(len(mvex)))
	}
	return out
}

// TestTrunVersion0NegativeOffsetIsSigned: a version 0 trun's composition
// offsets read signed, as muxers write and demuxers read them; the remux
// then succeeds instead of placing a frame hours away.
func TestTrunVersion0NegativeOffsetIsSigned(t *testing.T) {
	moof, mdat := oneFragment(1, 0, []uint32{10, 10, 10}, []uint32{960, 960, 960}, []uint32{1920, uint32(0xFFFFFC40), 0}, 0) // +1920, -960, 0
	data := fragmentedAAC(0, moof, mdat)
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{mv.tracks[0].samples[0].ctsMs, mv.tracks[0].samples[1].ctsMs, mv.tracks[0].samples[2].ctsMs}
	if want := []int64{40, 0, 40}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("cts ms = %v, want %v", got, want)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "v0.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemuxFromMP4(context.Background(), src, filepath.Join(dir, "v0.mkv")); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
}

// TestClusterSplitsOnWideComposition: two blocks of one decode window whose
// presentation times lie further apart than a SimpleBlock's int16 offset go
// to separate clusters instead of failing the remux.
func TestClusterSplitsOnWideComposition(t *testing.T) {
	moof, mdat := oneFragment(1, 0, []uint32{10, 10}, []uint32{1024, 1024}, []uint32{0, 1920000}, 1) // the second frame shown 40 s later
	data := fragmentedAAC(0, moof, mdat)
	dir := t.TempDir()
	src := filepath.Join(dir, "wide.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "wide.mkv")
	if err := RemuxFromMP4(context.Background(), src, out); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
	_, blocks := readMKV(t, out)
	if len(blocks) != 2 || blocks[0].Timecode != 0 || blocks[1].Timecode != (1024+1920000)*1000/48000 {
		t.Errorf("blocks = %+v, want two at 0 and 40021 ms", blocks)
	}
}

// TestRemuxRefusesAnInitSegment: a fragmented moov without fragments holds no samples: nothing to remux, said so.
func TestRemuxRefusesAnInitSegment(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "init.mp4")
	if err := os.WriteFile(src, fragmentedAAC(0), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RemuxFromMP4(context.Background(), src, filepath.Join(dir, "init.mkv"))
	if err == nil || !strings.Contains(err.Error(), "no samples") {
		t.Errorf("err = %v, want the absence of samples named", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "init.mkv")); serr == nil {
		t.Error("an output was left behind")
	}
}

// encryptedWebM writes a WebM whose one track declares AES-CTR ContentEncryption around one block.
func encryptedWebM(t *testing.T, codecID string) string {
	t.Helper()
	el := func(id uint32, body []byte) []byte {
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, id, int64(len(body)))
		b.Write(body)
		return b.Bytes()
	}
	uint := func(id uint32, v uint64) []byte {
		var b bytes.Buffer
		ebml.WriteElementHeader(&b, id, 1)
		ebml.WriteUint(&b, v, 1)
		return b.Bytes()
	}
	str := func(id uint32, s string) []byte { return el(id, []byte(s)) }
	enc := el(mkv.IDContentEncodings, el(mkv.IDContentEncoding,
		append(uint(mkv.IDContentEncodingType, 1),
			el(mkv.IDContentEncryption, append(uint(mkv.IDContentEncAlgo, 5),
				el(mkv.IDContentEncAESSettings, uint(mkv.IDAESSettingsCipherMode, 1))...))...)))
	trackType := uint64(mkv.TrackTypeAudio)
	if strings.HasPrefix(codecID, "V_") {
		trackType = mkv.TrackTypeVideo
	}
	var te []byte
	te = append(te, uint(mkv.IDTrackNumber, 1)...)
	te = append(te, uint(mkv.IDTrackType, trackType)...)
	te = append(te, str(mkv.IDCodecID, codecID)...)
	if trackType == mkv.TrackTypeVideo {
		te = append(te, el(mkv.IDVideo, append(uint(mkv.IDPixelWidth, 16), uint(mkv.IDPixelHeight, 16)...))...)
	}
	te = append(te, enc...)
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 4)
	ebml.WriteUint(&info, 1_000_000, 4)
	block := append([]byte{0x81, 0, 0, 0x80}, 1, 0x22, 0x33, 0x44) // track 1, timecode 0, keyframe, a signal byte and bytes
	cluster := el(mkv.IDCluster, append(uint(mkv.IDTimestamp, 0), el(mkv.IDSimpleBlock, block)...))
	var seg bytes.Buffer
	seg.Write(el(mkv.IDInfo, info.Bytes()))
	seg.Write(el(mkv.IDTracks, el(mkv.IDTrackEntry, te)))
	seg.Write(cluster)
	var f bytes.Buffer
	if err := writer.WriteEBMLHeader(&f); err != nil {
		t.Fatal(err)
	}
	f.Write(el(mkv.IDSegment, seg.Bytes()))
	path := filepath.Join(t.TempDir(), "enc.webm")
	if err := os.WriteFile(path, f.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRemuxToMP4RefusesEncryptedTracks: an encrypted Matroska track is
// refused by name - before any frame is parsed, so an encrypted VP9 track is
// not reported as a bad frame.
func TestRemuxToMP4RefusesEncryptedTracks(t *testing.T) {
	for _, codec := range []string{"A_OPUS", "V_VP9"} {
		src := encryptedWebM(t, codec)
		err := RemuxToMP4(context.Background(), src, filepath.Join(t.TempDir(), "out.mp4"))
		if err == nil || !strings.Contains(err.Error(), "encrypted (AES-CTR)") {
			t.Errorf("%s: RemuxToMP4 err = %v, want the track named encrypted (AES-CTR)", codec, err)
		}
		err = RemuxToHLS(context.Background(), src, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "encrypted (AES-CTR)") {
			t.Errorf("%s: RemuxToHLS err = %v, want the track named encrypted (AES-CTR)", codec, err)
		}
	}
}

// fragmentWithoutTfdt is oneFragment without a tfdt: the fragment's decode time is where the track's previous one ended.
func fragmentWithoutTfdt(trackID uint32, sizes, durs []uint32) ([]byte, []byte) {
	moof, mdat := oneFragment(trackID, 0, sizes, durs, nil, 1)
	tfdt := fullBox("tfdt", 1, 0, func(w *bw) { w.u64(0) })
	i := bytes.Index(moof, tfdt)
	out := append(append([]byte{}, moof[:i]...), moof[i+len(tfdt):]...)
	// the moof, traf and trun data_offset shrink by the tfdt's length
	binary.BigEndian.PutUint32(out[0:], uint32(len(out)))
	traf := bytes.Index(out, []byte("traf")) - 4
	binary.BigEndian.PutUint32(out[traf:], binary.BigEndian.Uint32(out[traf:])-uint32(len(tfdt)))
	trun := bytes.Index(out, []byte("trun")) - 4
	off := trun + 8 + 8
	binary.BigEndian.PutUint32(out[off:], uint32(int32(binary.BigEndian.Uint32(out[off:]))-int32(len(tfdt))))
	return out, mdat
}

// TestFragmentsWithoutTfdtContinue: a fragment without a tfdt starts where
// the track's previous fragment ended, on the sample table as on the probe's
// duration - not at zero, which collapsed every fragment onto the first.
func TestFragmentsWithoutTfdtContinue(t *testing.T) {
	moof1, mdat1 := fragmentWithoutTfdt(1, []uint32{10, 10, 10}, []uint32{1024, 1024, 1024})
	moof2, mdat2 := fragmentWithoutTfdt(1, []uint32{10, 10}, []uint32{1024, 1024})
	data := fragmentedAAC(0, moof1, mdat1, moof2, mdat2)
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	s := mv.tracks[0].samples
	if len(s) != 5 || s[3].dtsMs != 3072*1000/48000 || s[4].dtsMs != 4096*1000/48000 {
		t.Errorf("samples = %d, fragment 2 at %d and %d ms; want 5 samples at 64 and 85 ms", len(s), s[3].dtsMs, s[4].dtsMs)
	}
	if c, _ := probeMovie(t, data); c.DurationMs != 5120*1000/48000 {
		t.Errorf("probe DurationMs = %d, want %d", c.DurationMs, 5120*1000/48000)
	}
}

// baselineSPSAvcC is an avcC whose SPS states a 320x240 picture (20x15 macroblocks, no cropping).
func baselineSPSAvcC() []byte {
	bits := "1" + "1" + "1" + "1" + "010" + "0" + "000010100" + "0001111" + "1" + "1" + "0" + "0" + "1" // ids and counts, 20x15 MBs, frame_mbs_only, no crop, no VUI, trailing bit
	var sps []byte
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for k := 0; k < 8; k++ {
			b <<= 1
			if i+k < len(bits) && bits[i+k] == '1' {
				b |= 1
			}
		}
		sps = append(sps, b)
	}
	sps = append([]byte{0x67, 66, 0, 30}, sps...) // NAL header, profile_idc 66, constraints, level_idc
	avcC := []byte{1, 66, 0, 30, 0xFF, 0xE1, byte(len(sps) >> 8), byte(len(sps))}
	avcC = append(avcC, sps...)
	return append(avcC, 1, 0, 4, 0x68, 0xCE, 0x3C, 0x80) // one PPS
}

// videoTrak builds a trak of handler "vide" around one sample entry.
func videoTrak(trackID uint32, entry []byte) []byte {
	trak := soundTrak(trackID, 30000, 30000, entry, emptyTables()...)
	return bytes.Replace(trak, []byte("soun"), []byte("vide"), 1)
}

// TestZeroPictureSizeFromSPSOrRefused: a video entry stating a 0x0 picture
// takes its size from the SPS, and is refused by name when the codec
// configuration states none - Matroska PixelWidth/PixelHeight are never 0.
func TestZeroPictureSizeFromSPSOrRefused(t *testing.T) {
	zeroAVC := visualSampleEntry("avc1", &mkv.Track{Width: u32p(0), Height: u32p(0)}, box("avcC", baselineSPSAvcC()))
	zeroVP9 := visualSampleEntry("vp09", &mkv.Track{Width: u32p(0), Height: u32p(64530)}, fullBox("vpcC", 1, 0, func(w *bw) { w.u32(0); w.u32(0); w.u16(0) }))
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0), videoTrak(1, zeroAVC), videoTrak(2, zeroVP9))...)
	c, _ := probeMovie(t, data)
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(c.Tracks))
	}
	if avc := c.Tracks[0]; avc.Width == nil || *avc.Width != 320 || avc.Height == nil || *avc.Height != 240 {
		t.Errorf("avc1 0x0: size %v x %v, want 320x240 from the SPS", avc.Width, avc.Height)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "zero.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err := RemuxFromMP4(context.Background(), src, filepath.Join(dir, "zero.mkv"))
	if err == nil || !strings.Contains(err.Error(), "track 2 (vp9)") || !strings.Contains(err.Error(), "0x64530") {
		t.Errorf("remux err = %v, want the vp09 track refused for its 0x64530 picture", err)
	}
}

// TestRemuxDropsUncarriableSubtitles: a subtitle track the output cannot
// carry never fails from-mp4; it is dropped with its reason, as every
// operation that writes MP4 drops an ASS or PGS track.
func TestRemuxDropsUncarriableSubtitles(t *testing.T) {
	stpp := box("stpp", append(make([]byte, 8), "\x00\x00\x00"...)) // an XMLSubtitleSampleEntry: namespace, schema, mime
	ttml := soundTrak(2, 1000, 1000, stpp, emptyTables()...)
	ttml = bytes.Replace(ttml, []byte("soun"), []byte("subt"), 1)
	moof, mdat := oneFragment(1, 0, []uint32{10, 10}, []uint32{1024, 1024}, nil, 1)
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0),
		soundTrak(1, 48000, 0, audioSampleEntry("mp4a", &stereo48k, esdsBox(0x40, fakeASC)), emptyTables()...),
		ttml, container("mvex", trexBox(1), trexBox(2)))...)
	data = append(append(data, moof...), mdat...)
	dir := t.TempDir()
	src := filepath.Join(dir, "ttml.mp4")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var drops []DroppedTrack
	err := RemuxFromMP4(context.Background(), src, filepath.Join(dir, "out.mkv"), Options{OnDrop: func(d DroppedTrack) { drops = append(drops, d) }})
	if err != nil {
		t.Fatalf("RemuxFromMP4 with a TTML track: %v, want the track dropped, not the remux refused", err)
	}
	if len(drops) != 1 || drops[0].ID != 2 || drops[0].Type != mkv.SubtitleTrack || drops[0].Codec != "stpp" || !strings.Contains(drops[0].Reason, "stpp") {
		t.Errorf("drops = %+v, want the stpp subtitle track 2 with its reason", drops)
	}
	c, _ := readMKV(t, filepath.Join(dir, "out.mkv"))
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "aac" {
		t.Errorf("output tracks = %+v, want the aac track alone", c.Tracks)
	}
}

// TestProResBitDepthFromFourcc: a ProRes entry states its bit depth by its
// fourcc - 10 up to HQ, 12 for 4444 - and ProRes RAW or an unknown fourcc
// states none, as the mainstream decoder reads them.
func TestProResBitDepthFromFourcc(t *testing.T) {
	for _, tc := range []struct {
		fourcc string
		depth  uint16
	}{
		{"apco", 10}, {"apcs", 10}, {"apcn", 10}, {"apch", 10}, {"ap4h", 12}, {"ap4x", 12}, {"aprn", 0}, {"aprh", 0}, {"xxxx", 0},
	} {
		entry := visualSampleEntry(tc.fourcc, &mkv.Track{Width: u32p(720), Height: u32p(576)}, nil)
		data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", mvhdBox(0), videoTrak(1, entry))...)
		c, _ := probeMovie(t, data)
		got := c.Tracks[0].VideoBitDepth
		switch {
		case tc.depth == 0 && got != nil:
			t.Errorf("%s: video_bit_depth %d, want none", tc.fourcc, *got)
		case tc.depth != 0 && (got == nil || *got != tc.depth):
			t.Errorf("%s: video_bit_depth %v, want %d", tc.fourcc, got, tc.depth)
		}
	}
}
