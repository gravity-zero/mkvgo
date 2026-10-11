package mp4

import (
	"context"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
)

// Over a remote source, a plan stats it with no HEAD (its first responses tell
// the size and version) and a segment is one request per
// pass over its window (structure, then bytes when streamed), whatever the
// window size: each request covers the byte range the walk is about to read.
// Before the change: 3 HEAD for the plan, and here 4 to 16 requests a segment.
func TestRemotePlanRequestCount(t *testing.T) {
	src := openGOPSource(t, 6, false, true)
	for _, stream := range []bool{false, true} {
		rs := newRemoteSource(t, src)
		ctx := context.Background()
		fs := httpfs.New(httpfs.Options{WindowSize: 512}).Port()
		plan, err := PlanHLS(ctx, rs.srv.URL+"/s.mkv", Options{SegmentMs: openGOPMs, FS: fs, StreamFromFS: stream})
		if err != nil {
			t.Fatal(err)
		}
		gets, heads, _ := rs.counts()
		t.Logf("stream=%v plan: %d GET, %d HEAD", stream, gets, heads)
		if heads != 0 {
			t.Errorf("stream=%v: the plan made %d HEAD, want none", stream, heads)
		}
		for n := 2; n <= 4; n++ {
			g0, _, _ := rs.counts()
			h, err := plan.Open(ctx, plan.SegmentName(n-1))
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if _, err := h.WriteTo(ctx, &out); err != nil {
				t.Fatal(err)
			}
			g1, _, _ := rs.counts()
			limit := 1
			if stream {
				limit = 2
			}
			if n == 2 { // the first window served cold winds its audio grid over the one before (warmGridStart)
				limit++
			}
			if g1-g0 > limit {
				t.Errorf("stream=%v segment %d: %d requests, want at most %d", stream, n, g1-g0, limit)
			}
			want, _, err := plan.Resource(ctx, plan.SegmentName(n-1))
			if err != nil || out.String() != string(want) {
				t.Fatalf("segment %d: served bytes differ from the built ones (err %v)", n, err)
			}
		}
	}
}
