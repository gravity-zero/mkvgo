package s3fs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// signingTransport wraps an http.RoundTripper, signing every outgoing
// request with AWS Signature Version 4 (unsigned payload: GET/HEAD range
// reads never need to sign a body) before handing it to base.
type signingTransport struct {
	base         http.RoundTripper
	accessKey    string
	secretKey    string
	sessionToken string
	region       string
}

func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	signRequest(r, t.accessKey, t.secretKey, t.sessionToken, t.region, time.Now().UTC())

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// amzDateLayout and dateStampLayout are the two SigV4 timestamp formats: the
// full ISO 8601 basic-format instant (x-amz-date) and the date-only scope
// used in the credential scope and key-derivation chain.
const (
	amzDateLayout   = "20060102T150405Z"
	dateStampLayout = "20060102"
)

// signRequest adds x-amz-date, x-amz-content-sha256 (always UNSIGNED-PAYLOAD:
// this package only ever issues Range-read GETs, never a signed body),
// x-amz-security-token (when sessionToken is set) and a SigV4 Authorization
// header to req, computed against `now`. It mutates req.Header in place and
// returns the Authorization value for callers (tests) that want to inspect it
// directly. `now` is a parameter (rather than always time.Now()) so the
// signature is reproducible for a fixed instant, both for testing and because
// a real HTTP round trip must use one single instant for the signature to
// verify against what was actually sent.
func signRequest(req *http.Request, accessKey, secretKey, sessionToken, region string, now time.Time) string {
	amzDate := now.Format(amzDateLayout)
	dateStamp := now.Format(dateStampLayout)

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if sessionToken != "" {
		req.Header.Set("x-amz-security-token", sessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": "UNSIGNED-PAYLOAD",
		"x-amz-date":           amzDate,
	}
	if v := req.Header.Get("Range"); v != "" {
		headers["range"] = v
	}
	if sessionToken != "" {
		headers["x-amz-security-token"] = sessionToken
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)

	var chLines []string
	for _, n := range names {
		chLines = append(chLines, n+":"+strings.TrimSpace(headers[n]))
	}
	canonicalHeaders := strings.Join(chLines, "\n") + "\n"
	signedHeaders := strings.Join(names, ";")

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		req.Method, canonicalURI, canonicalQuery(req), canonicalHeaders, signedHeaders, "UNSIGNED-PAYLOAD",
	}, "\n")

	credScope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, credScope, sha256Hex(canonicalRequest),
	}, "\n")

	key := signingKey(secretKey, dateStamp, region)
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credScope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
	return auth
}

// canonicalQuery returns the sorted, percent-encoded canonical query string.
// This package never issues a request with query parameters, so it is always
// empty today; kept as a real (if trivial) step so the canonical request
// construction matches the SigV4 algorithm exactly, not a special case of it.
func canonicalQuery(req *http.Request) string {
	q := req.URL.Query()
	if len(q) == 0 {
		return ""
	}
	return q.Encode()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// signingKey derives the SigV4 signing key: HMAC-SHA256 chained through the
// date, region and "s3" service, terminated with the "aws4_request" constant.
func signingKey(secretKey, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}
