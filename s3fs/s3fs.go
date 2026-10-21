// Package s3fs implements a read-only mkv.FS port over S3 (or an
// S3-compatible service), reusing httpfs for the actual Range/window
// mechanics: it builds an *http.Client whose RoundTripper signs every request
// with AWS Signature Version 4 (stdlib crypto/hmac + crypto/sha256 only, no
// SDK dependency), then delegates the ranged reads to httpfs.
//
//	fs := s3fs.New(s3fs.Options{Region: "us-east-1"})
//	c, err := matroska.OpenMetaWithFS(ctx, "s3://my-bucket/movies/one.mkv", fs.Port())
//
// Credentials, region and a custom endpoint fall back to the standard AWS
// environment variables when left empty in Options: AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION (or
// AWS_DEFAULT_REGION), AWS_ENDPOINT_URL.
//
// A bucket name must follow the S3 naming rules, and a redirect is returned
// as an error rather than followed: the signed request, with its credentials,
// only ever goes to the endpoint's host.
package s3fs

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Options configures New.
type Options struct {
	// Region is the AWS region (e.g. "us-east-1"). Falls back to the
	// AWS_REGION then AWS_DEFAULT_REGION environment variables, then
	// "us-east-1".
	Region string
	// Endpoint overrides the S3 host, for S3-compatible services. Empty
	// means https://s3.<region>.amazonaws.com. Falls back to
	// AWS_ENDPOINT_URL. May include a scheme (http:// or https://); a bare
	// host:port is treated as https.
	Endpoint string
	// AccessKey, SecretKey, SessionToken are the SigV4 credentials. Empty
	// falls back to AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and
	// AWS_SESSION_TOKEN respectively.
	AccessKey, SecretKey, SessionToken string
	// PathStyle requests https://<host>/<bucket>/<key> URLs instead of the
	// default virtual-hosted https://<bucket>.<host>/<key> form. Some
	// S3-compatible services (and buckets with dots in their name) need it.
	PathStyle bool
	// WindowSize is the ranged-fetch granularity in bytes, passed through to
	// httpfs.Options.WindowSize; <= 0 means httpfs's default (512 KiB).
	WindowSize int
	// RequestTimeout bounds each request, passed through to httpfs.Options.RequestTimeout: 0 means 60 s, negative none.
	RequestTimeout time.Duration
	// AllowHTTPSessionToken sends a SessionToken to a plain http:// endpoint, where it crosses the network in clear; without it such a read is refused.
	AllowHTTPSessionToken bool
}

// S3FS is the S3-backed read-only filesystem. Construct with New; use Port to
// get the mkv.FS to pass to a reader (matroska.OpenMetaWithFS, mp4.OpenMeta,
// ...). Paths are s3://bucket/key.
type S3FS struct {
	opts   Options
	scheme string
	host   string
	client *http.Client
	hfs    *httpfs.FS
}

// s3Prefix is the URL scheme this package serves.
const s3Prefix = "s3://"

// IsURL reports whether path is an s3:// URL.
func IsURL(path string) bool {
	return strings.HasPrefix(path, s3Prefix)
}

// defaultTransport keeps up to 64 idle connections per host, what a server reads at once (http.DefaultTransport keeps 2).
var defaultTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	return t
}()

// New builds an S3FS. Region/credentials/endpoint left empty fall back to the
// environment (see the Options doc); Region still empty after that defaults
// to "us-east-1".
func New(opts Options) *S3FS {
	if opts.Region == "" {
		opts.Region = os.Getenv("AWS_REGION")
	}
	if opts.Region == "" {
		opts.Region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	if opts.AccessKey == "" {
		opts.AccessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if opts.SecretKey == "" {
		opts.SecretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if opts.SessionToken == "" {
		opts.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
	if opts.Endpoint == "" {
		opts.Endpoint = os.Getenv("AWS_ENDPOINT_URL")
	}

	scheme, host := "https", "s3."+opts.Region+".amazonaws.com"
	if opts.Endpoint != "" {
		scheme, host = parseEndpoint(opts.Endpoint)
	}

	transport := &signingTransport{
		base:         defaultTransport,
		accessKey:    opts.AccessKey,
		secretKey:    opts.SecretKey,
		sessionToken: opts.SessionToken,
		region:       opts.Region,
	}
	// A redirect is returned, not followed: following it would sign the next
	// hop and send the credentials to whatever host it names.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &S3FS{
		opts:   opts,
		scheme: scheme,
		host:   host,
		client: client,
		hfs:    httpfs.New(httpfs.Options{Client: client, WindowSize: opts.WindowSize, RequestTimeout: opts.RequestTimeout}),
	}
}

// BytesFetched returns the total bytes transferred so far through this FS's
// httpfs-backed reads.
func (s *S3FS) BytesFetched() int64 { return s.hfs.BytesFetched() }

// parseEndpoint splits an endpoint into (scheme, host), defaulting to https
// when the endpoint carries no scheme; a trailing slash, as AWS_ENDPOINT_URL is often written, is dropped.
func parseEndpoint(e string) (scheme, host string) {
	scheme, host = "https", e
	if i := strings.Index(e, "://"); i >= 0 {
		scheme, host = e[:i], e[i+3:]
	}
	return scheme, strings.TrimRight(host, "/")
}

// Port returns the mkv.FS wired to this S3 filesystem. It is read-only:
// every mutating operation returns an error.
func (s *S3FS) Port() *mkv.FS {
	inner := s.hfs.Port()
	roErr := func(op string) error { return fmt.Errorf("s3fs: %s: read-only filesystem", op) }

	return &mkv.FS{
		Open: func(path string) (mkv.ReadSeekCloser, error) {
			u, err := s.resolveURL(path)
			if err != nil {
				return nil, err
			}
			return inner.DoOpen(u)
		},
		OpenContext: func(ctx context.Context, path string) (mkv.ReadSeekCloser, error) {
			u, err := s.resolveURL(path)
			if err != nil {
				return nil, err
			}
			return inner.DoOpenContext(ctx, u)
		},
		Stat: func(path string) (os.FileInfo, error) {
			u, err := s.resolveURL(path)
			if err != nil {
				return nil, err
			}
			return inner.DoStat(u)
		},
		Create:    func(string) (mkv.WriteSeekCloser, error) { return nil, roErr("create") },
		OpenFile:  func(string, int, os.FileMode) (mkv.ReadWriteSeekCloser, error) { return nil, roErr("open-file") },
		MkdirAll:  func(string, os.FileMode) error { return roErr("mkdir") },
		WriteFile: func(string, []byte, os.FileMode) error { return roErr("write") },
		Remove:    func(string) error { return roErr("remove") },
		Rename:    func(string, string) error { return roErr("rename") },
	}
}

// resolveURL turns an s3://bucket/key path into the concrete HTTPS URL for
// this FS's endpoint and style.
func (s *S3FS) resolveURL(path string) (string, error) {
	bucket, key, err := splitS3Path(path)
	if err != nil {
		return "", err
	}
	if s.opts.SessionToken != "" && strings.EqualFold(s.scheme, "http") && !s.opts.AllowHTTPSessionToken {
		return "", fmt.Errorf("s3fs: %s: the endpoint is plain http and the session token would cross the network in clear; use an https:// endpoint, or credentials without a session token (unset AWS_SESSION_TOKEN), or set s3fs.Options.AllowHTTPSessionToken to send it anyway", path)
	}
	var b strings.Builder
	b.WriteString(s.scheme)
	b.WriteString("://")
	if s.opts.PathStyle {
		b.WriteString(s.host)
		b.WriteByte('/')
		b.WriteString(uriEncodeS3Path(bucket))
		if key != "" {
			b.WriteByte('/')
			b.WriteString(uriEncodeS3Path(key))
		}
	} else {
		b.WriteString(bucket)
		b.WriteByte('.')
		b.WriteString(s.host)
		b.WriteByte('/')
		b.WriteString(uriEncodeS3Path(key))
	}
	u, err := url.Parse(b.String())
	want := s.host
	if !s.opts.PathStyle {
		want = bucket + "." + s.host
	}
	if err != nil || u.Host != want {
		return "", fmt.Errorf("s3fs: %s does not resolve to host %s", path, want)
	}
	return b.String(), nil
}

// splitS3Path parses "s3://bucket/key" into (bucket, key). key may be empty
// (a bucket-root reference); bucket must be a valid S3 bucket name, which
// also keeps it from rewriting the host it is written in front of.
func splitS3Path(path string) (bucket, key string, err error) {
	if !IsURL(path) {
		return "", "", fmt.Errorf("s3fs: not an s3:// URL: %s", path)
	}
	rest := path[len(s3Prefix):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		bucket = rest
	} else {
		bucket, key = rest[:i], rest[i+1:]
	}
	if bucket == "" {
		return "", "", fmt.Errorf("s3fs: missing bucket in %s", path)
	}
	if err := checkBucket(bucket); err != nil {
		return "", "", fmt.Errorf("s3fs: %s: %w", path, err)
	}
	for seg := range strings.SplitSeq(key, "/") {
		if seg == "." || seg == ".." {
			return "", "", fmt.Errorf("s3fs: %s: a key segment %q could step out of the bucket on a server or proxy that resolves it; rename the object", path, seg)
		}
	}
	return bucket, key, nil
}

// checkBucket enforces the S3 bucket naming rules: 3 to 63 characters of
// [a-z0-9.-], alphanumeric at both ends, no "..", not an IPv4 address.
func checkBucket(b string) error {
	if len(b) < 3 || len(b) > 63 {
		return fmt.Errorf("bucket name %q is not 3 to 63 characters long", b)
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && c != '.' && c != '-' || !alnum && (i == 0 || i == len(b)-1) {
			return fmt.Errorf("bucket name %q is not lowercase letters, digits, dots and hyphens between a letter or digit at each end", b)
		}
	}
	if strings.Contains(b, "..") {
		return fmt.Errorf("bucket name %q holds two adjacent dots", b)
	}
	if a, err := netip.ParseAddr(b); err == nil && a.Is4() {
		return fmt.Errorf("bucket name %q is an IP address", b)
	}
	return nil
}

// uriEncodeS3Path percent-encodes a path per the S3 URI-encoding rule used in
// both the request URL and the SigV4 canonical request: each "/"-separated
// segment is percent-encoded (RFC 3986 unreserved characters left as-is,
// everything else escaped as uppercase %XX), and the slashes themselves are
// kept literal.
func uriEncodeS3Path(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = uriEncodeS3Segment(seg)
	}
	return strings.Join(segs, "/")
}

const s3UnreservedChars = "-_.~"

func uriEncodeS3Segment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte(s3UnreservedChars, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
