package reader

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// --- fixture builders (reuse uintElem/strElem/masterElem/trackEntry from probe_test.go) ---

func floatElem(id uint32, v float64) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, id, 8)
	ebml.WriteFloat(&b, v)
	return b.Bytes()
}

func infoElem() []byte {
	return masterElem(mkv.IDInfo,
		uintElem(mkv.IDTimecodeScale, 1_000_000, 4),
		floatElem(mkv.IDDuration, 1234.0),
		strElem(mkv.IDTitle, "meta-fixture"),
	)
}

// audioTrackEntry: FR audio, explicit default, with channels/sample-rate/bit-depth.
func audioTrackEntry() []byte {
	audio := masterElem(mkv.IDAudio,
		floatElem(mkv.IDSamplingFreq, 48000.0),
		uintElem(mkv.IDChannels, 6, 1),
		uintElem(mkv.IDBitDepth, 24, 2),
	)
	return trackEntry(
		uintElem(mkv.IDTrackNumber, 2, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_AC3"),
		strElem(mkv.IDLanguage, "fre"),
		uintElem(mkv.IDFlagDefault, 1, 1),
		audio,
	)
}

// tracksElem: the HDR video entry (from probe_test.go) + the audio entry, so the
// parity check covers colour, BCP47-absent/presence, channels, sample rate, etc.
func tracksElem() []byte {
	return masterElem(mkv.IDTracks, hevcHDRTrackEntry(), audioTrackEntry())
}

func clusterElem() []byte {
	return masterElem(mkv.IDCluster, uintElem(mkv.IDTimestamp, 0, 1))
}

func segmentMKV(children ...[]byte) []byte {
	var body bytes.Buffer
	for _, ch := range children {
		body.Write(ch)
	}
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, int64(body.Len()))
	buf.Write(body.Bytes())
	return buf.Bytes()
}

func idBytes(id uint32) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// seekEntry uses a fixed 8-byte SeekPosition so the SeekHead's serialized length
// is independent of the position values (lets us compute offsets in one pass).
func seekEntry(id uint32, pos uint64) []byte {
	sid := idBytes(id)
	var seekid bytes.Buffer
	ebml.WriteElementHeader(&seekid, mkv.IDSeekID, int64(len(sid)))
	seekid.Write(sid)
	var seekpos bytes.Buffer
	ebml.WriteElementHeader(&seekpos, mkv.IDSeekPosition, 8)
	ebml.WriteUint(&seekpos, pos, 8)
	return masterElem(mkv.IDSeek, seekid.Bytes(), seekpos.Bytes())
}

func seekHeadElem(entries ...[]byte) []byte { return masterElem(mkv.IDSeekHead, entries...) }

// buildSeekHeadMKV builds [SeekHead][Info][Tracks][Cluster], or, when
// tracksAfterCluster, [SeekHead][Info][Cluster][Tracks] - the case that REQUIRES
// the SeekHead to locate Tracks. SeekPositions are relative to the Segment data.
func buildSeekHeadMKV(t *testing.T, tracksAfterCluster bool) []byte {
	t.Helper()
	info, tracks, cluster := infoElem(), tracksElem(), clusterElem()
	l := uint64(len(seekHeadElem(seekEntry(mkv.IDInfo, 0), seekEntry(mkv.IDTracks, 0))))

	infoPos := l
	var tracksPos uint64
	var body [][]byte
	if tracksAfterCluster {
		tracksPos = l + uint64(len(info)) + uint64(len(cluster))
	} else {
		tracksPos = l + uint64(len(info))
	}
	sh := seekHeadElem(seekEntry(mkv.IDInfo, infoPos), seekEntry(mkv.IDTracks, tracksPos))
	if uint64(len(sh)) != l {
		t.Fatalf("SeekHead length not fixed-width: %d vs %d", len(sh), l)
	}
	if tracksAfterCluster {
		body = [][]byte{sh, info, cluster, tracks}
	} else {
		body = [][]byte{sh, info, tracks, cluster}
	}
	return segmentMKV(body...)
}

func buildPlainMKV() []byte { return segmentMKV(infoElem(), tracksElem(), clusterElem()) }

// TestReadMetaParity asserts ReadMeta returns Tracks + Info + DurationMs
// byte-identical to a full Read, across a file with a SeekHead, one without, and
// one whose Tracks sits after the first Cluster (reachable only via SeekHead).
func TestReadMetaParity(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"with SeekHead", buildSeekHeadMKV(t, false)},
		{"without SeekHead", buildPlainMKV()},
		{"tracks after cluster via SeekHead", buildSeekHeadMKV(t, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, err := Read(context.Background(), bytes.NewReader(tc.data), "x.mkv")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			meta, err := ReadMeta(context.Background(), bytes.NewReader(tc.data), "x.mkv")
			if err != nil {
				t.Fatalf("ReadMeta: %v", err)
			}

			if len(meta.Tracks) != 2 {
				t.Fatalf("meta tracks = %d, want 2", len(meta.Tracks))
			}
			if !reflect.DeepEqual(full.Tracks, meta.Tracks) {
				t.Errorf("Tracks differ:\n full = %+v\n meta = %+v", full.Tracks, meta.Tracks)
			}
			if !reflect.DeepEqual(full.Info, meta.Info) {
				t.Errorf("Info differ:\n full = %+v\n meta = %+v", full.Info, meta.Info)
			}
			if full.DurationMs != meta.DurationMs {
				t.Errorf("DurationMs: full=%d meta=%d", full.DurationMs, meta.DurationMs)
			}
			// Documented: meta leaves the rest nil.
			if meta.Chapters != nil || meta.Attachments != nil || meta.Tags != nil || meta.Cues != nil {
				t.Errorf("meta must leave Chapters/Attachments/Tags/Cues nil, got %v/%v/%v/%v",
					meta.Chapters, meta.Attachments, meta.Tags, meta.Cues)
			}
		})
	}
}

// countingReadSeeker counts bytes actually Read (Seek transfers nothing).
type countingReadSeeker struct {
	rs   io.ReadSeeker
	read int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := c.rs.Read(p)
	c.read += int64(n)
	return n, err
}
func (c *countingReadSeeker) Seek(off int64, whence int) (int64, error) {
	return c.rs.Seek(off, whence)
}

func cuePoint(time, track, pos uint64) []byte {
	ctp := masterElem(mkv.IDCueTrackPositions,
		uintElem(mkv.IDCueTrack, track, 1),
		uintElem(mkv.IDCueClusterPos, pos, 4),
	)
	return masterElem(mkv.IDCuePoint, uintElem(mkv.IDCueTime, time, 4), ctp)
}

func cuesElem(n int) []byte {
	pts := make([][]byte, n)
	for i := range pts {
		pts[i] = cuePoint(uint64(i*1000), 1, uint64(i*5000))
	}
	return masterElem(mkv.IDCues, pts...)
}

// TestReadMetaStopsEarly proves ReadMeta skips the body: with a large Cues index
// after Info+Tracks (the element a full Read parses point-by-point - the bench's
// main time sink), ReadMeta stops before it, reading a small buffer-bounded
// amount, while the full Read pulls the whole Cues index.
func TestReadMetaStopsEarly(t *testing.T) {
	cues := cuesElem(2000) // ~tens of KB of cue points
	data := segmentMKV(infoElem(), tracksElem(), cues, clusterElem())

	metaCR := &countingReadSeeker{rs: bytes.NewReader(data)}
	meta, err := ReadMeta(context.Background(), metaCR, "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	fullCR := &countingReadSeeker{rs: bytes.NewReader(data)}
	full, err := Read(context.Background(), fullCR, "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(meta.Tracks) != 2 || meta.Cues != nil {
		t.Fatalf("meta tracks=%d cues=%d, want 2 tracks and no cues", len(meta.Tracks), len(meta.Cues))
	}
	if len(full.Cues) == 0 {
		t.Fatal("full Read should have parsed the Cues (control)")
	}
	if metaCR.read > 4*metaBufSize {
		t.Errorf("ReadMeta read %d bytes, want <= %d (must not reach Cues)", metaCR.read, 4*metaBufSize)
	}
	if metaCR.read*4 > fullCR.read {
		t.Errorf("ReadMeta read %d bytes vs full %d - expected meta to read far less by skipping Cues", metaCR.read, fullCR.read)
	}
}

// TestReadMetaBoundsOversizedSeekID feeds a crafted SeekHead whose SeekID element
// declares 64 KiB (a real element ID is <= 4 bytes). ReadMeta must SKIP it (not
// pull it off the untrusted file) and still parse the inline Info+Tracks.
func TestReadMetaBoundsOversizedSeekID(t *testing.T) {
	const idSize = 64 << 10
	var seekid bytes.Buffer
	ebml.WriteElementHeader(&seekid, mkv.IDSeekID, idSize)
	seekid.Write(make([]byte, idSize))
	var seekpos bytes.Buffer
	ebml.WriteElementHeader(&seekpos, mkv.IDSeekPosition, 8)
	ebml.WriteUint(&seekpos, 0, 8)
	badSeek := masterElem(mkv.IDSeek, seekid.Bytes(), seekpos.Bytes())

	data := segmentMKV(seekHeadElem(badSeek), infoElem(), tracksElem(), clusterElem())

	cr := &countingReadSeeker{rs: bytes.NewReader(data)}
	c, err := ReadMeta(context.Background(), cr, "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (inline Info+Tracks past the bad SeekHead)", len(c.Tracks))
	}
	if cr.read > 8<<10 {
		t.Errorf("ReadMeta read %d B - the 64 KiB SeekID must be seeked over, not read", cr.read)
	}
}

// TestReadMetaPartialParity checks ReadMeta matches Read when only Info, only
// Tracks, or neither is present before the clusters.
func TestReadMetaPartialParity(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"info only", segmentMKV(infoElem(), clusterElem())},
		{"tracks only", segmentMKV(tracksElem(), clusterElem())},
		{"neither (clusters only)", segmentMKV(clusterElem())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, ferr := Read(context.Background(), bytes.NewReader(tc.data), "x.mkv")
			meta, merr := ReadMeta(context.Background(), bytes.NewReader(tc.data), "x.mkv")
			if (ferr == nil) != (merr == nil) {
				t.Fatalf("error mismatch: full=%v meta=%v", ferr, merr)
			}
			if ferr != nil {
				return
			}
			if !reflect.DeepEqual(full.Tracks, meta.Tracks) {
				t.Errorf("Tracks differ:\n full=%+v\n meta=%+v", full.Tracks, meta.Tracks)
			}
			if !reflect.DeepEqual(full.Info, meta.Info) || full.DurationMs != meta.DurationMs {
				t.Errorf("Info/Duration differ:\n full=%+v (%d)\n meta=%+v (%d)", full.Info, full.DurationMs, meta.Info, meta.DurationMs)
			}
		})
	}
}

// TestReadMetaRealFileParity proves byte-identical Tracks+Info+DurationMs against
// a full Read on the committed real ffmpeg-muxed fixture.
func TestReadMetaRealFileParity(t *testing.T) {
	const fixture = "testdata/probe/hdr_multi.mkv"
	f1, err := os.Open(fixture)
	if err != nil {
		t.Skipf("fixture: %v", err)
	}
	defer f1.Close()
	full, err := Read(context.Background(), f1, fixture)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	f2, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	meta, err := ReadMeta(context.Background(), f2, fixture)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if !reflect.DeepEqual(full.Tracks, meta.Tracks) {
		t.Errorf("Tracks differ on real file")
	}
	if !reflect.DeepEqual(full.Info, meta.Info) || full.DurationMs != meta.DurationMs {
		t.Errorf("Info/Duration differ on real file")
	}
}

// TestReadMetaConcurrent runs ReadMeta from many goroutines (each on its own
// reader over the shared, read-only bytes) - the exact shape of a library
// indexer. Run with -race; ReadMeta holds no shared mutable state, so this must
// be race-free and every call must return the same 2 tracks.
func TestReadMetaConcurrent(t *testing.T) {
	data := buildSeekHeadMKV(t, true) // exercises the SeekHead jump path too
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
			if err != nil {
				t.Errorf("ReadMeta: %v", err)
				return
			}
			if len(c.Tracks) != 2 {
				t.Errorf("tracks = %d, want 2", len(c.Tracks))
			}
		}()
	}
	wg.Wait()
}
