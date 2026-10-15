package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
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
