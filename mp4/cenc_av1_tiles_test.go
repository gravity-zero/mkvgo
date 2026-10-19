package mp4

import (
	"strings"
	"testing"
)

// TestAV1CENCRefusesAFrameOfSeveralTiles: one subsample over several tiles would encrypt their size fields too, which no
// decoder can read back; the split refuses it instead.
func TestAV1CENCRefusesAFrameOfSeveralTiles(t *testing.T) {
	var seq bitWriter
	seq.write(0, 3)   // seq_profile
	seq.write(1, 1)   // still_picture
	seq.write(1, 1)   // reduced_still_picture_header
	seq.write(0, 5)   // seq_level_idx[0]
	seq.write(6, 4)   // frame_width_bits_minus_1 (n=7)
	seq.write(5, 4)   // frame_height_bits_minus_1 (n=6)
	seq.write(127, 7) // max_frame_width_minus_1 (width 128: two 64x64 superblocks across)
	seq.write(63, 6)  // max_frame_height_minus_1 (height 64)
	seq.write(0, 10)  // 128x128 superblock, filter intra, intra edge, superres, cdef, restoration, high bitdepth, mono, colour description, range
	seq.write(0, 2)   // chroma_sample_position
	seq.write(0, 1)   // separate_uv_delta_q
	seq.write(0, 1)   // film_grain_params_present
	var fh bitWriter
	fh.write(0, 3) // disable_cdf_update, allow_screen_content_tools, render_and_frame_size_different
	fh.write(1, 1) // uniform_tile_spacing_flag
	fh.write(1, 1) // increment_tile_cols_log2: two tile columns
	fh.write(0, 1) // context_update_tile_id
	fh.write(0, 2) // tile_size_bytes_minus_1
	fh.write(0, 8) // base_q_idx
	fh.write(0, 6) // delta_q_y_dc, delta_q_u_dc, delta_q_u_ac, using_qmatrix, segmentation_enabled, reduced_tx_set
	sample := append(av1TemporalDelimiter(), obuLenPrefixed(obuSeqHeader, seq.bytes())...)
	sample = append(sample, obuLenPrefixed(obuFrame, append(fh.bytes(), make([]byte, 64)...))...)
	_, err := (&av1Splitter{}).split(sample)
	if err == nil || !strings.Contains(err.Error(), "tiles") {
		t.Errorf("a two-tile frame: err = %v, want a refusal naming its tiles", err)
	}
}
