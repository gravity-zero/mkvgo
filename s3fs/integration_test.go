package s3fs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// Integration against a real S3 service, opt-in:
// MKVGO_IT_S3="http://127.0.0.1:19000|<access key>|<secret key>".
func itS3(t *testing.T) (endpoint, access, secret string) {
	parts := strings.Split(os.Getenv("MKVGO_IT_S3"), "|")
	if len(parts) != 3 {
		t.Skip("set MKVGO_IT_S3=endpoint|access|secret to run against a real S3 service")
	}
	return parts[0], parts[1], parts[2]
}

// put sends one signed PUT (a bucket when key is empty) and returns the ETag.
func put(t *testing.T, endpoint, access, secret, bucket, key string, body []byte) string {
	t.Helper()
	url := endpoint + "/" + bucket
	if key != "" {
		url += "/" + uriEncodeS3Path(key)
	}
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	signRequest(req, access, secret, "", "us-east-1", time.Now().UTC())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bucketExists := key == "" && resp.StatusCode == http.StatusConflict
	if resp.StatusCode != http.StatusOK && !bucketExists {
		t.Fatalf("PUT %s: %s %s", url, resp.Status, msg)
	}
	return resp.Header.Get("ETag")
}

func TestIntegrationS3(t *testing.T) {
	endpoint, access, secret := itS3(t)
	const bucket = "mkvgo-it"
	put(t, endpoint, access, secret, bucket, "", nil)
	s3 := New(Options{Endpoint: endpoint, AccessKey: access, SecretKey: secret, PathStyle: true, WindowSize: 256 << 10})
	port := s3.Port()
	const size = 3 << 20
	v1 := make([]byte, size)
	rand.New(rand.NewSource(1)).Read(v1)
	etag1 := put(t, endpoint, access, secret, bucket, "it.bin", v1)
	path := "s3://" + bucket + "/it.bin"

	fi, err := port.DoStat(path)
	if err != nil {
		t.Fatal(err)
	}
	id := fi.(mkv.FileIdentity)
	if fi.Size() != size || id.Identity() != etag1 || !id.Strong() {
		t.Fatalf("stat: size %d, identity %q strong %v (PUT gave %q)", fi.Size(), id.Identity(), id.Strong(), etag1)
	}
	t.Logf("validator %s (strong %v)", id.Identity(), id.Strong())

	f, _ := port.DoOpen(path)
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 12; i++ {
		off := rng.Int63n(size - 4096)
		buf := make([]byte, 1+rng.Intn(4096))
		f.Seek(off, io.SeekStart)
		if _, err := io.ReadFull(f, buf); err != nil || !bytes.Equal(buf, v1[off:off+int64(len(buf))]) {
			t.Fatalf("range at %d: %v", off, err)
		}
	}

	// The same bytes uploaded again keep the ETag: a reader carries on.
	if etag := put(t, endpoint, access, secret, bucket, "it.bin", v1); etag != etag1 {
		t.Logf("re-upload of identical bytes changed the ETag: %s -> %s", etag1, etag)
	}
	f.Seek(2<<20, io.SeekStart)
	same := make([]byte, 100)
	if _, err := io.ReadFull(f, same); err != nil || !bytes.Equal(same, v1[2<<20:2<<20+100]) {
		t.Errorf("identical re-upload refused or changed the bytes: %v", err)
	}

	// Another content of the same size: 412, refused.
	v2 := append([]byte(nil), v1...)
	v2[size/2] ^= 0xFF
	put(t, endpoint, access, secret, bucket, "it.bin", v2)
	f.Seek(1<<20, io.SeekStart)
	if _, err := io.ReadFull(f, make([]byte, 100)); !errors.Is(err, mkv.ErrSourceChanged) {
		t.Errorf("same size, other content: err %v, want mkv.ErrSourceChanged", err)
	}
}

// A plan over S3 serves the local plan's exact bytes, then refuses a source
// replaced under it: every segment is the old bytes or ErrSourceChanged.
func TestIntegrationS3Plan(t *testing.T) {
	endpoint, access, secret := itS3(t)
	const bucket = "mkvgo-it"
	put(t, endpoint, access, secret, bucket, "", nil)
	src := "../internal/testdata/regfix.mkv"
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	put(t, endpoint, access, secret, bucket, "plan.mkv", data)
	ctx := context.Background()
	local, err := mp4.PlanHLS(ctx, src, mp4.Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	s3 := New(Options{Endpoint: endpoint, AccessKey: access, SecretKey: secret, PathStyle: true, WindowSize: 256 << 10})
	plan, err := mp4.PlanHLS(ctx, "s3://"+bucket+"/plan.mkv", mp4.Options{SegmentMs: 1000, FS: s3.Port(), StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	var segs []string
	for _, name := range plan.Resources() {
		if strings.HasSuffix(name, ".m4s") {
			segs = append(segs, name)
		}
	}
	if len(segs) < 2 {
		t.Fatalf("only %d segments", len(segs))
	}
	serve := func(name string) ([]byte, error) {
		h, err := plan.Open(ctx, name)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		_, err = h.WriteTo(ctx, &out)
		return out.Bytes(), err
	}
	for _, name := range segs {
		want, _, err := local.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := serve(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s over S3: err %v, %d bytes, equal %v", name, err, len(got), bytes.Equal(got, want))
		}
	}
	t.Logf("%d segments over S3 identical to the local plan", len(segs))
	flipped := append([]byte(nil), data...)
	for i := len(flipped) / 4; i < len(flipped); i += 4093 {
		flipped[i] ^= 0x5A
	}
	put(t, endpoint, access, secret, bucket, "plan.mkv", flipped)
	refused := 0
	for _, name := range segs {
		want, _, _ := local.Resource(ctx, name)
		got, err := serve(name)
		switch {
		case errors.Is(err, mp4.ErrSourceChanged):
			refused++
		case err == nil && bytes.Equal(got, want):
		default:
			t.Errorf("%s after the replacement: err %v, %d bytes not the plan's", name, err, len(got))
		}
	}
	if refused == 0 {
		t.Error("the replacement was never noticed")
	}
	t.Logf("after the replacement: %d of %d segments refused, the rest served from tables already built", refused, len(segs))
}
