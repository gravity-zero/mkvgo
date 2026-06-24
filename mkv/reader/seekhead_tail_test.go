package reader

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// voidElem is a Void element with n zero payload bytes — cheap filler for a
// cluster body the parser skips by size.
func voidElem(n int) []byte {
	var b bytes.Buffer
	ebml.WriteElementHeader(&b, mkv.IDVoid, int64(n))
	b.Write(make([]byte, n))
	return b.Bytes()
}

// bigCluster builds a Cluster whose body is ~payload bytes of zeros, large
// enough (when > the read buffer) that walking past it costs a real read.
func bigCluster(payload int) []byte {
	return masterElem(mkv.IDCluster, uintElem(mkv.IDTimestamp, 0, 1), voidElem(payload))
}

func tagsElem(name, value string) []byte {
	return masterElem(mkv.IDTags,
		masterElem(mkv.IDTag,
			masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, name), strElem(mkv.IDTagString, value)),
		),
	)
}

const tailCueCount = 8

// buildTailMKV assembles [SeekHead?][Info][Tracks][Cluster×n][Cues][Tags]. The
// Cues and Tags sit AFTER the clusters, so a sequential walk must cross every
// cluster to reach them; with a SeekHead the reader can jump straight to the
// tail. When staleCuesPos is set, the SeekHead's Cues offset is poisoned to land
// inside a cluster, exercising the stale-offset fallback.
func buildTailMKV(t *testing.T, withSeekHead bool, nClusters, clusterSize int, staleCues bool) []byte {
	t.Helper()
	info, tracks := infoElem(), tracksElem()
	cues := cuesElem(tailCueCount)
	tags := tagsElem("ENCODER", "mkvgo-test")
	cluster := bigCluster(clusterSize)

	clusters := make([]byte, 0, nClusters*len(cluster))
	for i := 0; i < nClusters; i++ {
		clusters = append(clusters, cluster...)
	}

	if !withSeekHead {
		return segmentMKV(info, tracks, clusters, cues, tags)
	}

	sh := func(infoP, tracksP, cuesP, tagsP uint64) []byte {
		return seekHeadElem(
			seekEntry(mkv.IDInfo, infoP),
			seekEntry(mkv.IDTracks, tracksP),
			seekEntry(mkv.IDCues, cuesP),
			seekEntry(mkv.IDTags, tagsP),
		)
	}
	shLen := uint64(len(sh(0, 0, 0, 0)))
	infoPos := shLen
	tracksPos := infoPos + uint64(len(info))
	clustersStart := tracksPos + uint64(len(tracks))
	cuesPos := clustersStart + uint64(len(clusters))
	tagsPos := cuesPos + uint64(len(cues))

	if staleCues {
		cuesPos = clustersStart + uint64(len(cluster)/2) // mid-cluster: zero padding
	}

	head := sh(infoPos, tracksPos, cuesPos, tagsPos)
	if uint64(len(head)) != shLen {
		t.Fatalf("SeekHead length not fixed-width: %d vs %d", len(head), shLen)
	}
	return segmentMKV(head, info, tracks, clusters, cues, tags)
}

// assertTailParsed checks the post-cluster Cues and Tags were both collected and
// the keyframe index was derived — i.e. no tail element was missed.
func assertTailParsed(t *testing.T, c *mkv.Container) {
	t.Helper()
	if len(c.Cues) != tailCueCount {
		t.Errorf("Cues = %d, want %d", len(c.Cues), tailCueCount)
	}
	if len(c.Keyframes) != tailCueCount {
		t.Errorf("Keyframes = %d, want %d", len(c.Keyframes), tailCueCount)
	}
	if len(c.Tags) != 1 {
		t.Errorf("Tags = %d, want 1 (tail Tags element must not be skipped)", len(c.Tags))
	}
	if len(c.Tracks) != 2 {
		t.Errorf("Tracks = %d, want 2", len(c.Tracks))
	}
}

// TestFullReadSeekHeadSkipsClusterRegion proves a full Read with a SeekHead jumps
// over the whole cluster region in one seek: the number of underlying Read calls
// is identical whether there are 20 clusters or 200, because none of them are
// read. Correctness is checked too — the tail Cues and Tags are still parsed.
func TestFullReadSeekHeadSkipsClusterRegion(t *testing.T) {
	const clusterSize = 128 << 10 // > fullReadBufSize, so walking one costs a read

	small := &callCountingReadSeeker{rs: bytes.NewReader(buildTailMKV(t, true, 20, clusterSize, false))}
	cSmall, err := Read(context.Background(), small, "small.mkv")
	if err != nil {
		t.Fatalf("Read(20 clusters): %v", err)
	}
	large := &callCountingReadSeeker{rs: bytes.NewReader(buildTailMKV(t, true, 200, clusterSize, false))}
	cLarge, err := Read(context.Background(), large, "large.mkv")
	if err != nil {
		t.Fatalf("Read(200 clusters): %v", err)
	}

	assertTailParsed(t, cSmall)
	assertTailParsed(t, cLarge)

	// 10x the clusters, but they are all seek-skipped, so the read-call count
	// must not grow with cluster count.
	if small.calls != large.calls {
		t.Errorf("Read calls grew with cluster count: 20 clusters=%d, 200 clusters=%d (the cluster region must be skipped via the SeekHead)", small.calls, large.calls)
	}
	t.Logf("SeekHead jump: %d Read calls regardless of cluster count", small.calls)
}

// buildHeadCuesMKV assembles [SeekHead(Info,Tracks,Cues)][Info][Tracks][Cues]
// [Cluster×n] — the Cues sit BEFORE the clusters (a real muxer layout, e.g. some
// Blu-ray remuxes). The SeekHead indexes only head elements, so nothing lies
// past the clusters and a full Read must STOP at the first cluster rather than
// walk all of them to EOF looking for something that is not there.
func buildHeadCuesMKV(t *testing.T, nClusters, clusterSize int) []byte {
	t.Helper()
	info, tracks := infoElem(), tracksElem()
	cues := cuesElem(tailCueCount)
	cluster := bigCluster(clusterSize)
	clusters := make([]byte, 0, nClusters*len(cluster))
	for i := 0; i < nClusters; i++ {
		clusters = append(clusters, cluster...)
	}

	sh := func(infoP, tracksP, cuesP uint64) []byte {
		return seekHeadElem(
			seekEntry(mkv.IDInfo, infoP),
			seekEntry(mkv.IDTracks, tracksP),
			seekEntry(mkv.IDCues, cuesP),
		)
	}
	shLen := uint64(len(sh(0, 0, 0)))
	infoPos := shLen
	tracksPos := infoPos + uint64(len(info))
	cuesPos := tracksPos + uint64(len(tracks))

	head := sh(infoPos, tracksPos, cuesPos)
	if uint64(len(head)) != shLen {
		t.Fatalf("SeekHead length not fixed-width: %d vs %d", len(head), shLen)
	}
	return segmentMKV(head, info, tracks, cues, clusters)
}

// TestFullReadStopsWhenSeekHeadHasNoTail is the Doctor Strange regression: with
// the Cues before the clusters and a SeekHead that indexes only head elements,
// the reader must stop at the first cluster, never walking the cluster region.
// The read-call count must therefore be identical for 20 and 200 clusters.
func TestFullReadStopsWhenSeekHeadHasNoTail(t *testing.T) {
	const clusterSize = 128 << 10

	small := &callCountingReadSeeker{rs: bytes.NewReader(buildHeadCuesMKV(t, 20, clusterSize))}
	cSmall, err := Read(context.Background(), small, "small.mkv")
	if err != nil {
		t.Fatalf("Read(20 clusters): %v", err)
	}
	large := &callCountingReadSeeker{rs: bytes.NewReader(buildHeadCuesMKV(t, 200, clusterSize))}
	_, err = Read(context.Background(), large, "large.mkv")
	if err != nil {
		t.Fatalf("Read(200 clusters): %v", err)
	}

	if len(cSmall.Cues) != tailCueCount || len(cSmall.Tracks) != 2 {
		t.Fatalf("head metadata not parsed: cues=%d tracks=%d", len(cSmall.Cues), len(cSmall.Tracks))
	}
	if small.calls != large.calls {
		t.Errorf("Read walked the clusters instead of stopping: 20 clusters=%d calls, 200 clusters=%d calls", small.calls, large.calls)
	}
	t.Logf("head-only SeekHead: %d Read calls regardless of cluster count (no walk)", small.calls)
}

// TestFullReadTailFallbackNoSeekHead checks the fallback: with no SeekHead the
// reader locates them by scanning back from EOF (no cluster walk), so the read
// count does not grow with the cluster count and the tail is parsed correctly.
func TestFullReadNoSeekHeadTailCuesScan(t *testing.T) {
	const clusterSize = 128 << 10

	small := &callCountingReadSeeker{rs: bytes.NewReader(buildTailMKV(t, false, 20, clusterSize, false))}
	cSmall, err := Read(context.Background(), small, "small.mkv")
	if err != nil {
		t.Fatalf("Read(20 clusters, no seekhead): %v", err)
	}
	large := &callCountingReadSeeker{rs: bytes.NewReader(buildTailMKV(t, false, 200, clusterSize, false))}
	cLarge, err := Read(context.Background(), large, "large.mkv")
	if err != nil {
		t.Fatalf("Read(200 clusters, no seekhead): %v", err)
	}

	assertTailParsed(t, cSmall)
	assertTailParsed(t, cLarge)

	if small.calls != large.calls {
		t.Errorf("the tail scan should not read more with more clusters: 20=%d, 200=%d", small.calls, large.calls)
	}
}

// buildNoCuesMKV assembles [Info][Tracks][Cluster×n] with no SeekHead and no
// Cues — the only layout that still forces a full cluster walk to EOF, since
// there is nothing at the tail to scan for.
func buildNoCuesMKV(t *testing.T, nClusters, clusterSize int) []byte {
	t.Helper()
	cluster := bigCluster(clusterSize)
	clusters := make([]byte, 0, nClusters*len(cluster))
	for i := 0; i < nClusters; i++ {
		clusters = append(clusters, cluster...)
	}
	return segmentMKV(infoElem(), tracksElem(), clusters)
}

// TestFullReadStaleSeekHeadFallsBack poisons the SeekHead's Cues offset so it
// points inside a cluster. The reader must reject it (the header there does not
// decode to a segment-level element) and fall back to walking, still recovering
// the real tail Cues and Tags rather than parsing garbage.
func TestFullReadStaleSeekHeadFallsBack(t *testing.T) {
	data := buildTailMKV(t, true, 30, 64<<10, true)
	c, err := Read(context.Background(), bytes.NewReader(data), "stale.mkv")
	if err != nil {
		t.Fatalf("Read with stale SeekHead: %v", err)
	}
	assertTailParsed(t, c)
}

// TestFullReadSeekHeadParity asserts the SeekHead fast path and the no-SeekHead
// walk return identical metadata for the same logical content.
func TestFullReadSeekHeadParity(t *testing.T) {
	withSH, err := Read(context.Background(), bytes.NewReader(buildTailMKV(t, true, 16, 64<<10, false)), "a.mkv")
	if err != nil {
		t.Fatalf("Read with SeekHead: %v", err)
	}
	without, err := Read(context.Background(), bytes.NewReader(buildTailMKV(t, false, 16, 64<<10, false)), "b.mkv")
	if err != nil {
		t.Fatalf("Read without SeekHead: %v", err)
	}

	if len(withSH.Cues) != len(without.Cues) || len(withSH.Tags) != len(without.Tags) ||
		len(withSH.Tracks) != len(without.Tracks) || len(withSH.Keyframes) != len(without.Keyframes) {
		t.Errorf("SeekHead vs walk mismatch: cues %d/%d tags %d/%d tracks %d/%d keyframes %d/%d",
			len(withSH.Cues), len(without.Cues), len(withSH.Tags), len(without.Tags),
			len(withSH.Tracks), len(without.Tracks), len(withSH.Keyframes), len(without.Keyframes))
	}
	if !reflect.DeepEqual(withSH.Info, without.Info) {
		t.Errorf("Info mismatch:\n seekhead = %+v\n walk     = %+v", withSH.Info, without.Info)
	}
}
