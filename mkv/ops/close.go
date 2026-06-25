package ops

import "io"

// closeWithErr closes c and, on the success path, surfaces a Close error through
// *err — e.g. a custom FS (S3/network) that finalises or commits the write on
// Close, where dropping that error would silently lose data. It is the defer
// companion to an output op with a named error return: `defer closeWithErr(out, &err)`.
func closeWithErr(c io.Closer, err *error) {
	if cerr := c.Close(); cerr != nil && *err == nil {
		*err = cerr
	}
}
