package matroska

import (
	"context"
	"testing"
)

// TestFacadeReadOptionsOpenMeta proves each ReadOption constructor wires
// through OpenMeta without error - at minimum the option-passing path runs.
func TestFacadeReadOptionsOpenMeta(t *testing.T) {
	requireFixture(t)
	ctx := context.Background()

	tests := []struct {
		name string
		opt  ReadOption
	}{
		{"InBandColourFallback", WithInBandColourFallback()},
		{"SampledKeyframes", WithSampledKeyframes(16)},
		{"KeyframeIndex", WithKeyframeIndex()},
		{"Bitrate", WithBitrate()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := OpenMeta(ctx, fixturePath, tt.opt)
			assertNoErr(t, err)
			if len(c.Tracks) == 0 {
				t.Error("Tracks is empty")
			}
		})
	}
}

// TestFacadeReadOptionsOpenMetaWithFS proves the same options also run
// through OpenMetaWithFS (nil FS falls back to the OS filesystem).
func TestFacadeReadOptionsOpenMetaWithFS(t *testing.T) {
	requireFixture(t)
	c, err := OpenMetaWithFS(context.Background(), fixturePath, nil, WithBitrate(), WithSampledKeyframes(0))
	assertNoErr(t, err)
	if len(c.Tracks) == 0 {
		t.Error("Tracks is empty")
	}
}
