package termimg

import (
	"image"
	"strconv"
	"strings"
)

// HalfBlocks draws img in at most cols x rows terminal cells using "▀" (the
// upper half block: the text colour paints the top pixel of a cell, the
// background colour the bottom one) with 24-bit colour sequences. A cell is
// taken to be twice as tall as wide, so it holds two square pixels and the
// picture keeps its shape. A small picture is not enlarged.
//
// It returns the lines joined with "\n" (no trailing newline). Every line
// ends with "\x1b[0m" and has the same visible width. A colour sequence is
// written only when the colour differs from the cell before. Transparent
// pixels are composited on black. An empty image gives "".
func HalfBlocks(img image.Image, cols, rows int) string {
	if img == nil || cols <= 0 || rows <= 0 {
		return ""
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return ""
	}
	w, h := Fit(b.Dx(), b.Dy(), cols, 2*rows)
	h += h & 1 // whole cells: two pixels each (2*rows is even, so this still fits)
	px := Scale(img, w, h)

	var sb strings.Builder
	sb.Grow(w * (h / 2) * 24)
	seq := make([]byte, 0, 48)
	for y := 0; y < h; y += 2 {
		if y > 0 {
			sb.WriteByte('\n')
		}
		top := px.Pix[y*px.Stride : y*px.Stride+4*w]
		bot := px.Pix[(y+1)*px.Stride : (y+1)*px.Stride+4*w]
		// Each line starts from the terminal's default colours, which no
		// pixel is assumed to match: -1 is "nothing set yet".
		fg, bg := int32(-1), int32(-1)
		for x := 0; x < 4*w; x += 4 {
			t := int32(top[x])<<16 | int32(top[x+1])<<8 | int32(top[x+2])
			u := int32(bot[x])<<16 | int32(bot[x+1])<<8 | int32(bot[x+2])
			if t != fg || u != bg {
				// One sequence can set both: ESC [ 38;2;r;g;b ; 48;2;r;g;b m
				seq = append(seq[:0], "\x1b["...)
				if t != fg {
					seq = appendRGB(append(seq, "38;2;"...), t)
					fg = t
				}
				if u != bg {
					if len(seq) > 2 {
						seq = append(seq, ';')
					}
					seq = appendRGB(append(seq, "48;2;"...), u)
					bg = u
				}
				seq = append(seq, 'm')
				sb.Write(seq)
			}
			sb.WriteString("▀")
		}
		sb.WriteString("\x1b[0m")
	}
	return sb.String()
}

// appendRGB appends colour c (0xRRGGBB) as "r;g;b".
func appendRGB(dst []byte, c int32) []byte {
	dst = strconv.AppendUint(dst, uint64(c>>16&0xFF), 10)
	dst = append(dst, ';')
	dst = strconv.AppendUint(dst, uint64(c>>8&0xFF), 10)
	dst = append(dst, ';')
	return strconv.AppendUint(dst, uint64(c&0xFF), 10)
}
