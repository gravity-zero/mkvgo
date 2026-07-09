package ops

// targets.go - the built-in Target capability table. It is intentionally a
// plain, static data table (no logic) so it stays trivially reviewable and
// updatable: every field is a fact about what the named player accepts,
// stated as of the baseline below. A caller who knows better (a newer OS, a
// device with hardware HEVC decoding, etc.) constructs its own Target instead
// of using TargetByName.
//
// Baseline: mid-2020s desktop/mobile browser and streaming-device media
// pipelines. Kept deliberately conservative where real-world support is
// hardware/OS-dependent (HEVC on non-Apple/non-Edge Chromium, VP9/AV1 on
// Safari) rather than guessing a capability mkvgo cannot verify head-only.

// TargetByName returns one of the built-in capability profiles, or
// (Target{}, false) for an unknown name.
//
// Recognised names: "safari", "chrome", "firefox", "chromecast-gen3",
// "mse-generic", "chromium-generic", "brave", "opera", "vivaldi",
// "samsung-internet", "edge".
func TargetByName(name string) (Target, bool) {
	switch name {
	case "safari":
		return safariTarget(), true
	case "chrome", "chromium-generic", "brave", "opera", "vivaldi", "samsung-internet":
		return chromiumBaseline(name), true
	case "edge":
		return edgeTarget(), true
	case "firefox":
		return firefoxTarget(), true
	case "chromecast-gen3":
		return chromecastGen3Target(), true
	case "mse-generic":
		return mseGenericTarget(), true
	}
	return Target{}, false
}

// safariTarget: Safari (macOS/iOS) plays HEVC (Main and Main10) with HDR10
// and Dolby Vision, natively via HLS or progressive MP4. Its VP9/AV1 support
// is limited/version-dependent, so both are left out of VideoCodecs
// (conservative: a caller on a confirmed-capable Safari version overrides).
func safariTarget() Target {
	return Target{
		Name:        "safari",
		Container:   []string{"mp4", "hls"},
		VideoCodecs: []string{"h264", "hevc"},
		AudioCodecs: []string{"aac", "ac3", "eac3", "mp3", "flac"},
		HDR:         true,
		DolbyVision: true,
		HEVCMain10:  true,
		VP9Profile2: false,
	}
}

// chromiumBaseline is the shared, conservative Chromium-family media
// pipeline: H.264/VP8/VP9/AV1 video, Opus/AAC/MP3/FLAC/Vorbis audio, no HEVC
// (support is hardware/OS-dependent across the family and not guaranteed, so
// it is left unsupported by default) and no confirmed HDR pass-through or
// Dolby Vision. Chrome, Brave, Opera, Vivaldi and Samsung Internet all use
// Chromium's media pipeline closely enough to share this table unchanged -
// name is the only thing that differs between them.
func chromiumBaseline(name string) Target {
	return Target{
		Name:        name,
		Container:   []string{"mp4", "webm"},
		VideoCodecs: []string{"h264", "vp8", "vp9", "av1"},
		AudioCodecs: []string{"aac", "opus", "mp3", "flac", "vorbis"},
		HDR:         false,
		DolbyVision: false,
		HEVCMain10:  false,
		VP9Profile2: true,
	}
}

// edgeTarget is the one Chromium browser that differs from the shared
// baseline: Edge plays HEVC (Main and Main10) on Windows via the OS-provided
// HEVC Video Extension, so it starts from chromiumBaseline and turns HEVC on.
// HDR is left at the baseline's false - the OS decoder path does not
// guarantee an HDR10-aware render pipeline, so this stays conservative until
// a caller confirms otherwise for its deployment.
func edgeTarget() Target {
	t := chromiumBaseline("edge")
	t.VideoCodecs = append(t.VideoCodecs, "hevc")
	t.HEVCMain10 = true
	return t
}

// firefoxTarget: same conservative baseline as Chromium (H.264/VP8/VP9/AV1,
// no HEVC by default - support depends on OS codecs Firefox does not bundle).
func firefoxTarget() Target {
	return Target{
		Name:        "firefox",
		Container:   []string{"mp4", "webm"},
		VideoCodecs: []string{"h264", "vp8", "vp9", "av1"},
		AudioCodecs: []string{"aac", "opus", "mp3", "flac", "vorbis"},
		HDR:         false,
		DolbyVision: false,
		HEVCMain10:  false,
		VP9Profile2: true,
	}
}

// chromecastGen3Target: the (non-Ultra) Chromecast 3rd generation caps at
// 1080p, H.264 High up to Level 4.2 plus VP8/VP9 (profile 0, no 10-bit), no
// HEVC and no HDR/Dolby Vision (that is the Ultra model).
func chromecastGen3Target() Target {
	return Target{
		Name:         "chromecast-gen3",
		Container:    []string{"mp4", "webm"},
		VideoCodecs:  []string{"h264", "vp8", "vp9"},
		AudioCodecs:  []string{"aac", "mp3", "vorbis", "opus"},
		MaxWidth:     1920,
		MaxHeight:    1080,
		MaxLevelH264: 42,
		HDR:          false,
		DolbyVision:  false,
		HEVCMain10:   false,
		VP9Profile2:  false,
	}
}

// mseGenericTarget is the generic MediaSource Extensions baseline: plain
// H.264 (capped at High@4.1, the safe universal bar across MSE
// implementations) plus AAC, packaged in fragmented MP4. Anything above that
// - HEVC/VP9/AV1, higher levels, 10-bit, HDR, Dolby Vision - is out of scope
// for this baseline and flags a transcode; a caller targeting a specific,
// more capable MSE player should build its own Target instead.
func mseGenericTarget() Target {
	return Target{
		Name:         "mse-generic",
		Container:    []string{"mp4"},
		VideoCodecs:  []string{"h264"},
		AudioCodecs:  []string{"aac"},
		MaxLevelH264: 41,
		HDR:          false,
		DolbyVision:  false,
		HEVCMain10:   false,
		VP9Profile2:  false,
	}
}
