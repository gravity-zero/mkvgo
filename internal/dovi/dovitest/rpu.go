package dovitest

// bits appends values most significant bit first.
type bits struct {
	b    []byte
	free int
}

func (w *bits) u(v uint64, n int) {
	for n--; n >= 0; n-- {
		if w.free == 0 {
			w.b, w.free = append(w.b, 0), 8
		}
		w.free--
		w.b[len(w.b)-1] |= byte(v>>n&1) << w.free
	}
}

func (w *bits) ue(v uint64) {
	n := 0
	for (v+1)>>n > 1 {
		n++
	}
	w.u(0, n)
	w.u(v+1, n+1)
}

// coef writes a coefficient: its integer part, Exp-Golomb coded, and 23 fractional bits.
func (w *bits) coef(integer, fraction uint64) {
	w.ue(integer)
	w.u(fraction, 23)
}

// RPU builds a profile 7 RPU, its enhancement layer a full one or a minimal one: a two-piece polynomial for luma, a
// regression and a polynomial for chroma, display management data with one extension block.
func RPU(full bool) []byte { return build(full, false) }

// RPU81 builds the profile 8.1 RPU that RPU(full) converts to.
func RPU81(full bool) []byte { return build(full, true) }

func build(full, converted bool) []byte {
	w := &bits{}
	w.u(0x19, 8)
	w.u(2, 6)   // rpu_type
	w.u(18, 11) // rpu_format
	w.u(1, 4)   // vdr_rpu_profile
	w.u(0, 4)   // vdr_rpu_level
	w.u(1, 1)   // vdr_seq_info_present_flag
	w.u(0, 1)   // chroma_resampling_explicit_filter_flag
	w.u(0, 2)   // coefficient_data_type
	w.ue(23)    // coefficient_log2_denom
	w.u(1, 2)   // vdr_rpu_normalized_idc
	w.u(0, 1)   // bl_video_full_range_flag
	w.ue(2)     // bl_bit_depth_minus8
	w.ue(2)     // el_bit_depth_minus8
	w.ue(4)     // vdr_bit_depth_minus8
	w.u(0, 1)   // spatial_resampling_filter_flag
	w.u(0, 3)   // reserved
	if converted {
		w.u(0b01, 2) // no enhancement layer filter, residual off
	} else {
		w.u(0b10, 2)
	}
	w.u(1, 1) // vdr_dm_metadata_present_flag
	w.u(0, 1) // use_prev_vdr_rpu_flag

	identity := converted && full
	w.ue(0) // vdr_rpu_id
	w.ue(0) // mapping_color_space
	w.ue(0) // mapping_chroma_format_idc
	for c := range 3 {
		if c == 0 && !identity {
			w.ue(1)
			w.u(0, 10)
			w.u(512, 10)
			w.u(1023, 10)
			continue
		}
		w.ue(0)
		w.u(0, 10)
		w.u(1023, 10)
	}
	if !converted {
		w.u(0, 3) // nlq_method_idc
		w.u(0, 10)
		w.u(1023, 10)
		w.ue(1) // num_x_partitions_minus1
		w.ue(2) // num_y_partitions_minus1
	} else {
		w.ue(0)
		w.ue(0)
	}
	if identity {
		for range 3 {
			w.ue(0)   // polynomial
			w.ue(0)   // first order
			w.u(0, 1) // not interpolated
			w.coef(0, 0)
			w.coef(1, 0) // the signed code of +1
		}
	} else {
		for piece := range uint64(2) { // luma: two second-order pieces
			w.ue(0)
			w.ue(1)
			w.coef(piece, 0x1234)
			w.coef(2, 0x7FFFFF)
			w.coef(3, 0x000100)
		}
		w.ue(1)   // chroma: a first-order regression
		w.u(0, 2) // mmr_order_minus1
		for k := range uint64(8) {
			w.coef(k, 0x400000+k)
		}
		w.ue(0) // chroma: a first-order polynomial
		w.ue(0)
		w.u(0, 1)
		w.coef(0, 0x000300)
		w.coef(1, 0x000003)
	}
	if !converted {
		for range 3 {
			if full {
				w.u(512, 10)
				w.coef(0, 0x100000)
				w.coef(0, 0x200000)
				w.coef(0, 0x000400)
				continue
			}
			w.u(0, 10)
			w.coef(1, 0)
			w.coef(0, 0)
			w.coef(0, 0)
		}
	}

	w.ue(0) // affected_dm_metadata_id
	w.ue(0) // current_dm_metadata_id
	w.ue(1) // scene_refresh_flag
	if converted {
		for _, c := range []int16{9574, 0, 13802, 9574, -1540, -5348, 9574, 17610, 0} {
			w.u(uint64(uint16(c)), 16)
		}
		for _, o := range []uint64{16777216, 134217728, 134217728} {
			w.u(o, 32)
		}
		for _, c := range []int16{7222, 8771, 390, 2654, 12430, 1300, 0, 422, 15962} {
			w.u(uint64(uint16(c)), 16)
		}
	} else {
		for i := range uint64(9) {
			w.u(8192+i, 16)
		}
		for i := range uint64(3) {
			w.u(1000+i, 32)
		}
		for i := range uint64(9) {
			w.u(4096+i, 16)
		}
	}
	w.u(0xFFFF, 16) // signal_eotf
	w.u(0, 16)
	w.u(0, 16)
	w.u(0, 32)
	w.u(12, 5) // signal_bit_depth
	if converted {
		w.u(0, 2) // signal_color_space
	} else {
		w.u(1, 2)
	}
	w.u(0, 2)     // signal_chroma_format
	w.u(1, 2)     // signal_full_range_flag
	w.u(7, 12)    // source_min_pq
	w.u(3079, 12) // source_max_pq
	w.u(42, 10)   // source_diagonal
	w.ue(1)       // one extension block
	w.free = 0
	w.ue(5)
	w.u(1, 8) // level 1
	for i := range uint64(5) {
		w.u(0xA0+i, 8)
	}
	w.free = 0

	crc := uint32(0xFFFFFFFF)
	for _, c := range w.b[1:] {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return append(w.b, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc), 0x80)
}
