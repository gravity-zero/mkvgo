//go:build !mkvgo_poison

package poison

// On reports whether recycled buffers are poisoned (the mkvgo_poison build tag).
const On = false
