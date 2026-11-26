package dovitest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// CheckSample walks a length-prefixed HEVC sample with no help from the converter: no NAL unit may hold a start code
// or an escape out of place, and each RPU, unescaped here, must open on 0x19, close on 0x80 and carry the CRC of its
// own bytes. It returns how many RPUs the sample holds and how many escape bytes they needed.
func CheckSample(tb testing.TB, sample []byte, lenSize int) (rpus, escapes int) {
	tb.Helper()
	for len(sample) > 0 {
		if len(sample) < lenSize {
			tb.Fatalf("%d bytes left where a NAL unit length of %d is due", len(sample), lenSize)
		}
		n := 0
		for _, b := range sample[:lenSize] {
			n = n<<8 | int(b)
		}
		if sample = sample[lenSize:]; n < 2 || n > len(sample) {
			tb.Fatalf("NAL unit of %d bytes in %d", n, len(sample))
		}
		nal := sample[:n]
		sample = sample[n:]
		var plain []byte
		used := 0
		for i := 0; i < len(nal); i++ {
			if i >= 2 && nal[i-1] == 0 && nal[i-2] == 0 && nal[i] <= 3 {
				if nal[i] != 3 || (i+1 < len(nal) && nal[i+1] > 3) {
					tb.Fatalf("NAL unit of type %d: bytes 00 00 %02x at %d, a start code or a stray escape", nal[0]>>1&0x3F, nal[i], i-2)
				}
				used++
				i++ // the escaped byte opens a new run
				if i == len(nal) {
					break
				}
			}
			plain = append(plain, nal[i])
		}
		if nal[0]>>1&0x3F != 62 {
			continue
		}
		rpus, escapes = rpus+1, escapes+used
		body := plain[2:]
		if len(body) < 7 || body[0] != 0x19 || body[len(body)-1] != 0x80 {
			tb.Fatalf("RPU of %d bytes does not sit between 0x19 and 0x80", len(body))
		}
		if got, want := binary.BigEndian.Uint32(body[len(body)-5:]), crcOf(body[1:len(body)-5]); got != want {
			tb.Fatalf("RPU carries CRC %08x, its bytes give %08x", got, want)
		}
	}
	return rpus, escapes
}

// Probe is what ffprobe says of a file's first video frame and of its video stream.
type Probe struct {
	Frames []struct {
		SideData []map[string]any `json:"side_data_list"`
	} `json:"frames"`
	Streams []struct {
		SideData []map[string]any `json:"side_data_list"`
	} `json:"streams"`
}

// FFprobe runs the ffprobe named by MKVGO_FFPROBE on path; the test is skipped when the variable is unset.
func FFprobe(tb testing.TB, path string) *Probe {
	tb.Helper()
	bin := os.Getenv("MKVGO_FFPROBE")
	if bin == "" {
		tb.Skip("set MKVGO_FFPROBE to an ffprobe binary")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-v", "error", "-of", "json", "-select_streams", "v:0", "-read_intervals", "%+#1", "-show_frames",
		"-show_entries", "frame=side_data_list:stream_side_data_list", path)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		tb.Fatalf("ffprobe %s: %v\n%s", path, err, stderr.Bytes())
	}
	p := &Probe{}
	if err := json.Unmarshal(out, p); err != nil || len(p.Frames) == 0 || len(p.Streams) == 0 {
		tb.Fatalf("ffprobe %s: %v, %d frames and %d streams in\n%s", path, err, len(p.Frames), len(p.Streams), out)
	}
	return p
}

func side(list []map[string]any, kind string) map[string]any {
	for _, sd := range list {
		if sd["side_data_type"] == kind {
			return sd
		}
	}
	return nil
}

// RPU is the first frame's parsed Dolby Vision metadata, nil when it has none.
func (p *Probe) RPU() map[string]any { return side(p.Frames[0].SideData, "Dolby Vision Metadata") }

// Config is the stream's Dolby Vision configuration record, nil when it has none.
func (p *Probe) Config() map[string]any {
	return side(p.Streams[0].SideData, "DOVI configuration record")
}

// hdr is the stream's static HDR metadata, each fraction as its value: containers store them over different bases.
func (p *Probe) hdr() map[string]float64 {
	out := map[string]float64{}
	for _, kind := range []string{"Mastering display metadata", "Content light level metadata"} {
		for k, v := range side(p.Streams[0].SideData, kind) {
			switch v := v.(type) {
			case float64:
				out[k] = v
			case string:
				if num, den, ok := strings.Cut(v, "/"); ok {
					n, _ := strconv.ParseFloat(num, 64)
					d, _ := strconv.ParseFloat(den, 64)
					out[k] = math.Round(n/d*1e5) / 1e5
				}
			}
		}
	}
	return out
}

func expect(tb testing.TB, what string, got map[string]any, want map[string]any) {
	tb.Helper()
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			tb.Errorf("%s: %s is %v, want %v", what, k, got[k], v)
		}
	}
}

// curves returns the reshaping of each component, without the quantizer of the residual.
func curves(rpu map[string]any) []any {
	comps, _ := rpu["components"].([]any)
	out := make([]any, 0, len(comps))
	for _, c := range comps {
		m, _ := c.(map[string]any)
		out = append(out, map[string]any{"pivots": m["pivots"], "pieces": m["pieces"]})
	}
	return out
}

var identity = map[string]any{"pivots": "0 1023", "pieces": []any{map[string]any{
	"mapping_idc": 0.0, "mapping_idc_name": "polynomial", "poly_order": 1.0, "poly_coef": "0 8388608"}}}

// CheckProfile81 holds ffprobe's reading of out, the profile 8.1 conversion of the fixture src, against the values the
// conversion owes: the configuration, the residual off, the profile 8.1 matrices, and the curves - the identity when
// the enhancement layer was a full one and the source's were not, the source's own otherwise.
func CheckProfile81(tb testing.TB, src, out string, full bool) {
	tb.Helper()
	in, got := FFprobe(tb, src), FFprobe(tb, out)
	before, after := in.RPU(), got.RPU()
	if before == nil || after == nil || got.Config() == nil {
		tb.Fatalf("ffprobe parsed no Dolby Vision metadata: source %v, output %v, configuration %v", before != nil, after != nil, got.Config())
	}
	expect(tb, "source RPU", before, map[string]any{"disable_residual_flag": 0.0, "nlq_method_idc_name": "linear_dz"})
	was := curves(before)
	if len(was) != 3 || reflect.DeepEqual(was, []any{identity, identity, identity}) {
		tb.Fatalf("the source's curves are not a reshaping to convert from: %v", was)
	}
	expect(tb, "configuration", got.Config(), map[string]any{"dv_profile": 8.0, "dv_level": 6.0, "rpu_present_flag": 1.0,
		"el_present_flag": 0.0, "bl_present_flag": 1.0, "dv_bl_signal_compatibility_id": 1.0})
	expect(tb, "RPU", after, map[string]any{
		"vdr_rpu_profile": 1.0, "disable_residual_flag": 1.0, "el_spatial_resampling_filter_flag": 0.0,
		"nlq_method_idc_name": "none", "num_x_partitions": 1.0, "num_y_partitions": 1.0, "signal_color_space": 0.0,
		"coef_log2_denom": before["coef_log2_denom"], "bl_bit_depth": before["bl_bit_depth"], "vdr_bit_depth": before["vdr_bit_depth"],
		"source_min_pq": before["source_min_pq"], "source_max_pq": before["source_max_pq"], "scene_refresh_flag": before["scene_refresh_flag"],
		"ycc_to_rgb_matrix": "9574/8192 0/8192 13802/8192 9574/8192 -1540/8192 -5348/8192 9574/8192 17610/8192 0/8192",
		"ycc_to_rgb_offset": "16777216/268435456 134217728/268435456 134217728/268435456",
		"rgb_to_lms_matrix": "7222/16384 8771/16384 390/16384 2654/16384 12430/16384 1300/16384 0/16384 422/16384 15962/16384",
	})
	want := was
	if full {
		want = []any{identity, identity, identity}
	}
	if now := curves(after); !reflect.DeepEqual(now, want) {
		tb.Errorf("curves (full enhancement layer: %v) are\n%v\nwant\n%v", full, now, want)
	}
	for _, c := range after["components"].([]any) {
		if _, has := c.(map[string]any)["nlq_offset"]; has {
			tb.Errorf("a component still carries the quantizer of the residual: %v", c)
		}
	}
	if !reflect.DeepEqual(got.hdr(), in.hdr()) || in.hdr()["max_luminance"] != 1000 || in.hdr()["max_content"] != 1000 {
		tb.Errorf("static HDR metadata %v, the source has %v", got.hdr(), in.hdr())
	}
}

// CheckBase holds ffprobe's reading of out, the base layer of the fixture src: no Dolby Vision left, in the stream or
// in the frame, and the base layer's own HDR description kept.
func CheckBase(tb testing.TB, src, out string) {
	tb.Helper()
	in, got := FFprobe(tb, src), FFprobe(tb, out)
	if got.Config() != nil || got.RPU() != nil || side(got.Frames[0].SideData, "Dolby Vision RPU Data") != nil {
		tb.Errorf("the base layer still carries Dolby Vision: configuration %v, RPU %v", got.Config(), got.RPU())
	}
	if !reflect.DeepEqual(got.hdr(), in.hdr()) || in.hdr()["max_luminance"] != 1000 || in.hdr()["max_content"] != 1000 {
		tb.Errorf("static HDR metadata %v, the source has %v", got.hdr(), in.hdr())
	}
}

// crcDiagnostic opens what the decoder behind ffprobe logs for an RPU whose CRC does not match its bytes.
const crcDiagnostic = "RPU CRC mismatch"

// DecodeChecked decodes path with the ffprobe named by MKVGO_FFPROBE told to check CRCs and fail on them, and returns
// its video frames with what it logged. The exit status is not looked at: it stays 0 on a frame decoded without its
// RPU.
func DecodeChecked(tb testing.TB, path string) (frames []map[string]any, log string) {
	tb.Helper()
	bin := os.Getenv("MKVGO_FFPROBE")
	if bin == "" {
		tb.Skip("set MKVGO_FFPROBE to an ffprobe binary")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-v", "warning", "-err_detect", "crccheck+explode", "-of", "json", "-select_streams", "v:0", "-show_frames", path)
	cmd.Stderr = &stderr
	out, _ := cmd.Output()
	var got struct {
		Frames []map[string]any `json:"frames"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		tb.Fatalf("ffprobe %s: %v in\n%s\n%s", path, err, out, stderr.Bytes())
	}
	return got.Frames, stderr.String()
}

// CheckCRCJudged holds an outside decoder's verdict on two one-frame files that differ by the CRC of their RPU alone:
// good decodes to the profile 8.1 metadata with no CRC diagnostic, bad decodes its picture, gives no Dolby Vision
// metadata and is diagnosed by name.
func CheckCRCJudged(tb testing.TB, good, bad string) {
	tb.Helper()
	frames, log := DecodeChecked(tb, good)
	if len(frames) != 1 || frames[0]["media_type"] != "video" || strings.Contains(log, crcDiagnostic) {
		tb.Fatalf("valid file: %d frames decoded, log:\n%s", len(frames), log)
	}
	list, _ := frames[0]["side_data_list"].([]any)
	rpu := sideOf(list, "Dolby Vision Metadata")
	if rpu == nil {
		tb.Fatalf("valid file: the frame carries no decoded Dolby Vision metadata, log:\n%s", log)
	}
	expect(tb, "valid file", rpu, map[string]any{"vdr_rpu_profile": 1.0, "disable_residual_flag": 1.0, "nlq_method_idc_name": "none", "signal_color_space": 0.0})
	if now := curves(rpu); !reflect.DeepEqual(now, []any{identity, identity, identity}) {
		tb.Errorf("valid file: curves %v, want the identity", now)
	}

	frames, log = DecodeChecked(tb, bad)
	if len(frames) != 1 || frames[0]["media_type"] != "video" {
		tb.Fatalf("damaged CRC: %d frames decoded, want the picture all the same; log:\n%s", len(frames), log)
	}
	list, _ = frames[0]["side_data_list"].([]any)
	if sideOf(list, "Dolby Vision Metadata") != nil {
		tb.Errorf("damaged CRC: the frame still carries decoded Dolby Vision metadata")
	}
	if !strings.Contains(log, crcDiagnostic) {
		tb.Errorf("damaged CRC: the log does not say %q:\n%s", crcDiagnostic, log)
	}
}

func sideOf(list []any, kind string) map[string]any {
	for _, sd := range list {
		if m, _ := sd.(map[string]any); m["side_data_type"] == kind {
			return m
		}
	}
	return nil
}
