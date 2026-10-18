# Security policy

## Reporting a vulnerability

Report it privately through GitHub: the repository's **Security** tab,
**Report a vulnerability**. Please do not open a public issue for it.

Say which version you ran, what input or request triggers the issue (a file,
the bytes of a crafted element, a URL), and what happens: a crash, a hang, an
allocation, a read or a request it should not make, credentials or key
material where they should not be. A report is acknowledged within a week.

## Supported versions

Only the latest release receives fixes: upgrade with
`go get github.com/gravity-zero/mkvgo@latest`. Every version from v0.13.0 to
v0.55.1, and the pseudo-versions after v0.12.0, are retracted for the flaws
fixed in v0.56.0.

| Version                   | Supported |
|---------------------------|-----------|
| the latest release        | yes       |
| any earlier one           | no        |

## Scope

mkvgo parses files it cannot trust, and reads them from sources it cannot
trust. In scope:

- **Parsing untrusted input**: Matroska, WebM, EBML, MP4 and fragmented MP4,
  subtitles (SRT, ASS, WebVTT, PGS) - any panic, hang, out-of-bounds read, or
  allocation a few bytes of input can size beyond the documented bounds.
- **Remote sources**: `httpfs` and `s3fs` - a request to a host the caller did
  not choose, credentials or signed URLs reaching another host or an error
  message, allocations a server's claimed sizes drive.
- **Serving**: `mkvhttp` and `mkvgo serve` - a path or resource name reaching
  outside what was meant to be served, a request that makes the server read or
  hold far more than it serves.
- **Encryption**: HLS AES-128 and CENC (`cenc`, `cbcs`) packaging - key or IV
  reuse that mkvgo itself causes, key material written where it should not be.

Out of scope: the cost of a legitimately large file within the documented
bounds, the forensic session marking (documented as experimental, robust
neither to collusion nor to re-encoding), and key or IV reuse across separate
packaging runs the caller configured with the same key and base IV.
