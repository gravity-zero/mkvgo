package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

// isoBox is an ISO BMFF box of type typ around payload.
func isoBox(typ string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	b := make([]byte, 8, n)
	binary.BigEndian.PutUint32(b, uint32(n))
	copy(b[4:], typ)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

// fullV1 is a version-1 header (creation, modification) then a timescale and an 8-byte duration.
func fullV1(ts uint32, dur uint64) []byte {
	b := make([]byte, 32)
	b[0] = 1
	binary.BigEndian.PutUint32(b[20:], ts)
	binary.BigEndian.PutUint64(b[24:], dur)
	return b
}

func TestROB04_DurationNeverNegative(t *testing.T) {
	for _, dur := range []uint64{1 << 61, 1<<62 - 1, 1<<53 + 7, 9_300_000_000_000_000} {
		for _, ts := range []uint32{1, 7, 1000, 90000} {
			mv, err := parseMoov(isoBox("mvhd", fullV1(ts, dur)), 1<<40, sampleNone)
			if err == nil && mv.durationMs < 0 {
				t.Errorf("mvhd %d ticks at %d/s: duration %d ms", dur, ts, mv.durationMs)
			}
			mehd := make([]byte, 12)
			mehd[0] = 1
			binary.BigEndian.PutUint64(mehd[4:], dur)
			if ms := mehdDurationMs([]memBox{{typ: "mvex", payload: isoBox("mehd", mehd)}}, ts); ms < 0 {
				t.Errorf("mehd %d ticks at %d/s: duration %d ms", dur, ts, ms)
			}
			mdhd := fullV1(ts, dur)
			if ms := mdhdDurationMs(mdhd); ms < 0 {
				t.Errorf("mdhd %d ticks at %d/s: duration %d ms", dur, ts, ms)
			}
		}
	}
	if ms := mdhdDurationMs(fullV1(1000, 3_600_000)); ms != 3_600_000 {
		t.Fatalf("an hour at 1000/s reads %d ms", ms)
	}
	if ms := mdhdDurationMs(fullV1(90000, 1<<61)); ms != 25620477880152155 {
		t.Fatalf("2^61 ticks at 90 kHz reads %d ms, want 25620477880152155", ms)
	}
}

// withMvhdV1 rewrites the fixture's version-0 mvhd (moov last in the file) as version 1 with dur ticks at ts.
func withMvhdV1(t *testing.T, path string, ts uint32, dur uint64) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moov := bytes.LastIndex(data, []byte("moov")) - 4
	i := bytes.Index(data[moov:], []byte("mvhd")) + moov - 4
	old := data[i : i+int(binary.BigEndian.Uint32(data[i:]))]
	if old[8] != 0 {
		t.Fatal("fixture mvhd is not version 0")
	}
	v1 := fullV1(ts, dur)
	v1 = append(v1, old[8+20:]...) // rate, volume, matrix, next track ID: unchanged after the version 0 duration
	out := append(append(append([]byte{}, data[:i]...), isoBox("mvhd", v1)...), data[i+len(old):]...)
	binary.BigEndian.PutUint32(out[moov:], binary.BigEndian.Uint32(out[moov:])+12)
	return out
}

func TestROB04_MvhdDurationThroughReadMeta(t *testing.T) {
	for _, dur := range []uint64{1 << 61, 1<<62 - 1, 9_300_000_000_000_000} {
		data := withMvhdV1(t, "../internal/testdata/interleaved.mp4", 1, dur)
		c, _, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mp4")
		if err != nil {
			t.Fatal(err)
		}
		mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleNone)
		if err != nil {
			t.Fatal(err)
		}
		if mv.durationMs < 0 {
			t.Errorf("mvhd of %d ticks at 1/s parses as %d ms", dur, mv.durationMs)
		}
		if c.Info.Duration < 0 || c.DurationMs < 0 {
			t.Errorf("mvhd of %d ticks at 1/s: Duration %v, %d ms", dur, c.Info.Duration, c.DurationMs)
		}
	}
}
