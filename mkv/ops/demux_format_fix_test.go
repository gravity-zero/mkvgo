package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func vorbisTrack(comment []byte) mkv.Track {
	ident := append([]byte("\x01vorbis\x00\x00\x00\x00\x02\x44\xac\x00\x00"), make([]byte, 14)...) // 2 ch, 44.1 kHz
	setup := []byte("\x05vorbis-setup-header")
	cp := []byte{2, byte(len(ident))}
	for n := len(comment); ; n -= 255 { // Xiph lacing of the comment's size
		if n < 255 {
			cp = append(cp, byte(n))
			break
		}
		cp = append(cp, 255)
	}
	cp = append(append(append(cp, ident...), comment...), setup...)
	return mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "vorbis", CodecPrivate: cp, SampleRate: f64(44100), Channels: u8(2)}
}

// TestDemuxOggVorbisGranuleEndsItsPacket: Vorbis rides in SimpleBlocks, which carry no duration: each page's granule
// was its packet's start, not its end, and the last page cut the last packet.
func TestDemuxOggVorbisGranuleEndsItsPacket(t *testing.T) {
	dir := t.TempDir()
	src := demuxFixture(t, dir, []mkv.Track{vorbisTrack([]byte("\x03vorbis-comment"))}, 5,
		func(_ uint64, i int) ([]byte, bool) { return []byte{0, byte(i)}, true })
	pages := oggPages(t, readAll(t, filepath.Join(demuxTo(t, src, false), "1.ogg")))
	for i, p := range pages[3:] {
		if want := int64(4410 * (i + 1)); p.granule != want { // 100 ms a packet at 44.1 kHz
			t.Errorf("packet %d: granule %d, want %d", i, p.granule, want)
		}
	}
}

// TestDemuxOggPacketLongerThanAPage: a Vorbis comment header past 65,025 bytes (cover art) did not fit one page and
// failed the demux; it spans pages, each one after the first marked as continuing it.
func TestDemuxOggPacketLongerThanAPage(t *testing.T) {
	dir := t.TempDir()
	comment := append([]byte("\x03vorbis"), bytes.Repeat([]byte{0xA5}, 140_000)...)
	src := demuxFixture(t, dir, []mkv.Track{vorbisTrack(comment)}, 3,
		func(_ uint64, i int) ([]byte, bool) { return []byte{0, byte(i)}, true })
	pages := oggPages(t, readAll(t, filepath.Join(demuxTo(t, src, false), "1.ogg")))
	var got []byte
	n := 0
	for _, p := range pages[1:] {
		if n > 0 && p.flags&1 == 0 {
			break
		}
		got = append(got, p.body...)
		n++
		if len(got) >= len(comment) {
			break
		}
	}
	if !bytes.Equal(got, comment) || n < 3 {
		t.Errorf("comment header over %d pages, %d bytes, want %d over 3 or more", n, len(got), len(comment))
	}
}

// TestDemuxOpusEndTrim: the last block's DiscardPadding trims the stream's end; the last page's granule says so
// (RFC 7845 4.4), it counted the trimmed samples.
func TestDemuxOpusEndTrim(t *testing.T) {
	dir := t.TempDir()
	head := []byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00")
	opus := mkv.Track{ID: 1, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: head, SampleRate: f64(48000), Channels: u8(2)}
	var blocks []mkv.Block
	for i := range 4 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 20, Keyframe: true, Data: []byte{0xFC, byte(i), 0, 0}})
	}
	blocks[3].DiscardPaddingNs = 10_000_000 // 10 ms: 480 samples
	src := buildMultiClusterMKV(t, dir, "src.mkv", []mkv.Track{opus}, [][]mkv.Block{blocks}, 80)
	pages := oggPages(t, readAll(t, filepath.Join(demuxTo(t, src, false), "1.opus")))
	if last := pages[len(pages)-1]; last.granule != 4*960-480 {
		t.Errorf("last granule %d, want %d", last.granule, 4*960-480)
	}
}

// TestDemuxRefusalSaysWhatToDo: a frame the codec's file cannot hold stopped the demux with "NAL size 0 out of
// bounds at 4"; the refusal names the track, the block's time and the way out.
func TestDemuxRefusalSaysWhatToDo(t *testing.T) {
	dir := t.TempDir()
	sps, pps := []byte{0x67, 0x64, 0x00, 0x1F}, []byte{0x68, 0xEE}
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcRecord(sps, pps), Width: u32(64), Height: u32(64)}
	src := demuxFixture(t, dir, []mkv.Track{video}, 3, func(_ uint64, i int) ([]byte, bool) {
		if i == 2 {
			return []byte{0, 0, 0, 0, 0x41}, false // a NAL length of 0
		}
		return []byte{0, 0, 0, 2, 0x65, byte(i)}, i == 0
	})
	err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("a frame Annex-B cannot hold was accepted")
	}
	for _, want := range []string{"track 1", "0.200", "--raw"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
}
