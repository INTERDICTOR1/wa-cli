package termimg

import (
	"image"
	"image/color"
)

// rgbaRows returns a function that gives row y of img (0 is the top row of
// its bounds) as 8-bit alpha-premultiplied RGBA, four bytes per pixel. The
// slice is only good until the next call, and must not be written to: for an
// *image.RGBA it is the image's own memory, for everything else a one-row
// buffer the pixels are converted into. This is how the package reads any
// kind of image without ever making a full-size RGBA copy of it.
func rgbaRows(img image.Image) func(y int) []byte {
	b := img.Bounds()
	w := b.Dx()
	if s, ok := img.(*image.RGBA); ok {
		return func(y int) []byte {
			o := s.PixOffset(b.Min.X, b.Min.Y+y)
			return s.Pix[o : o+4*w : o+4*w]
		}
	}

	buf := make([]byte, 4*w)
	switch s := img.(type) {
	case *image.NRGBA:
		return func(y int) []byte {
			o := s.PixOffset(b.Min.X, b.Min.Y+y)
			src := s.Pix[o : o+4*w]
			for i := 0; i < len(buf); i += 4 {
				p, d := src[i:i+4:i+4], buf[i:i+4:i+4]
				switch a := uint32(p[3]); a {
				case 0xFF:
					d[0], d[1], d[2], d[3] = p[0], p[1], p[2], 0xFF
				case 0:
					d[0], d[1], d[2], d[3] = 0, 0, 0, 0
				default:
					// Multiply by alpha exactly as color.NRGBA.RGBA
					// does (in 16 bits, then keep the top 8), so every
					// way of reading a pixel gives the same number.
					a *= 0x101
					d[0] = uint8(uint32(p[0]) * 0x101 * a / 0xFFFF >> 8)
					d[1] = uint8(uint32(p[1]) * 0x101 * a / 0xFFFF >> 8)
					d[2] = uint8(uint32(p[2]) * 0x101 * a / 0xFFFF >> 8)
					d[3] = p[3]
				}
			}
			return buf
		}

	case *image.Paletted:
		// Colours beyond the palette (a corrupt file) stay transparent.
		var pal [256][4]byte
		for i, c := range s.Palette {
			if i < len(pal) {
				r, g, bl, a := c.RGBA()
				pal[i] = [4]byte{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)}
			}
		}
		return func(y int) []byte {
			o := s.PixOffset(b.Min.X, b.Min.Y+y)
			for x, c := range s.Pix[o : o+w] {
				copy(buf[4*x:4*x+4], pal[c][:])
			}
			return buf
		}

	case *image.Gray:
		return func(y int) []byte {
			o := s.PixOffset(b.Min.X, b.Min.Y+y)
			for x, g := range s.Pix[o : o+w] {
				d := buf[4*x : 4*x+4 : 4*x+4]
				d[0], d[1], d[2], d[3] = g, g, g, 0xFF
			}
			return buf
		}

	case *image.YCbCr:
		hs, _ := chromaStep(s.SubsampleRatio)
		if hs == 0 || b.Min.X < 0 || b.Min.Y < 0 {
			break
		}
		shift := uint(hs / 2) // hs is 1, 2 or 4: shift by 0, 1 or 2
		first := b.Min.X >> shift
		return func(y int) []byte {
			yo := s.YOffset(b.Min.X, b.Min.Y+y)
			co := s.COffset(b.Min.X, b.Min.Y+y)
			for x, yy := range s.Y[yo : yo+w] {
				c := co + (b.Min.X+x)>>shift - first
				r, g, bl := color.YCbCrToRGB(yy, s.Cb[c], s.Cr[c])
				d := buf[4*x : 4*x+4 : 4*x+4]
				d[0], d[1], d[2], d[3] = r, g, bl, 0xFF
			}
			return buf
		}
	}

	// Anything else, pixel by pixel. Every image type of the standard
	// library can hand out a pixel as plain numbers (RGBA64At); a foreign
	// type may only have At, which boxes each pixel in an interface.
	if s, ok := img.(image.RGBA64Image); ok {
		return func(y int) []byte {
			for x := 0; x < w; x++ {
				c := s.RGBA64At(b.Min.X+x, b.Min.Y+y)
				d := buf[4*x : 4*x+4 : 4*x+4]
				d[0], d[1], d[2], d[3] = uint8(c.R>>8), uint8(c.G>>8), uint8(c.B>>8), uint8(c.A>>8)
			}
			return buf
		}
	}
	return func(y int) []byte {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			d := buf[4*x : 4*x+4 : 4*x+4]
			d[0], d[1], d[2], d[3] = uint8(r>>8), uint8(g>>8), uint8(bl>>8), uint8(a>>8)
		}
		return buf
	}
}

// isOpaque reports whether img is known to have no transparent pixels.
func isOpaque(img image.Image) bool {
	switch s := img.(type) {
	case *oriented:
		return isOpaque(s.src)
	case interface{ Opaque() bool }:
		return s.Opaque()
	}
	return false
}
