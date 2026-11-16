package subtitle

// bitmap.go - what the bitmap subtitle decoders (PGS, VobSub, DVB) hand back: pictures and where they sit on the
// subtitle plane, in one shape whatever the codec.

import "image"

// BitmapDisplaySet is one decoded display set of any bitmap subtitle codec: the type PGSDisplaySet names, which the
// VobSub and DVB decoders return as well.
type BitmapDisplaySet = PGSDisplaySet

// BitmapObject is one picture of a BitmapDisplaySet: the type PGSObject names.
type BitmapObject = PGSObject

// trimTransparent cuts a picture down to the rectangle its visible pixels cover, and says where that rectangle
// starts in the original; nil when no pixel is visible.
func trimTransparent(img *image.NRGBA) (*image.NRGBA, image.Point) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	rowVisible := func(y int) bool {
		row := img.Pix[y*img.Stride : y*img.Stride+4*w]
		for i := 3; i < len(row); i += 4 {
			if row[i] != 0 {
				return true
			}
		}
		return false
	}
	top, bottom := 0, h
	for top < h && !rowVisible(top) {
		top++
	}
	if top == h {
		return nil, image.Point{}
	}
	for !rowVisible(bottom - 1) {
		bottom--
	}
	left, right := w, 0
	for y := top; y < bottom; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+4*w]
		for x := 0; x < left; x++ {
			if row[4*x+3] != 0 {
				left = x
			}
		}
		for x := w - 1; x >= right; x-- {
			if row[4*x+3] != 0 {
				right = x + 1
			}
		}
	}
	if top == 0 && left == 0 && bottom == h && right == w {
		return img, image.Point{}
	}
	out := image.NewNRGBA(image.Rect(0, 0, right-left, bottom-top))
	for y := top; y < bottom; y++ {
		copy(out.Pix[(y-top)*out.Stride:(y-top+1)*out.Stride], img.Pix[y*img.Stride+4*left:])
	}
	return out, image.Pt(left, top)
}
