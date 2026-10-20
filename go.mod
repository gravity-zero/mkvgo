module github.com/gravity-zero/mkvgo

go 1.24.0

// Security fixes in v0.56.0: CENC "cenc" IV reuse (re-package and rotate keys), S3 credential leak, remote OOM. Upgrade.
retract [v0.12.1-0, v0.55.1]
