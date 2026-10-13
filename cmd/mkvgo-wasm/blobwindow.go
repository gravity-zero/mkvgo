package main

const (
	blobWindowMin = 64 << 10
	blobWindowMax = 512 << 10
)

// nextBlobWindow sizes the Blob read at off: it doubles while the reads run on from the previous one
// (prevEnd), up to blobWindowMax, and starts at blobWindowMin after a jump.
func nextBlobWindow(prevEnd, off int64, prev int) int {
	if prev > 0 && off == prevEnd {
		return min(2*prev, blobWindowMax)
	}
	return blobWindowMin
}
