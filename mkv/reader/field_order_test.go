package reader

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestFieldOrderElementBothParsers reads the Matroska FieldOrder element (0x9D)
// through the seekable and the streaming parser, with the element before and
// after FlagInterlaced (the two are resolved together, whichever comes first).
func TestFieldOrderElementBothParsers(t *testing.T) {
	cases := []struct {
		name       string
		video      []byte
		scan, want string
	}{
		{"flag then order tt", masterElem(mkv.IDVideo,
			uintElem(mkv.IDPixelWidth, 720, 2), uintElem(mkv.IDPixelHeight, 576, 2),
			uintElem(mkv.IDFlagInterlaced, 1, 1), uintElem(mkv.IDFieldOrder, 1, 1),
		), "interlaced", "tt"},
		{"order bb then flag", masterElem(mkv.IDVideo,
			uintElem(mkv.IDFieldOrder, 6, 1), uintElem(mkv.IDFlagInterlaced, 1, 1),
			uintElem(mkv.IDPixelWidth, 720, 2), uintElem(mkv.IDPixelHeight, 576, 2),
		), "interlaced", "bb"},
		{"order without an interlaced flag is ignored", masterElem(mkv.IDVideo,
			uintElem(mkv.IDPixelWidth, 720, 2), uintElem(mkv.IDPixelHeight, 576, 2),
			uintElem(mkv.IDFieldOrder, 1, 1),
		), "", ""},
		{"progressive flag, stale order ignored", masterElem(mkv.IDVideo,
			uintElem(mkv.IDFlagInterlaced, 2, 1), uintElem(mkv.IDFieldOrder, 14, 1),
		), "progressive", "progressive"},
	}
	for _, c := range cases {
		te := trackEntry(
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
			strElem(mkv.IDCodecID, "V_MPEG2"),
			c.video,
		)
		seek := readFirstTrack(t, buildMKV(te))
		if seek.ScanType != c.scan || seek.FieldOrder != c.want {
			t.Errorf("%s (seekable): ScanType/FieldOrder = %q/%q, want %q/%q", c.name, seek.ScanType, seek.FieldOrder, c.scan, c.want)
		}

		var seg bytes.Buffer
		seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
		seg.Write(masterElem(mkv.IDTracks, te))
		cont, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
		if err != nil {
			t.Fatalf("%s: ReadStream: %v", c.name, err)
		}
		if len(cont.Tracks) == 0 {
			t.Fatalf("%s: no tracks", c.name)
		}
		str := cont.Tracks[0]
		if str.ScanType != c.scan || str.FieldOrder != c.want {
			t.Errorf("%s (stream): ScanType/FieldOrder = %q/%q, want %q/%q", c.name, str.ScanType, str.FieldOrder, c.scan, c.want)
		}
	}
}

// TestContainerScanWinsOverSPS: a container FlagInterlaced beats the SPS, and an
// interlaced container flag over a progressive SPS still yields no field order.
func TestContainerScanWinsOverSPS(t *testing.T) {
	avcc := buildHighSPSAvcC(1, 1, 1) // frame_mbs_only_flag = 1
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEG4/ISO/AVC"),
		bytesElem(mkv.IDCodecPrivate, avcc),
		masterElem(mkv.IDVideo, uintElem(mkv.IDFlagInterlaced, 1, 1)),
	)
	tr := readFirstTrack(t, buildMKV(te))
	if tr.ScanType != "interlaced" || tr.FieldOrder != "" {
		t.Errorf("ScanType/FieldOrder = %q/%q, want interlaced/\"\"", tr.ScanType, tr.FieldOrder)
	}
}

// TestAVCScanNeedsProof: an H.264 track is interlaced only when the container
// says so; frame_mbs_only_flag=0 alone leaves the scan type unknown.
func TestAVCScanNeedsProof(t *testing.T) {
	cases := []struct {
		name         string
		frameMbsOnly bool
		video        []byte
		scan, order  string
	}{
		{"field coding allowed, no flag", false, nil, "", ""},
		{"field coding allowed, FlagInterlaced 1", false, masterElem(mkv.IDVideo, uintElem(mkv.IDFlagInterlaced, 1, 1)), "interlaced", ""},
		{"field coding allowed, FieldOrder tt", false, masterElem(mkv.IDVideo, uintElem(mkv.IDFlagInterlaced, 1, 1), uintElem(mkv.IDFieldOrder, 1, 1)), "interlaced", "tt"},
		{"field coding allowed, FlagInterlaced 2", false, masterElem(mkv.IDVideo, uintElem(mkv.IDFlagInterlaced, 2, 1)), "progressive", "progressive"},
		{"frame_mbs_only", true, nil, "progressive", "progressive"},
	}
	for _, c := range cases {
		elems := [][]byte{
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
			strElem(mkv.IDCodecID, "V_MPEG4/ISO/AVC"),
			bytesElem(mkv.IDCodecPrivate, buildHighSPSAvcCScan(1, 1, 1, c.frameMbsOnly)),
		}
		if c.video != nil {
			elems = append(elems, c.video)
		}
		tr := readFirstTrack(t, buildMKV(trackEntry(elems...)))
		if tr.ScanType != c.scan || tr.FieldOrder != c.order {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want %q/%q", c.name, tr.ScanType, tr.FieldOrder, c.scan, c.order)
		}
	}
}

// TestFieldOrderSwappedValuesReadAsTheProber: files ffmpeg wrote from tff and bff
// interlaced encodes, FieldOrder 9 and 14 (the "swapped" values), read with the
// names the reference prober gives them - the first letter is the field coded
// first: 9 "tb", 14 "bt".
func TestFieldOrderSwappedValuesReadAsTheProber(t *testing.T) {
	for file, want := range map[string]string{"h264_fieldorder_9.mkv": "tb", "h264_fieldorder_14.mkv": "bt"} {
		data, err := os.ReadFile("../../internal/testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		tr := readFirstTrack(t, data)
		if tr.ScanType != "interlaced" || tr.FieldOrder != want {
			t.Errorf("%s: ScanType/FieldOrder = %q/%q, want interlaced/%q", file, tr.ScanType, tr.FieldOrder, want)
		}
	}
}
