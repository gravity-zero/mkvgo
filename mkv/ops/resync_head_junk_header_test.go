package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestResyncKeepsMetadataBehindJunkThatReadsAsAnElement: junk ahead of the Tracks that happens to decode as an
// element header, or a Void declaring more than it holds, used to send the tolerant walk to the first Cluster: exit
// 0 and an output with no track. The skipped range must be the junk and nothing else.
func TestResyncKeepsMetadataBehindJunkThatReadsAsAnElement(t *testing.T) {
	for name, junk := range map[string][]byte{
		"declares more than the file holds": []byte("0000\x00\x00"),
		"declares a few bytes into Tracks":  {'0', '0', '0', 0x88, 0x00, 0x00},
		"a Void declaring 6 bytes too many": {0xEC, 0x8A, 0x00, 0x00, 0x00, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			data := livefixture.Build(livefixture.Options{})
			at := int64(bytes.Index(data, []byte{0x16, 0x54, 0xAE, 0x6B}))
			if at < 0 {
				t.Fatal("no Tracks in the fixture")
			}
			spliced := append(append(append([]byte(nil), data[:at]...), junk...), data[at:]...)
			src := filepath.Join(t.TempDir(), "junk.mkv")
			writeAll(t, src, spliced)

			var skipped []mkv.DamagedRange
			dst := filepath.Join(t.TempDir(), "resync.mkv")
			err := Reindex(ctx, src, dst, mkv.Options{
				Resync: true,
				OnSkip: func(r mkv.DamagedRange) { skipped = append(skipped, r) },
			})
			if err != nil {
				t.Fatalf("Reindex with Resync: %v", err)
			}
			if end := at + int64(len(junk)); len(skipped) != 1 || skipped[0].StartOffset != at || skipped[0].EndOffset != end {
				t.Errorf("skipped = %+v, want exactly the junk [%d,%d)", skipped, at, end)
			}
			checkLiveOutput(t, dst)
		})
	}
}

// TestDiagnoseCountsJunkBehindTheHeadIndex: with Cues and Tags ahead of them, 6 stray bytes before the first Cluster
// went unseen (the head read had all it asked for) and Diagnose answered healthy on a file the strict reindex
// refuses. The finding is there, and the count is a field, not only a sentence.
func TestDiagnoseCountsJunkBehindTheHeadIndex(t *testing.T) {
	ctx := context.Background()
	data := livefixture.Build(livefixture.Options{SizedClusters: true})
	cluster := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	head := []byte{
		0x1C, 0x53, 0xBB, 0x6B, 0x8D, 0xBB, 0x8B, 0xB3, 0x81, 0x00, 0xB7, 0x86, 0xF7, 0x81, 0x01, 0xF1, 0x81, 0x00,
		0x12, 0x54, 0xC3, 0x67, 0x8E, 0x73, 0x73, 0x8B, 0x67, 0xC8, 0x88, 0x45, 0xA3, 0x81, 0x41, 0x44, 0x87, 0x81, 0x42,
	}
	junk := []byte("0000\x00\x00")
	spliced := spliceGrowing(t, data, cluster, append(head, junk...))
	src := filepath.Join(t.TempDir(), "junk.mkv")
	writeAll(t, src, spliced)

	d, err := Diagnose(ctx, src)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.ResyncedBytes != int64(len(junk)) {
		t.Errorf("Diagnosis.ResyncedBytes = %d, want the %d junk bytes", d.ResyncedBytes, len(junk))
	}
	found := false
	for _, f := range d.Findings {
		found = found || (f.Kind == mkv.FindingDamaged && f.Scope == mkv.ScopeHead)
	}
	if !found || d.Healthy {
		t.Errorf("findings = %+v, healthy %v; want a damaged head", d.Findings, d.Healthy)
	}
}

// TestStrictRefusalOfBytesReadAsAnElementSaysWhatToDo: the strict walks met such bytes as an element running past
// the end and said "copy unknown 0x303030 body: EOF", or "truncated file" with advice to run the command that had
// just refused. They name the offset and the rewrite that reads past it.
func TestStrictRefusalOfBytesReadAsAnElementSaysWhatToDo(t *testing.T) {
	ctx := context.Background()
	data := livefixture.Build(livefixture.Options{SizedClusters: true})
	cluster := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	spliced := spliceGrowing(t, data, cluster, []byte("0000\x00\x00"))
	for name, run := range map[string]func(src string) error{
		"reindex":         func(src string) error { return Reindex(ctx, src, filepath.Join(t.TempDir(), "out.mkv")) },
		"reindex inplace": func(src string) error { return ReindexInPlace(ctx, src) },
	} {
		src := filepath.Join(t.TempDir(), "junk.mkv")
		writeAll(t, src, spliced)
		err := run(src)
		if err == nil {
			t.Fatalf("%s accepted the file", name)
		}
		for _, want := range []string{"reindex --resync", "offset"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q does not say %q", name, err, want)
			}
		}
	}
}

// spliceGrowing returns data with extra spliced in at offset at, its sized Segment grown by as much.
func spliceGrowing(t *testing.T, data []byte, at int, extra []byte) []byte {
	t.Helper()
	r := bytes.NewReader(data)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	segAt := int64(n) + h.Size
	if _, err := r.Seek(segAt, 0); err != nil {
		t.Fatal(err)
	}
	seg, segHdr, err := ebml.ReadElementHeader(r)
	if err != nil || seg.ID != mkv.IDSegment || seg.Size < 0 {
		t.Fatalf("Segment header: 0x%X, size %d, %v", seg.ID, seg.Size, err)
	}
	out := append(append(append([]byte(nil), data[:at]...), extra...), data[at:]...)
	var size bytes.Buffer
	if _, err := ebml.WriteDataSizeWidth(&size, seg.Size+int64(len(extra)), segHdr-4); err != nil {
		t.Fatal(err)
	}
	copy(out[segAt+4:], size.Bytes())
	return out
}

// TestAnalyzeSaysWhatItPassedOver: the stats of a file read past stray bytes are whole, and say the file is not.
func TestAnalyzeSaysWhatItPassedOver(t *testing.T) {
	data := livefixture.Build(livefixture.Options{SizedClusters: true})
	cluster := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	src := filepath.Join(t.TempDir(), "junk.mkv")
	writeAll(t, src, spliceGrowing(t, data, cluster, []byte("0000\x00\x00")))
	r, err := Analyze(context.Background(), src)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if r.ClusterCount != livefixture.Clusters {
		t.Errorf("clusters = %d, want %d", r.ClusterCount, livefixture.Clusters)
	}
	told := false
	for _, w := range r.Warnings {
		told = told || (strings.Contains(w, "6 undecodable byte(s)") && strings.Contains(w, "reindex --resync"))
	}
	if !told {
		t.Errorf("warnings = %q, want the 6 bytes passed over and the command that removes them", r.Warnings)
	}
}
