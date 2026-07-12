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

// TestScanTopLevelLayout64BitAndSize0: the largesize header form and the
// size-0 (to end of file) form both locate the moov.
func TestScanTopLevelLayout64BitAndSize0(t *testing.T) {
	var w bw
	w.bytes(box("ftyp", []byte("isom0000")))
	// moov in the 64-bit largesize form.
	moovPayload := box("mvhd", buildMvhdPayload(0, 1000, 42))
	w.u32(1)
	w.fourcc("moov")
	w.u64(uint64(16 + len(moovPayload)))
	w.bytes(moovPayload)
	// trailing mdat with size 0 = to EOF.
	w.u32(0)
	w.fourcc("mdat")
	w.bytes([]byte{1, 2, 3, 4})

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
