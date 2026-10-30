// Package oneopt holds the rule of every exported function taking variadic options: one value at most, since only
// the first is read.
package oneopt

import "github.com/gravity-zero/mkvgo/mkv"

// Check refuses more than one options value (mkv.ErrTooManyOptions).
func Check[T any](opts []T) error {
	if len(opts) > 1 {
		return mkv.ErrTooManyOptions
	}
	return nil
}

// Must is Check for a function that returns no error: more than one value is a programming error, it panics.
func Must[T any](opts []T) {
	if err := Check(opts); err != nil {
		panic(err)
	}
}
