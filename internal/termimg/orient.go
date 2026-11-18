package termimg

import (
	"image"
	"image/color"
)

// Orient returns img turned upright for EXIF orientation o (1..8). o==1 (or
// anything outside 2..8) returns img itself.
//
// The result is a view of img, not a copy, so orienting a freshly decoded
// 12-megapixel photo costs nothing. Scale knows the view and stays fast;
// other code reads it pixel by pixel, which is fine for the small pictures
// that Scale returns but slow for big ones. So: Orient first, then Scale.
func Orient(img image.Image, o int) image.Image {
	if img == nil || o < 2 || o > 8 {
		return img
	}
	return &oriented{src: img, o: o, b: img.Bounds()}
}

// oriented is src seen through an EXIF orientation.
//
// The orientation says where the stored picture's first row and first
// column end up when it is viewed correctly:
//
//	1: top, left        5: left, top       (5..8 swap width and height)
//	2: top, right       6: right, top
//	3: bottom, right    7: right, bottom
//	4: bottom, left     8: left, bottom
type oriented struct {
	src image.Image
	o   int
	b   image.Rectangle // src.Bounds()
}

func (v *oriented) ColorModel() color.Model { return v.src.ColorModel() }

func (v *oriented) Bounds() image.Rectangle {
	w, h := v.b.Dx(), v.b.Dy()
	if v.o >= 5 {
		w, h = h, w
	}
	return image.Rect(0, 0, w, h)
}

func (v *oriented) At(x, y int) color.Color {
	sx, sy := storedXY(v.o, x, y, v.b.Dx(), v.b.Dy())
	return v.src.At(v.b.Min.X+sx, v.b.Min.Y+sy)
}

// RGBA64At lets code that reads pixel by pixel skip the color.Color
// interface, when src allows it too.
func (v *oriented) RGBA64At(x, y int) color.RGBA64 {
	sx, sy := storedXY(v.o, x, y, v.b.Dx(), v.b.Dy())
	sx, sy = v.b.Min.X+sx, v.b.Min.Y+sy
	if s, ok := v.src.(image.RGBA64Image); ok {
		return s.RGBA64At(sx, sy)
	}
	r, g, b, a := v.src.At(sx, sy).RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
}

// storedXY maps a pixel of the upright picture to where it is stored, in a
// stored picture of w x h pixels with orientation o.
func storedXY(o, x, y, w, h int) (int, int) {
	switch o {
	case 2: // mirrored left-right
		return w - 1 - x, y
	case 3: // upside down
		return w - 1 - x, h - 1 - y
	case 4: // mirrored top-bottom
		return x, h - 1 - y
	case 5: // flipped over the top-left to bottom-right diagonal
		return y, x
	case 6: // needs a quarter turn clockwise
		return y, h - 1 - x
	case 7: // flipped over the other diagonal
		return w - 1 - y, h - 1 - x
	case 8: // needs a quarter turn anticlockwise
		return w - 1 - y, x
	}
	return x, y
}

// orientRGBA turns img upright for orientation o. img must be one of ours:
// origin at (0,0) and no gaps between rows. Mirroring and the half turn
// happen in place; the others need a second buffer because width and height
// swap.
func orientRGBA(img *image.RGBA, o int) *image.RGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	pix := img.Pix
	swap := func(i, j int) { // pixels i and j, counted from the top left
		a, b := pix[4*i:4*i+4:4*i+4], pix[4*j:4*j+4:4*j+4]
		a[0], a[1], a[2], a[3], b[0], b[1], b[2], b[3] = b[0], b[1], b[2], b[3], a[0], a[1], a[2], a[3]
	}
	switch o {
	case 2:
		for y := 0; y < h; y++ {
			for i, j := y*w, y*w+w-1; i < j; i, j = i+1, j-1 {
				swap(i, j)
			}
		}
	case 3:
		for i, j := 0, w*h-1; i < j; i, j = i+1, j-1 {
			swap(i, j)
		}
	case 4:
		for top, bot := 0, h-1; top < bot; top, bot = top+1, bot-1 {
			for x := 0; x < w; x++ {
				swap(top*w+x, bot*w+x)
			}
		}
	case 5, 6, 7, 8:
		out := image.NewRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < w; y++ {
			row := out.Pix[y*out.Stride : y*out.Stride+4*h]
			for x := 0; x < h; x++ {
				sx, sy := storedXY(o, x, y, w, h)
				copy(row[4*x:4*x+4], pix[4*(sy*w+sx):4*(sy*w+sx)+4])
			}
		}
		return out
	}
	return img
}
