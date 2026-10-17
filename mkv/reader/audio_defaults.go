package reader

import "github.com/gravity-zero/mkvgo/mkv"

// maxSampleRate bounds a SamplingFrequency a file can mean; 1 Hz is the floor.
const maxSampleRate = 1 << 24

// validSampleRate reports whether v is a sample rate and not a corrupt float (NaN fails both tests).
func validSampleRate(v float64) bool { return v >= 1 && v <= maxSampleRate }

// audioDefaults applies the spec defaults an audio TrackEntry leaves implicit - one channel, 8000 Hz (48000 for Opus,
// whose output rate the codec fixes) - after the codec configuration had its say; a value rejected as absurd stays unknown.
func audioDefaults(t *mkv.Track, rateRejected, channelsRejected bool) {
	if t.Type != mkv.AudioTrack {
		return
	}
	if t.Channels == nil && !channelsRejected {
		one := uint8(1)
		t.Channels = &one
	}
	if t.SampleRate == nil && !rateRejected {
		rate := 8000.0
		if t.Codec == "opus" {
			rate = 48000
		}
		t.SampleRate = &rate
	}
}

// encryptionName names a ContentEncryption from its ContentEncAlgo and, for AES, its AESSettingsCipherMode.
func encryptionName(algo, mode uint64) string {
	switch algo {
	case 1:
		return "DES"
	case 2:
		return "3DES"
	case 3:
		return "Twofish"
	case 4:
		return "Blowfish"
	case 5:
		switch mode {
		case 1:
			return "AES-CTR"
		case 2:
			return "AES-CBC"
		}
		return "AES"
	}
	return "unspecified"
}
