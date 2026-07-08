package s3fs

import (
	"net/http"
	"testing"
	"time"
)

// TestSigV4_KnownVectors checks the exact Authorization header against two
// hand-derived known-good vectors. Both use the credentials, bucket name and
// date from the published AWS Signature Version 4 documentation's canonical
// "GET Object" walkthrough example (access key AKIAIOSFODNN7EXAMPLE, secret
// wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY, bucket "examplebucket", date
// 2013-05-24) - adapted here to this package's UNSIGNED-PAYLOAD convention
// (the documented walkthrough signs the actual, empty, request body instead,
// so its own published signature does not apply verbatim). The expected
// signature/Authorization strings below were computed independently from the
// documented canonical-request format via crypto/hmac + crypto/sha256 (the
// same primitives production code uses), not copied from production code
// itself, so a bug in the canonical-request assembly, the signing-key
// derivation chain or the header formatting is caught either way.
func TestSigV4_KnownVectors(t *testing.T) {
	fixedTime := func(rfc3339 string) time.Time {
		tm, err := time.Parse(time.RFC3339, rfc3339)
		if err != nil {
			t.Fatal(err)
		}
		return tm.UTC()
	}

	t.Run("virtual-host GET with Range", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-9")
		now := fixedTime("2013-05-24T00:00:00Z")

		got := signRequest(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "", "us-east-1", now)

		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
			"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
			"Signature=edacce68e5445863e1f916719fac26d3be9c1581fccd7878ade0879597fc0dc1"
		if got != want {
			t.Errorf("Authorization mismatch:\n got:  %s\n want: %s", got, want)
		}
		if req.Header.Get("x-amz-content-sha256") != "UNSIGNED-PAYLOAD" {
			t.Errorf("x-amz-content-sha256 = %q, want UNSIGNED-PAYLOAD", req.Header.Get("x-amz-content-sha256"))
		}
		if req.Header.Get("x-amz-date") != "20130524T000000Z" {
			t.Errorf("x-amz-date = %q", req.Header.Get("x-amz-date"))
		}
		if req.Header.Get("Authorization") != want {
			t.Errorf("Authorization header not set on the request")
		}
	})

	t.Run("path-style GET with session token, encoded key", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "https://s3.eu-west-1.amazonaws.com/examplebucket/some/nested%20key.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		now := fixedTime("2020-01-01T12:00:00Z")

		got := signRequest(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "EXAMPLESESSIONTOKEN1234567890", "eu-west-1", now)

		want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20200101/eu-west-1/s3/aws4_request, " +
			"SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token, " +
			"Signature=3294d36fd77bd835725ff9e3ca69c13e1b55020ca1c8b23bb8adfe381802b857"
		if got != want {
			t.Errorf("Authorization mismatch:\n got:  %s\n want: %s", got, want)
		}
		if req.Header.Get("x-amz-security-token") != "EXAMPLESESSIONTOKEN1234567890" {
			t.Errorf("x-amz-security-token not set")
		}
	})
}

// TestUriEncodeS3Path checks the S3 URI-encoding rule (segments encoded,
// slashes kept) matches what the second known-good vector's request URL
// assumes.
func TestUriEncodeS3Path(t *testing.T) {
	got := uriEncodeS3Path("some/nested key.txt")
	want := "some/nested%20key.txt"
	if got != want {
		t.Errorf("uriEncodeS3Path = %q, want %q", got, want)
	}
}
