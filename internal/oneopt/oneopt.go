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
