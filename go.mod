module github.com/gravity-zero/mkvgo

go 1.27

toolchain go1.27.0

// Security fixes in v0.55.x: CENC "cenc" IV reuse (re-package and rotate keys), S3 credential leak, remote OOM. Upgrade.
retract [v0.13.0, v0.54.0]
