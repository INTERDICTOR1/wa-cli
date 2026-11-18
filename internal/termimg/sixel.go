package termimg

import (
	"bufio"
	"errors"
	"image"
	"io"
	"math"
	"strconv"
)

var popPower = 1.0
var expReserveDiv = 0
var expMaximinPow = 0.5

var errEmptyImage = errors.New("termimg: the image has no pixels")

// WriteSixel writes img as a Sixel image, the pixel graphics protocol of DEC
// terminals that Windows Terminal (1.22 and later), WezTerm, foot, mlterm
// and xterm understand. img is drawn at its own size, one image pixel per
// terminal pixel, so scale it first. Transparent pixels are composited on
// black.
//
// Sixel can only show a limited set of colours at once, so the picture is
// reduced to a palette of at most maxColors (2..256, clamped) chosen by
// median cut, and dithered (Floyd–Steinberg) so that the eye blends
// neighbouring pixels back into the colours that were lost.
//
// The output is, in order:
//
//	ESC P 0;1;0 q             start of a Sixel image
//	" 1;1;<width>;<height>    square pixels, and the size
//	#<n>;2;<r>;<g>;<b>        palette entry n, each part 0..100
//	the picture in bands of six pixel rows, top to bottom, separated by "-".
//	A band is drawn once per colour it uses, the passes separated by "$"
//	(back to the left edge): #<n>, then one character per column: 63 plus
//	one bit per row that has colour n there (the top row is bit 0).
//	!<count><character> repeats a character.
//	ESC \                     end
//
// It is produced band by band through a buffered writer, never built in
// memory whole. WriteSixel allocates a 1 MB table, some 300 bytes per pixel
// of width, and while the palette is chosen 8 bytes per distinct colour
// (a few hundred kB for a photo, 2 MB at the very most), whatever the
// height.
func WriteSixel(w io.Writer, img image.Image, maxColors int) error {
	if img == nil {
		return errEmptyImage
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= 0 || height <= 0 {
		return errEmptyImage
	}
	maxColors = min(max(maxColors, 2), 256)

	rows := rgbaRows(img)
	pal, table := medianCut(rows, width, height, maxColors)

	bw := bufio.NewWriterSize(w, 64<<10)
	line := make([]byte, 0, width+16) // the header, then one pass of one band at a time
	line = append(line, "\x1bP0;1;0q\"1;1;"...)
	line = strconv.AppendInt(line, int64(width), 10)
	line = append(line, ';')
	line = strconv.AppendInt(line, int64(height), 10)
	for i := 0; i < pal.n; i++ {
		line = append(line, '#')
		line = strconv.AppendInt(line, int64(i), 10)
		line = append(line, ";2"...)
		for _, v := range pal.percent[i] {
			line = append(line, ';')
			line = strconv.AppendInt(line, int64(v), 10)
		}
	}
	if _, err := bw.Write(line); err != nil {
		return err
	}

	const band = 6 // pixel rows per sixel
	d := newDitherer(pal, width, table)
	index := make([]byte, band*width) // the palette entry of each pixel of the band
	// cells[c*width+x] is the sixel (six bits, one per row) that colour c
	// has at column x of the band. Only the columns from first[c] to
	// last[c] are in use, so only those need writing and wiping.
	cells := make([]byte, pal.n*width)
	var first, last [256]int32
	for c := range first {
		first[c], last[c] = int32(width), -1
	}

	for top := 0; top < height; top += band {
		n := min(band, height-top) // the last band may be short
		for i := 0; i < n; i++ {
			d.row(rows(top+i), index[i*width:(i+1)*width], top+i)
		}
		for i := 0; i < n; i++ {
			bit := byte(1) << i
			for x, c := range index[i*width : (i+1)*width] {
				cells[int(c)*width+x] |= bit
				if int32(x) < first[c] {
					first[c] = int32(x)
				}
				if int32(x) > last[c] {
					last[c] = int32(x)
				}
			}
		}

		passes := 0
		for c := 0; c < pal.n; c++ {
			if last[c] < 0 {
				continue // colour not used in this band
			}
			line = line[:0]
			if passes > 0 {
				line = append(line, '$')
			}
			passes++
			line = append(line, '#')
			line = strconv.AppendInt(line, int64(c), 10)
			lo, hi := int(first[c]), int(last[c])+1
			line = appendSixels(line, 0, lo) // nothing up to the first pixel
			used := cells[c*width+lo : c*width+hi]
			for x := 0; x < len(used); {
				v, run := used[x], 1
				for x+run < len(used) && used[x+run] == v {
					run++
				}
				line = appendSixels(line, v, run)
				x += run
			}
			// Nothing after the last pixel either: the rest of the band
			// is simply left alone.
			clear(used)
			first[c], last[c] = int32(width), -1
			if _, err := bw.Write(line); err != nil {
				return err
			}
		}
		if top+band < height {
			if err := bw.WriteByte('-'); err != nil {
				return err
			}
		}
	}
	if _, err := bw.WriteString("\x1b\\"); err != nil {
		return err
	}
	return bw.Flush()
}

// appendSixels appends count copies of the sixel with the given bits, as a
// repeat (!<count><character>) when that is shorter.
func appendSixels(dst []byte, bits byte, count int) []byte {
	ch := '?' + bits
	if count > 3 {
		dst = append(dst, '!')
		dst = strconv.AppendInt(dst, int64(count), 10)
		return append(dst, ch)
	}
	for ; count > 0; count-- {
		dst = append(dst, ch)
	}
	return dst
}

// palette is the set of colours a Sixel image is drawn with.
type palette struct {
	n       int
	percent [256][3]uint8 // red, green, blue the way Sixel takes them: 0..100
	rgb     [256][3]int32 // the same colours as the terminal shows them: 0..255
}

// How much a difference in red, green and blue counts when comparing
// colours. The eye is most sensitive to green and least to blue.
var channelWeight = [3]int32{2, 4, 1}

// closest returns the palette entry nearest to a colour.
func (p *palette) closest(r, g, b int32) int {
	best, bestDist := 0, int32(1<<31-1)
	for i := 0; i < p.n; i++ {
		c := &p.rgb[i]
		dr, dg, db := r-c[0], g-c[1], b-c[2]
		if d := channelWeight[0]*dr*dr + channelWeight[1]*dg*dg + channelWeight[2]*db*db; d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

// cube numbers the 4x4x4 cube of colours that a colour falls in: the top six
// bits of red, green and blue. A quarter of a million cubes are fine enough
// (Sixel itself has only 101 levels per channel) and few enough to keep one
// number for each.
func cube(r, g, b byte) int { return int(r>>2)<<12 | int(g>>2)<<6 | int(b>>2) }

// colorBin is a cube that occurs in the picture, and in how many pixels.
type colorBin struct {
	cube uint32
	n    uint32
}

// channel returns the bin's red (0), green (1) or blue (2), 0..63.
func (b colorBin) channel(axis int) int { return int(b.cube>>(12-6*axis)) & 63 }

// colorBox is a group of bins (bins[lo:hi]) that will share one palette
// entry unless it gets split.
type colorBox struct {
	lo, hi int
	axis   int     // the channel along which its colours differ most
	cut    float64 // their mean on that channel: where to split
	spread float64 // how much they differ, and so what a split gains; 0 = one colour
}

// medianCut picks at most maxColors colours to draw an image with.
//
// It counts how often each cube of colours occurs. Then, starting with all
// of them in one box, it keeps splitting the box whose colours differ most,
// across the channel where they differ most, at their mean, until there are
// enough boxes. Each box becomes one palette entry: the exact average of the
// pixels in it, found in a second pass.
//
// The megabyte table it counts in is returned, zeroed, for the ditherer to
// use next, so that only one is ever allocated.
func medianCut(rows func(y int) []byte, width, height, maxColors int) (*palette, []uint32) {
	table := make([]uint32, 1<<18)
	for y := 0; y < height; y++ {
		row := rows(y)[:4*width]
		for ; len(row) >= 4; row = row[4:] {
			table[cube(row[0], row[1], row[2])]++
		}
	}
	used := 0
	for _, n := range table {
		if n > 0 {
			used++
		}
	}
	bins := make([]colorBin, 0, used)
	for c, n := range table {
		if n > 0 {
			bins = append(bins, colorBin{cube: uint32(c), n: n})
		}
	}

	measure := func(lo, hi int) colorBox {
		box := colorBox{lo: lo, hi: hi}
		for axis := 0; axis < 3; axis++ {
			var n, sum float64
			for _, b := range bins[lo:hi] {
				n += math.Pow(float64(b.n), popPower)
				sum += math.Pow(float64(b.n), popPower) * float64(b.channel(axis))
			}
			mean := sum / n
			var spread float64
			for _, b := range bins[lo:hi] {
				d := float64(b.channel(axis)) - mean
				spread += math.Pow(float64(b.n), popPower) * d * d
			}
			if spread *= float64(channelWeight[axis]); spread > box.spread {
				box.axis, box.cut, box.spread = axis, mean, spread
			}
		}
		return box
	}

	reserve := 0
	if expReserveDiv > 0 {
		reserve = maxColors / expReserveDiv
	}
	boxes := make([]colorBox, 1, maxColors)
	boxes[0] = measure(0, len(bins))
	for len(boxes) < maxColors-reserve {
		worst := 0
		for i := range boxes {
			if boxes[i].spread > boxes[worst].spread {
				worst = i
			}
		}
		box := boxes[worst]
		if box.spread <= 0 {
			break // every box is a single cube already
		}
		// Move the bins below the cut to the front.
		i, j := box.lo, box.hi-1
		for i <= j {
			if float64(bins[i].channel(box.axis)) < box.cut {
				i++
			} else {
				bins[i], bins[j] = bins[j], bins[i]
				j--
			}
		}
		if i == box.lo || i == box.hi {
			boxes[worst].spread = 0 // rounding left nothing to split off
			continue
		}
		boxes[worst] = measure(box.lo, i)
		boxes = append(boxes, measure(i, box.hi))
	}

	// EXPERIMENT: maximin
	owner := make([]uint8, len(bins))
	for i, box := range boxes {
		for j := box.lo; j < box.hi; j++ {
			owner[j] = uint8(i)
		}
	}
	entries := len(boxes)
	dist := make([]float64, len(bins))
	for entries < maxColors && reserve > 0 {
		var cn [256]float64
		var cs [256][3]float64
		for i, b := range bins {
			o := owner[i]
			w := float64(b.n)
			cn[o] += w
			for a := 0; a < 3; a++ {
				cs[o][a] += w * float64(b.channel(a))
			}
		}
		best, bestScore := -1, 0.0
		for i, b := range bins {
			o := owner[i]
			var d float64
			for a := 0; a < 3; a++ {
				x := float64(b.channel(a)) - cs[o][a]/cn[o]
				d += float64(channelWeight[a]) * x * x
			}
			dist[i] = d
			if sc := d * math.Pow(float64(b.n), expMaximinPow); sc > bestScore {
				best, bestScore = i, sc
			}
		}
		if best < 0 {
			break
		}
		nb := bins[best]
		for i, b := range bins {
			var d float64
			for a := 0; a < 3; a++ {
				x := float64(b.channel(a) - nb.channel(a))
				d += float64(channelWeight[a]) * x * x
			}
			if d < dist[i] {
				owner[i] = uint8(entries)
			}
		}
		entries++
	}
	for i, b := range bins {
		table[b.cube] = uint32(owner[i])
	}
	var count [256]uint64
	var sum [256][3]uint64
	for y := 0; y < height; y++ {
		row := rows(y)[:4*width]
		for ; len(row) >= 4; row = row[4:] {
			i := table[cube(row[0], row[1], row[2])] & 0xFF
			count[i]++
			sum[i][0] += uint64(row[0])
			sum[i][1] += uint64(row[1])
			sum[i][2] += uint64(row[2])
		}
	}
	clear(table)

	// Sixel colours have 101 levels per channel, so two boxes can round to
	// the same colour; keep one of each.
	pal := &palette{}
	seen := make(map[[3]uint8]bool, len(boxes))
	for i := 0; i < entries; i++ {
		if count[i] == 0 {
			continue
		}
		var pc [3]uint8
		for c := range pc {
			v := (sum[i][c] + count[i]/2) / count[i] // the average, 0..255
			pc[c] = uint8((v*100 + 127) / 255)       // in percent
		}
		if seen[pc] {
			continue
		}
		seen[pc] = true
		pal.percent[pal.n] = pc
		for c := range pc {
			pal.rgb[pal.n][c] = (int32(pc[c])*255 + 50) / 100
		}
		pal.n++
	}
	return pal, table
}

// ditherer turns rows of pixels into rows of palette entries, spreading the
// error of each choice over the neighbours that are still to come
// (Floyd–Steinberg: 7/16 to the next pixel, 3/16, 5/16 and 1/16 to the three
// below). Rows are walked alternately left to right and right to left, which
// keeps the error from drifting to one side in visible streaks.
type ditherer struct {
	pal   *palette
	width int
	// The error owed to the pixels of this row and the next, in sixteenths:
	// three numbers per pixel, with one spare pixel at each end so the
	// edges need no special case.
	cur, next []int32
	// nearest[cube] is the palette entry closest to that cube of colours,
	// plus one, or 0 when it hasn't been worked out yet. Searching the
	// palette for every pixel would be far too slow.
	nearest []uint32
}

// newDitherer needs a zeroed table with a number per cube (see medianCut).
func newDitherer(pal *palette, width int, table []uint32) *ditherer {
	return &ditherer{
		pal:     pal,
		width:   width,
		cur:     make([]int32, 3*(width+2)),
		next:    make([]int32, 3*(width+2)),
		nearest: table,
	}
}

// row dithers row y, given as RGBA in src, into palette entries in out.
func (d *ditherer) row(src, out []byte, y int) {
	cur, next, pal := d.cur, d.next, d.pal
	x, end, step := 0, d.width, 1
	if y&1 == 1 {
		x, end, step = d.width-1, -1, -1
	}
	for ; x != end; x += step {
		e := 3 * (x + 1)
		r := clamp8(int32(src[4*x]) + (cur[e]+8)>>4)
		g := clamp8(int32(src[4*x+1]) + (cur[e+1]+8)>>4)
		b := clamp8(int32(src[4*x+2]) + (cur[e+2]+8)>>4)

		key := r>>2<<12 | g>>2<<6 | b>>2 // as cube(), r, g and b being 0..255
		c := d.nearest[key]
		if c == 0 {
			// Look up the middle of the cube, so the answer doesn't
			// depend on which of its colours came first.
			c = uint32(pal.closest(r&^3|2, g&^3|2, b&^3|2)) + 1
			d.nearest[key] = c
		}
		c--
		out[x] = byte(c)

		p := &pal.rgb[c]
		er, eg, eb := r-p[0], g-p[1], b-p[2]
		ahead, behind := e+3*step, e-3*step
		cur[ahead] += 7 * er
		cur[ahead+1] += 7 * eg
		cur[ahead+2] += 7 * eb
		next[behind] += 3 * er
		next[behind+1] += 3 * eg
		next[behind+2] += 3 * eb
		next[e] += 5 * er
		next[e+1] += 5 * eg
		next[e+2] += 5 * eb
		next[ahead] += er
		next[ahead+1] += eg
		next[ahead+2] += eb
	}
	clear(cur)
	d.cur, d.next = next, cur
}

func clamp8(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}
