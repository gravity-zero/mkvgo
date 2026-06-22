package reader

import "testing"

import "github.com/gravity-zero/mkvgo/mkv"

// TestReadDolbyVisionMKV checks the reader decodes a dvcC BlockAdditionMapping
// into Track.DolbyVision.
func TestReadDolbyVisionMKV(t *testing.T) {
	rec := []byte{1, 0, 16, 0x35, 0x10, 0, 0, 0} // profile 8, level 6, compat 1
	bam := masterElem(mkv.IDBlockAdditionMapping,
		uintElem(mkv.IDBlockAddIDType, mkv.BlockAddIDTypeDVCC, 4),
		bytesElem(mkv.IDBlockAddIDExtraData, rec),
	)
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		bam,
	)))
	if tr.DolbyVision == nil {
		t.Fatal("DolbyVision is nil; want it decoded from the dvcC BlockAdditionMapping")
	}
	dv := tr.DolbyVision
	if dv.Profile != 8 || dv.BLSignalCompatID != 1 || dv.Level != 6 {
		t.Errorf("dv = %+v, want profile 8 / compat 1 / level 6", dv)
	}
}

// TestReadNoDolbyVisionMKV checks a plain HEVC track leaves DolbyVision nil.
func TestReadNoDolbyVisionMKV(t *testing.T) {
	tr := readFirstTrack(t, buildMKV(trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
	)))
	if tr.DolbyVision != nil {
		t.Errorf("DolbyVision = %+v, want nil for a non-DV track", tr.DolbyVision)
	}
}
