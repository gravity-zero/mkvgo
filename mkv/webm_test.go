package mkv

import (
	"strings"
	"testing"
)

// minimal valid-enough codec setup data for the validator's prefix/shape checks.
var (
	opusHead   = append([]byte("OpusHead"), 0x01, 0x02, 0x38, 0x01, 0x80, 0xbb, 0x00, 0x00, 0x00, 0x00, 0x00)
	vorbisInit = []byte{0x02, 0x00, 0x00, 0x01} // 3 packed headers marker
	av1Config  = []byte{0x81, 0x00, 0x00, 0x00} // av1C marker byte
)

func TestIsWebMCodec(t *testing.T) {
	tests := []struct {
		codec string
		want  bool
	}{
		{"V_VP8", true},
		{"V_VP9", true},
		{"V_AV1", true},
		{"A_VORBIS", true},
		{"A_OPUS", true},
		// short names, as the reader actually stores them in Track.Codec:
		{"vp8", true},
		{"vp9", true},
		{"av1", true},
		{"vorbis", true},
		{"opus", true},
		{"h264", false},
		{"aac", false},
		{"S_TEXT/WEBVTT", true},
		{"D_WEBVTT/SUBTITLES", true},
		{"D_WEBVTT/CAPTIONS", true},
		{"V_MPEG4/ISO/AVC", false},  // h264
		{"V_MPEGH/ISO/HEVC", false}, // hevc
		{"A_AC3", false},
		{"S_TEXT/ASS", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsWebMCodec(tt.codec); got != tt.want {
			t.Errorf("IsWebMCodec(%q) = %v, want %v", tt.codec, got, tt.want)
		}
	}
}

func TestValidateWebM(t *testing.T) {
	tests := []struct {
		name     string
		tracks   []Track
		wantErr  bool
		mentions string // substring expected in the error
	}{
		{
			name:    "vp9 + opus ok",
			tracks:  []Track{{ID: 1, Type: VideoTrack, Codec: "V_VP9"}, {ID: 2, Type: AudioTrack, Codec: "A_OPUS", CodecPrivate: opusHead}},
			wantErr: false,
		},
		{
			name:    "av1 with config ok",
			tracks:  []Track{{ID: 1, Type: VideoTrack, Codec: "V_AV1", CodecPrivate: av1Config}},
			wantErr: false,
		},
		{
			name:    "vorbis with setup ok",
			tracks:  []Track{{ID: 1, Type: AudioTrack, Codec: "A_VORBIS", CodecPrivate: vorbisInit}},
			wantErr: false,
		},
		{
			// short names, as a real parsed Container carries them.
			name:    "short-name vp9 + opus ok",
			tracks:  []Track{{ID: 1, Type: VideoTrack, Codec: "vp9"}, {ID: 2, Type: AudioTrack, Codec: "opus", CodecPrivate: opusHead}},
			wantErr: false,
		},
		{
			name:    "empty container ok",
			tracks:  nil,
			wantErr: false,
		},
		{
			name:     "h264 rejected",
			tracks:   []Track{{ID: 1, Type: VideoTrack, Codec: "V_MPEG4/ISO/AVC"}},
			wantErr:  true,
			mentions: "V_MPEG4/ISO/AVC",
		},
		{
			name:     "ac3 audio rejected",
			tracks:   []Track{{ID: 1, Type: VideoTrack, Codec: "V_VP9"}, {ID: 2, Type: AudioTrack, Codec: "A_AC3"}},
			wantErr:  true,
			mentions: "A_AC3",
		},
		{
			name:     "opus without OpusHead rejected",
			tracks:   []Track{{ID: 1, Type: AudioTrack, Codec: "A_OPUS"}},
			wantErr:  true,
			mentions: "OpusHead",
		},
		{
			name:     "vorbis without setup rejected",
			tracks:   []Track{{ID: 1, Type: AudioTrack, Codec: "A_VORBIS"}},
			wantErr:  true,
			mentions: "setup-header",
		},
		{
			name:     "av1 without config rejected",
			tracks:   []Track{{ID: 1, Type: VideoTrack, Codec: "V_AV1"}},
			wantErr:  true,
			mentions: "av1C",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWebM(&Container{Tracks: tt.tracks})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateWebM err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.mentions != "" && !strings.Contains(err.Error(), tt.mentions) {
				t.Errorf("error %q does not mention %q", err, tt.mentions)
			}
		})
	}
}

func TestWebMNonSubsetElements(t *testing.T) {
	clean := &Container{Tracks: []Track{{Codec: "vp9"}}}
	if got := WebMNonSubsetElements(clean); len(got) != 0 {
		t.Errorf("clean container: got %v, want none", got)
	}
	dirty := &Container{
		Chapters:    []Chapter{{Title: "ch"}},
		Attachments: []Attachment{{}},
		Tags:        []Tag{{}},
	}
	if got := WebMNonSubsetElements(dirty); len(got) != 3 {
		t.Errorf("dirty container: got %v, want 3 (Chapters, Attachments, Tags)", got)
	}
}

func TestWebMDocTypeVersion(t *testing.T) {
	vp9opus := &Container{Tracks: []Track{{Codec: "V_VP9"}, {Codec: "A_OPUS"}}}
	if got := WebMDocTypeVersion(vp9opus); got != 2 {
		t.Errorf("WebMDocTypeVersion(VP9/Opus) = %d, want 2", got)
	}
	withAV1 := &Container{Tracks: []Track{{Codec: "V_AV1"}, {Codec: "A_OPUS"}}}
	if got := WebMDocTypeVersion(withAV1); got != 4 {
		t.Errorf("WebMDocTypeVersion(AV1) = %d, want 4", got)
	}
}
