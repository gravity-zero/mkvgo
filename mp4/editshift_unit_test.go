package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildTkhdPayload builds a tkhd payload of the given version with the given
// duration, matching the ISO layout the duration-field locator expects.
func buildTkhdPayload(version uint8, dur int64) []byte {
	var w bw
	w.u8(version)
	w.u24(7)
	if version == 1 {
		w.u64(0) // creation
		w.u64(0) // modification
		w.u32(1) // track_ID
		w.u32(0) // reserved
		w.u64(uint64(dur))
	} else {
		w.u32(0)
		w.u32(0)
		w.u32(1)
		w.u32(0)
		w.u32(uint32(dur))
	}
	w.zeros(60) // layer/group/volume/matrix/width/height
	return w.b
}

func buildMvhdPayload(version uint8, ts uint32, dur int64) []byte {
	var w bw
	w.u8(version)
	w.u24(0)
	if version == 1 {
		w.u64(0)
		w.u64(0)
		w.u32(ts)
		w.u64(uint64(dur))
	} else {
		w.u32(0)
		w.u32(0)
		w.u32(ts)
		w.u32(uint32(dur))
	}
	w.zeros(80)
	return w.b
}

// TestTkhdMvhdDurationFieldBothVersions: the duration locator and patcher
// handle the v0 (32-bit) and v1 (64-bit) header forms.
func TestTkhdMvhdDurationFieldBothVersions(t *testing.T) {
	for _, version := range []uint8{0, 1} {
		want := int64(90_000)
		if version == 1 {
			want = int64(0x1_0000_0000) // does not fit 32 bits: v1's reason to exist
		}
		p := buildTkhdPayload(version, want)
		_, got, err := tkhdDurationField(p)
		if err != nil || got != want {
			t.Errorf("v%d: tkhd duration = %d (%v), want %d", version, got, err, want)
		}
		patched, err := patchTkhdDuration(p, want+500)
		if err != nil {
			t.Fatal(err)
		}
		if _, got, _ := tkhdDurationField(patched); got != want+500 {
			t.Errorf("v%d: patched tkhd duration = %d, want %d", version, got, want+500)
		}

		mp := buildMvhdPayload(version, 1000, want)
		mpPatched, err := patchMvhdDuration(mp, want+500)
		if err != nil {
			t.Fatal(err)
		}
		ts, dur := parseMovieHeader(mpPatched)
		if ts != 1000 || int64(dur) != want+500 {
			t.Errorf("v%d: patched mvhd = ts %d dur %d, want 1000/%d", version, ts, dur, want+500)
		}
	}
	// Truncated payloads refuse rather than mis-locate.
	if _, _, err := tkhdDurationField([]byte{1, 0, 0, 0}); err == nil {
		t.Error("short v1 tkhd must refuse")
	}
	if _, err := patchMvhdDuration([]byte{1, 0, 0}, 1); err == nil {
		t.Error("short mvhd must refuse")
	}
}

// TestElstEntriesBothVersions: v1 (64-bit) edit lists parse, and rebuilding
// promotes to v1 exactly when a value does not fit the 32-bit form.
func TestElstEntriesBothVersions(t *testing.T) {
	entries := []elstEntry{
		{segDur: 0x1_2345_6789, mediaTime: -1, rate: 1 << 16}, // needs v1
		{segDur: 90_000, mediaTime: 1024, rate: 1 << 16},
	}
	edts := buildElstBox(entries)
	eb, err := iterBoxes(edts[8:]) // skip the edts header
	if err != nil {
		t.Fatal(err)
	}
	elst, ok := findMemBox(eb, "elst")
	if !ok || elst.payload[0] != 1 {
		t.Fatalf("an entry beyond 32 bits must select elst v1 (version=%d)", elst.payload[0])
	}
	got, err := trakElstEntries([]memBox{{typ: "edts", payload: edts[8:]}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != entries[0] || got[1] != entries[1] {
		t.Errorf("v1 round trip = %+v, want %+v", got, entries)
	}

	// Small values stay on the compact v0 form.
	small := buildElstBox([]elstEntry{{segDur: 300, mediaTime: -1, rate: 1 << 16}})
	eb, _ = iterBoxes(small[8:])
	if elst, _ := findMemBox(eb, "elst"); elst.payload[0] != 0 {
		t.Error("small values must keep elst v0")
	}
	// A truncated v1 list refuses.
	bad := append([]byte(nil), elst.payload[:10]...)
	bad[0] = 1
	binary.BigEndian.PutUint32(bad[4:8], 2)
	if _, err := trakElstEntries([]memBox{{typ: "edts", payload: box("elst", bad)}}); err == nil {
		t.Error("truncated elst v1 must refuse")
	}
}

// TestScanTopLevelLayout64Bit: the largesize header form locates the moov.
func TestScanTopLevelLayout64Bit(t *testing.T) {
	var w bw
	w.bytes(box("ftyp", []byte("isom0000")))
	// moov in the 64-bit largesize form.
	moovPayload := box("mvhd", buildMvhdPayload(0, 1000, 42))
	w.u32(1)
	w.fourcc("moov")
	w.u64(uint64(16 + len(moovPayload)))
	w.bytes(moovPayload)
	w.bytes(box("mdat", []byte{1, 2, 3, 4}))

	f := &memSeekCloser{Reader: *bytes.NewReader(w.b)}
	lay, err := scanTopLevelLayout(f)
	if err != nil {
		t.Fatal(err)
	}
	if lay.moovHdr != 16 {
		t.Errorf("moovHdr = %d, want 16 (largesize form)", lay.moovHdr)
	}
	if lay.moovOff != 16 || lay.moovSize != int64(16+len(moovPayload)) {
		t.Errorf("moov located at %d size %d", lay.moovOff, lay.moovSize)
	}
}

// TestScanTopLevelLayoutRefusesSize0: a last box declaring size 0 runs to the
// END OF THE FILE. The repair lands its moov by appending, so that box would
// simply grow over the new moov - turning it into payload - while the old moov
// got retired to a free box: a file with no reachable movie header, reported as
// a success. The scan refuses the layout instead of locating a moov in it.
func TestScanTopLevelLayoutRefusesSize0(t *testing.T) {
	var w bw
	w.bytes(box("ftyp", []byte("isom0000")))
	w.bytes(box("moov", box("mvhd", buildMvhdPayload(0, 1000, 42))))
	w.u32(0) // mdat, size 0 = to EOF (what streaming writers emit)
	w.fourcc("mdat")
	w.bytes([]byte{1, 2, 3, 4})

	f := &memSeekCloser{Reader: *bytes.NewReader(w.b)}
	if _, err := scanTopLevelLayout(f); err == nil {
		t.Fatal("a size-0 (to-EOF) last box must be refused: appending a moov would extend it")
	} else if !strings.Contains(err.Error(), "size 0") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestScanTopLevelLayoutRefusesTrailingJunk: bytes past the last box would sit
// between the last box and the appended moov, desyncing a strict forward walk.
// mp4.Diagnose already flags this shape ("trailing-junk", remedy: remux); the
// repair used to walk right past it and append anyway.
func TestScanTopLevelLayoutRefusesTrailingJunk(t *testing.T) {
	var w bw
	w.bytes(box("ftyp", []byte("isom0000")))
	w.bytes(box("moov", box("mvhd", buildMvhdPayload(0, 1000, 42))))
	w.bytes(box("mdat", []byte{1, 2, 3, 4}))
	w.bytes([]byte{0xDE, 0xAD, 0xBE}) // 3 bytes: too short to be a box header

	f := &memSeekCloser{Reader: *bytes.NewReader(w.b)}
	if _, err := scanTopLevelLayout(f); err == nil {
		t.Fatal("junk past the last box must be refused")
	} else if !strings.Contains(err.Error(), "junk") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestScanTopLevelLayoutFindsFirstMoovAndStrays: an interrupted repair leaves
// its appended moov behind, so a file can carry two. The live one - what a
// forward walk (and mp4.Open) reads - is the FIRST; the scan must report that
// one and list the rest as strays. Taking the last would retime an already
// shifted moov and leave the head one, the one players read, untouched.
func TestScanTopLevelLayoutFindsFirstMoovAndStrays(t *testing.T) {
	var w bw
	ftyp := box("ftyp", []byte("isom0000"))
	w.bytes(ftyp)
	live := box("moov", box("mvhd", buildMvhdPayload(0, 1000, 42)))
	w.bytes(live)
	mdat := box("mdat", []byte{1, 2, 3, 4})
	w.bytes(mdat)
	strayOff := int64(len(ftyp) + len(live) + len(mdat))
	w.bytes(box("moov", box("mvhd", buildMvhdPayload(0, 1000, 99))))

	f := &memSeekCloser{Reader: *bytes.NewReader(w.b)}
	lay, err := scanTopLevelLayout(f)
	if err != nil {
		t.Fatal(err)
	}
	if lay.moovOff != int64(len(ftyp)) {
		t.Errorf("moovOff = %d, want %d (the FIRST moov, the one a forward walk reads)",
			lay.moovOff, len(ftyp))
	}
	if len(lay.strayMoovs) != 1 || lay.strayMoovs[0] != strayOff {
		t.Errorf("strayMoovs = %v, want [%d]", lay.strayMoovs, strayOff)
	}
}

// memSeekCloser adapts a bytes.Reader to the RW handle scanTopLevelLayout
// reads through (writes unused).
type memSeekCloser struct{ bytes.Reader }

func (m *memSeekCloser) Write(p []byte) (int, error) { return 0, os.ErrPermission }
func (m *memSeekCloser) Close() error                { return nil }

// noSyncFile hides the Sync method of the real handle, so the crash-safe
// swap's hard requirement is observable.
type noSyncFile struct{ mkv.ReadWriteSeekCloser }

// TestMP4RetimeTracksRequiresSync: a handle that cannot Sync refuses rather
// than silently degrading the crash-ordered moov swap.
func TestMP4RetimeTracksRequiresSync(t *testing.T) {
	path := editShiftFixture(t, false)
	fs := &mkv.FS{OpenFile: func(p string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
		f, err := os.OpenFile(p, flag, perm)
		if err != nil {
			return nil, err
		}
		return noSyncFile{f}, nil
	}}
	err := RetimeTracks(context.Background(), path, map[uint64]int64{2: 300_000_000}, Options{FS: fs})
	if err == nil || !strings.Contains(err.Error(), "Sync") {
		t.Errorf("a Sync-less handle must refuse the crash-safe swap, got %v", err)
	}
}
