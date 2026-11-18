package termimg

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"
)

// sixelPicture is what a terminal would have on screen after a Sixel image,
// plus what the test wants to know about how it got there.
type sixelPicture struct {
	params        string             // what stands between ESC P and q
	width, height int                // from the raster attributes
	aspect        [2]int             // pixel aspect ratio, from the same
	colors        map[int]color.RGBA // the palette entries that were defined
	pix           []color.RGBA
	painted       []int // how many times each pixel was drawn
}

func (p *sixelPicture) at(x, y int) color.RGBA { return p.pix[y*p.width+x] }

// decodeSixel is a small, strict Sixel terminal, written from the DEC
// manual (VT330/VT340 Programmer Reference, chapter 14) rather than from
// the encoder:
//
//	ESC P <params> q    starts the image
//	" Pan;Pad;Ph;Pv     raster attributes: pixel aspect ratio, width, height
//	# Pc;2;R;G;B        defines colour Pc from red, green, blue in 0..100
//	# Pc                selects colour Pc for what follows
//	? to ~              a sixel: (character - 63) is six bits, bit 0 the
//	                    top pixel of a column; set bits are painted in the
//	                    selected colour, then the cursor moves one column on
//	! Pn <sixel>        the sixel repeated Pn times
//	$                   back to the left edge, same band
//	-                   to the left edge of the next band, six rows down
//	ESC \               ends the image
//
// It fails the test on anything else, on drawing outside the declared size,
// and on drawing with a colour that was never defined.
func decodeSixel(t testing.TB, data []byte) *sixelPicture {
	t.Helper()
	p := &sixelPicture{colors: map[int]color.RGBA{}}
	if !bytes.HasPrefix(data, []byte("\x1bP")) {
		t.Fatalf("sixel: starts with %q, want ESC P", data[:min(len(data), 8)])
	}
	q := bytes.IndexByte(data, 'q')
	if q < 0 {
		t.Fatal("sixel: no q after ESC P")
	}
	p.params = string(data[2:q])
	if strings.Trim(p.params, "0123456789;") != "" {
		t.Fatalf("sixel: bad parameters %q", p.params)
	}
	if !bytes.HasSuffix(data, []byte("\x1b\\")) {
		t.Fatalf("sixel: ends with %q, want ESC \\", data[max(len(data)-8, 0):])
	}
	body := data[q+1 : len(data)-2]
	if bytes.IndexByte(body, 0x1b) >= 0 {
		t.Fatal("sixel: an escape character inside the image")
	}

	i := 0
	// numbers reads semicolon-separated numbers at body[i:].
	numbers := func() []int {
		var out []int
		for {
			start, n := i, 0
			for i < len(body) && body[i] >= '0' && body[i] <= '9' {
				n = n*10 + int(body[i]-'0')
				i++
			}
			if i == start {
				t.Fatalf("sixel: expected a number at byte %d (%q)", i, body[max(i-10, 0):min(i+10, len(body))])
			}
			out = append(out, n)
			if i >= len(body) || body[i] != ';' {
				return out
			}
			i++
		}
	}

	x, y, current := 0, 0, -1
	paint := func(bits byte, count int) {
		if bits != 0 {
			c, ok := p.colors[current]
			if !ok {
				t.Fatalf("sixel: drawing with colour %d, which was never defined", current)
			}
			if p.pix == nil {
				t.Fatal("sixel: drawing before the raster attributes")
			}
			for k := 0; k < count; k++ {
				for row := 0; row < 6; row++ {
					if bits&(1<<row) == 0 {
						continue
					}
					if x+k >= p.width || y+row >= p.height {
						t.Fatalf("sixel: drawing at %d,%d outside the declared %dx%d", x+k, y+row, p.width, p.height)
					}
					p.pix[(y+row)*p.width+x+k] = c
					p.painted[(y+row)*p.width+x+k]++
				}
			}
		}
		x += count
	}

	for i < len(body) {
		ch := body[i]
		i++
		switch {
		case ch == '"':
			a := numbers()
			if len(a) != 4 || p.pix != nil {
				t.Fatalf("sixel: raster attributes %v (or given twice)", a)
			}
			p.aspect, p.width, p.height = [2]int{a[0], a[1]}, a[2], a[3]
			p.pix = make([]color.RGBA, p.width*p.height)
			p.painted = make([]int, p.width*p.height)
		case ch == '#':
			a := numbers()
			switch {
			case len(a) == 1:
				current = a[0]
			case len(a) == 5 && a[1] == 2:
				for _, v := range a[2:] {
					if v > 100 {
						t.Fatalf("sixel: colour %v has a part above 100", a)
					}
				}
				if _, dup := p.colors[a[0]]; dup {
					t.Fatalf("sixel: colour %d defined twice", a[0])
				}
				// The usual conversion of terminals, rounding to nearest.
				p.colors[a[0]] = color.RGBA{
					R: uint8((a[2]*255 + 50) / 100),
					G: uint8((a[3]*255 + 50) / 100),
					B: uint8((a[4]*255 + 50) / 100),
					A: 0xFF,
				}
				current = a[0]
			default:
				t.Fatalf("sixel: bad colour introducer %v", a)
			}
		case ch == '!':
			a := numbers()
			if len(a) != 1 || a[0] < 1 || i >= len(body) || body[i] < '?' || body[i] > '~' {
				t.Fatalf("sixel: bad repeat %v at byte %d", a, i)
			}
			paint(body[i]-'?', a[0])
			i++
		case ch == '$':
			x = 0
		case ch == '-':
			x, y = 0, y+6
		case ch >= '?' && ch <= '~':
			paint(ch-'?', 1)
		default:
			t.Fatalf("sixel: unexpected byte %q at %d", ch, i-1)
		}
	}
	if p.pix == nil {
		t.Fatal("sixel: no raster attributes")
	}
	return p
}

// sixel runs WriteSixel and decodes the result, checking everything that
// must hold for any image: the framing, the declared size, a palette within
// bounds, and every pixel painted exactly once.
func sixel(t testing.TB, img image.Image, maxColors int) (*sixelPicture, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteSixel(&buf, img, maxColors); err != nil {
		t.Fatalf("WriteSixel: %v", err)
	}
	p := decodeSixel(t, buf.Bytes())
	if p.params != "0;1;0" {
		t.Errorf("sixel parameters = %q, want 0;1;0", p.params)
	}
	if p.aspect != [2]int{1, 1} {
		t.Errorf("pixel aspect ratio = %v, want 1:1", p.aspect)
	}
	if b := img.Bounds(); p.width != b.Dx() || p.height != b.Dy() {
		t.Fatalf("declared size %dx%d, want %dx%d", p.width, p.height, b.Dx(), b.Dy())
	}
	limit := min(max(maxColors, 2), 256)
	if len(p.colors) > limit || len(p.colors) == 0 {
		t.Errorf("%d colours defined, want 1..%d", len(p.colors), limit)
	}
	for n := range p.colors {
		if n < 0 || n >= limit {
			t.Errorf("colour number %d is outside 0..%d", n, limit-1)
		}
	}
	for i, n := range p.painted {
		if n != 1 {
			t.Fatalf("pixel %d,%d was painted %d times, want once", i%p.width, i/p.width, n)
		}
	}
	return p, buf.Bytes()
}

func TestSixelFlatColours(t *testing.T) {
	colours := []color.RGBA{
		{0, 0, 0, 255}, {255, 255, 255, 255}, {255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255},
		{200, 100, 50, 255}, {127, 127, 127, 255}, {1, 2, 3, 255}, {13, 77, 201, 255}, {254, 253, 252, 255},
	}
	// Heights around the six-row bands, and widths around the shortest
	// repeat worth writing.
	sizes := []image.Point{{1, 1}, {3, 5}, {4, 6}, {5, 7}, {100, 12}, {17, 13}, {64, 64}, {2, 100}}
	for _, c := range colours {
		for _, size := range sizes {
			img := image.NewRGBA(image.Rectangle{Max: size})
			for i := 0; i < len(img.Pix); i += 4 {
				copy(img.Pix[i:], []byte{c.R, c.G, c.B, c.A})
			}
			p, out := sixel(t, img, 256)
			if len(p.colors) != 1 {
				t.Errorf("%v %v: %d colours for a flat image", c, size, len(p.colors))
			}
			// Sixel colours have 101 levels: at worst 1.3 of 255 away.
			for i, got := range p.pix {
				if maxDiff(got, c) > 2 {
					t.Fatalf("%v %v: pixel %d = %v", c, size, i, got)
				}
			}
			// One colour, one run per band: the output must stay tiny.
			bands := (size.Y + 5) / 6
			if len(out) > 60+bands*10 {
				t.Errorf("%v %v: %d bytes of output, repeats are not being used:\n%q", c, size, len(out), out)
			}
		}
	}
}

// gridColour returns one of 216 colours that survive the trip through
// Sixel's 0..100 scale unchanged (multiples of 51 are multiples of 20%).
func gridColour(i int) color.RGBA {
	return color.RGBA{R: uint8(i % 6 * 51), G: uint8(i / 6 % 6 * 51), B: uint8(i / 36 % 6 * 51), A: 255}
}

// TestSixelExact draws pictures whose colours all fit the palette and
// survive Sixel's rounding, so every single pixel must come back exactly:
// any mistake in bands, passes, repeats or bit order shows up here.
func TestSixelExact(t *testing.T) {
	patterns := map[string]func(x, y, w, h int, n *noise) int{
		"random":     func(x, y, w, h int, n *noise) int { return int(n.next() >> 8) },
		"columns":    func(x, y, w, h int, n *noise) int { return x },
		"rows":       func(x, y, w, h int, n *noise) int { return y },
		"diagonal":   func(x, y, w, h int, n *noise) int { return x + y },
		"wide runs":  func(x, y, w, h int, n *noise) int { return x/5 + y/4*3 },
		"checkers":   func(x, y, w, h int, n *noise) int { return (x + y) % 2 },
		"left edge":  func(x, y, w, h int, n *noise) int { return min(x, 1) },
		"right edge": func(x, y, w, h int, n *noise) int { return min(w-1-x, 1) },
		"one dot": func(x, y, w, h int, n *noise) int {
			if x == w/2 && y == h/2 {
				return 1
			}
			return 0
		},
	}
	sizes := []image.Point{{1, 1}, {1, 6}, {6, 1}, {7, 7}, {31, 11}, {40, 12}, {64, 13}, {3, 61}, {130, 18}}
	for name, pattern := range patterns {
		for _, ncolours := range []int{2, 3, 16, 216} {
			for _, size := range sizes {
				n := noise(1 + size.X*31 + size.Y)
				img := image.NewRGBA(image.Rectangle{Max: size})
				for y := 0; y < size.Y; y++ {
					for x := 0; x < size.X; x++ {
						// Spread the few colours over the whole grid, so
						// they are far apart.
						img.SetRGBA(x, y, gridColour(pattern(x, y, size.X, size.Y, &n)%ncolours*(215/(ncolours-1))))
					}
				}
				p, _ := sixel(t, img, max(ncolours, 2))
				for y := 0; y < size.Y; y++ {
					for x := 0; x < size.X; x++ {
						if got, want := p.at(x, y), img.RGBAAt(x, y); got != want {
							t.Fatalf("%s, %d colours, %v: pixel %d,%d = %v, want %v", name, ncolours, size, x, y, got, want)
						}
					}
				}
			}
		}
	}
}

func TestSixelRepeats(t *testing.T) {
	tests := []struct {
		bits  byte
		count int
		want  string
	}{
		{0, 0, ""},
		{0, 1, "?"},
		{63, 2, "~~"},
		{1, 3, "@@@"},
		{1, 4, "!4@"},
		{0, 250, "!250?"},
		{63, 2000, "!2000~"},
	}
	for _, tt := range tests {
		if got := string(appendSixels(nil, tt.bits, tt.count)); got != tt.want {
			t.Errorf("appendSixels(%d, %d) = %q, want %q", tt.bits, tt.count, got, tt.want)
		}
	}

	// A whole image, byte for byte: 10x7 pixels, left half black and right
	// half white, is two bands (six rows, then one). Colour 0 is black.
	img := image.NewRGBA(image.Rect(0, 0, 10, 7))
	for y := 0; y < 7; y++ {
		for x := 0; x < 10; x++ {
			img.SetRGBA(x, y, color.RGBA{A: 255})
			if x >= 5 {
				img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
	}
	p, out := sixel(t, img, 2)
	// "~" is all six rows, "@" only the top one; the white pass first
	// skips the five black columns with a repeated "?" (nothing).
	want := "\x1bP0;1;0q\"1;1;10;7" +
		"#0;2;0;0;0#1;2;100;100;100" +
		"#0!5~$#1!5?!5~" + "-" +
		"#0!5@$#1!5?!5@" +
		"\x1b\\"
	if p.colors[0] != (color.RGBA{A: 255}) {
		// The palette may come in either order.
		want = "\x1bP0;1;0q\"1;1;10;7" +
			"#0;2;100;100;100#1;2;0;0;0" +
			"#0!5?!5~$#1!5~" + "-" +
			"#0!5?!5@$#1!5@" +
			"\x1b\\"
	}
	if string(out) != want {
		t.Errorf("output:\n%q\nwant:\n%q", out, want)
	}
}

// blockMean is the average colour of the size x size block at bx, by.
func blockMean(at func(x, y int) color.RGBA, bx, by, size int) [3]float64 {
	var sum [3]float64
	for y := by * size; y < (by+1)*size; y++ {
		for x := bx * size; x < (bx+1)*size; x++ {
			c := at(x, y)
			sum[0] += float64(c.R)
			sum[1] += float64(c.G)
			sum[2] += float64(c.B)
		}
	}
	n := float64(size * size)
	return [3]float64{sum[0] / n, sum[1] / n, sum[2] / n}
}

// TestSixelPhotos checks pictures with far more colours than the palette
// holds: each pixel must stay near its colour, and (this is what dithering
// is for) the average over a small area must stay much nearer.
//
// With few colours the area test doesn't apply: a palette entry is the
// average of the colours it replaces, so the extremes of the picture lie
// outside anything that mixing palette colours can give. TestDither covers
// that case with palettes that do span the picture.
func TestSixelPhotos(t *testing.T) {
	grey := image.NewRGBA(image.Rect(0, 0, 256, 64))
	for i := 0; i < len(grey.Pix); i += 4 {
		v := uint8(i / 4 % 256)
		copy(grey.Pix[i:], []byte{v, v, v, 255})
	}
	tests := []struct {
		name      string
		img       *image.RGBA
		maxColors int
		pixelTol  float64 // mean error per pixel and channel
		blockTol  float64 // worst error of an 8x8 block's mean, per channel; 0 = don't check
	}{
		{"gradient, 256 colours", gradient(160, 96), 256, 6, 4},
		{"gradient, 64 colours", gradient(160, 96), 64, 12, 0},
		{"gradient, 16 colours", gradient(160, 96), 16, 24, 0},
		{"noisy gradient, 256 colours", photo(160, 91), 256, 9, 8},
		{"noisy gradient, 8 colours", photo(160, 91), 8, 36, 0},
		{"grey ramp, 256 colours", grey, 256, 3, 2},
		{"grey ramp, 2 colours", grey, 2, 50, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := sixel(t, tt.img, tt.maxColors)
			mean, worst := pictureError(p.at, tt.img.RGBAAt, p.width, p.height, 8)
			t.Logf("%d colours: mean pixel error %.2f (limit %.0f), worst 8x8 block error %.2f (limit %.0f)",
				len(p.colors), mean, tt.pixelTol, worst, tt.blockTol)
			if mean > tt.pixelTol {
				t.Errorf("mean pixel error %.1f, want at most %.1f", mean, tt.pixelTol)
			}
			if tt.blockTol > 0 && worst > tt.blockTol {
				t.Errorf("worst 8x8 block error %.1f, want at most %.1f", worst, tt.blockTol)
			}
		})
	}
}

// pictureError compares two w x h pictures: the mean difference per pixel
// and channel, and the largest difference between the mean colours of the
// same block of size x size pixels in each.
func pictureError(got, want func(x, y int) color.RGBA, w, h, size int) (mean, worstBlock float64) {
	diff := func(a, b uint8) float64 { return float64(max(int(a)-int(b), int(b)-int(a))) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g, w := got(x, y), want(x, y)
			mean += diff(g.R, w.R) + diff(g.G, w.G) + diff(g.B, w.B)
		}
	}
	mean /= float64(3 * w * h)
	for by := 0; by < h/size; by++ {
		for bx := 0; bx < w/size; bx++ {
			g, w := blockMean(got, bx, by, size), blockMean(want, bx, by, size)
			for c := range g {
				worstBlock = max(worstBlock, g[c]-w[c], w[c]-g[c])
			}
		}
	}
	return mean, worstBlock
}

// TestDither drives the ditherer with palettes that span the whole picture
// (black and white for a grey ramp; the eight corners of the colour cube
// for a colour gradient). Every colour is then some mix of palette colours,
// so the dithered picture, seen from a distance (here: averaged over 16x16
// pixels), must match the original closely although no single pixel does.
func TestDither(t *testing.T) {
	grey := image.NewRGBA(image.Rect(0, 0, 256, 64))
	for i := 0; i < len(grey.Pix); i += 4 {
		v := uint8(i / 4 % 256)
		copy(grey.Pix[i:], []byte{v, v, v, 255})
	}
	blackWhite := &palette{n: 2}
	blackWhite.rgb[1] = [3]int32{255, 255, 255}
	corners := &palette{n: 8}
	for i := 0; i < 8; i++ {
		corners.rgb[i] = [3]int32{int32(i & 1 * 255), int32(i >> 1 & 1 * 255), int32(i >> 2 * 255)}
	}
	tests := []struct {
		name     string
		img      *image.RGBA
		pal      *palette
		blockTol float64
	}{
		{"grey ramp in black and white", grey, blackWhite, 8},
		{"gradient in eight colours", gradient(160, 96), corners, 8},
		{"noisy gradient in eight colours", photo(160, 96), corners, 8},
	}
	for _, tt := range tests {
		w, h := tt.img.Rect.Dx(), tt.img.Rect.Dy()
		d := newDitherer(tt.pal, w, make([]uint32, 1<<18))
		out := make([]byte, w*h)
		for y := 0; y < h; y++ {
			d.row(tt.img.Pix[y*tt.img.Stride:], out[y*w:(y+1)*w], y)
		}
		got := func(x, y int) color.RGBA {
			c := tt.pal.rgb[out[y*w+x]]
			return color.RGBA{uint8(c[0]), uint8(c[1]), uint8(c[2]), 255}
		}
		mean, worst := pictureError(got, tt.img.RGBAAt, w, h, 16)
		t.Logf("%s: mean pixel error %.1f, worst 16x16 block error %.2f (limit %.0f)", tt.name, mean, worst, tt.blockTol)
		if worst > tt.blockTol {
			t.Errorf("%s: worst 16x16 block error %.1f, want at most %.1f", tt.name, worst, tt.blockTol)
		}
		// The further away, the closer: what is left is the error still
		// on its way across a block's edge. (Without diffusion these
		// numbers are in the tens.)
		if _, far := pictureError(got, tt.img.RGBAAt, w/32*32, h/32*32, 32); far > 3 {
			t.Errorf("%s: worst 32x32 block error %.2f, want at most 3", tt.name, far)
		}
		// And it really is dithering: single pixels are far off.
		if mean < 20 {
			t.Errorf("%s: mean pixel error %.1f is implausibly small for so few colours", tt.name, mean)
		}
	}
}

func TestSixelPaletteLimit(t *testing.T) {
	img := photo(120, 50) // thousands of colours
	for _, tt := range []struct{ maxColors, wantMin, wantMax int }{
		{-5, 2, 2}, {0, 2, 2}, {1, 2, 2}, {2, 2, 2}, {3, 3, 3}, {16, 16, 16}, {64, 60, 64},
		{255, 230, 255}, {256, 230, 256}, {257, 230, 256}, {100000, 230, 256},
	} {
		p, _ := sixel(t, img, tt.maxColors) // sixel checks the upper bound
		if n := len(p.colors); n < tt.wantMin || n > tt.wantMax {
			t.Errorf("maxColors %d: %d colours, want %d..%d", tt.maxColors, n, tt.wantMin, tt.wantMax)
		}
	}
}

func TestSixelTransparencyAndSources(t *testing.T) {
	// Transparent pixels come out black, half-transparent ones darker.
	n := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 12; x++ {
			n.SetNRGBA(x, y, color.NRGBA{255, 255, 255, uint8(x / 4 * 255 / 2)}) // alpha 0, 127, 255
		}
	}
	p, _ := sixel(t, n, 16)
	for x, want := range map[int]uint8{1: 0, 5: 127, 10: 255} {
		if got := p.at(x, 6); maxDiff(got, color.RGBA{want, want, want, 255}) > 2 {
			t.Errorf("alpha column %d = %v, want grey %d", x, got, want)
		}
	}

	// Any kind of image gives the same bytes as its plain RGBA copy.
	src := gradient(40, 27)
	_, want := sixel(t, src, 64)
	nrgba := image.NewNRGBA(src.Rect)
	copy(nrgba.Pix, src.Pix) // opaque, so the bytes mean the same
	sub := image.NewRGBA(image.Rect(0, 0, 60, 50))
	for y := 0; y < 27; y++ {
		copy(sub.Pix[sub.PixOffset(7, 9+y):], src.Pix[y*src.Stride:(y+1)*src.Stride])
	}
	for name, img := range map[string]image.Image{
		"NRGBA":     nrgba,
		"sub-image": sub.SubImage(image.Rect(7, 9, 47, 36)),
		"At only":   atOnly{src},
		"oriented":  Orient(orientRGBA(Scale(src, 40, 27), 3), 3), // turned twice: itself
	} {
		if _, got := sixel(t, img, 64); !bytes.Equal(got, want) {
			t.Errorf("%s: output differs from the RGBA image's", name)
		}
	}

	// A JPEG-style image is converted on the fly.
	y := photoYCbCr(64, 48, image.YCbCrSubsampleRatio420)
	p, _ = sixel(t, y, 256)
	var total float64
	for py := 0; py < 48; py++ {
		for px := 0; px < 64; px++ {
			total += float64(maxDiff(p.at(px, py), rgbaAt(y, px, py)))
		}
	}
	if mean := total / (64 * 48); mean > 12 {
		t.Errorf("YCbCr source: mean pixel error %.1f", mean)
	}
}

func TestSixelErrors(t *testing.T) {
	for _, img := range []image.Image{nil, image.NewRGBA(image.Rect(0, 0, 0, 5)), image.NewRGBA(image.Rect(3, 3, 3, 3))} {
		var buf bytes.Buffer
		if err := WriteSixel(&buf, img, 256); err == nil || buf.Len() != 0 {
			t.Errorf("WriteSixel(%v) = %v with %d bytes written, want an error and nothing", img, err, buf.Len())
		}
	}
	// A writer that fails is reported, early or late.
	for _, after := range []int{0, 1, 3} {
		w := &failingWriter{after: after}
		if err := WriteSixel(w, photo(800, 600), 256); !errors.Is(err, errWriteFailed) {
			t.Errorf("writer failing after %d writes: err = %v", after, err)
		}
	}
}

var errWriteFailed = errors.New("write failed")

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errWriteFailed
	}
	w.after--
	return len(p), nil
}

// chunkCounter records how the output arrived.
type chunkCounter struct {
	writes, bytes, largest int
}

func (c *chunkCounter) Write(p []byte) (int, error) {
	c.writes++
	c.bytes += len(p)
	c.largest = max(c.largest, len(p))
	return len(p), nil
}

// TestSixelStreams checks that the image is handed to the writer piece by
// piece, and that the memory to produce it doesn't grow with its height:
// a picture four times as tall is four times the output for the same
// allocations.
func TestSixelStreams(t *testing.T) {
	var size, spent [2]uint64
	for i, height := range []int{300, 1200} {
		img := photo(800, height)
		var out chunkCounter
		if err := WriteSixel(&out, img, 256); err != nil {
			t.Fatal(err)
		}
		if out.largest > 64<<10 || out.writes < out.bytes/(64<<10) {
			t.Errorf("800x%d: %d bytes arrived in %d writes, the largest %d: want pieces of at most 64 kB",
				height, out.bytes, out.writes, out.largest)
		}
		size[i] = uint64(out.bytes)
		spent[i] = allocatedBytes(func() { WriteSixel(&out, img, 256) })
		t.Logf("800x%d: %d bytes of output in %d writes, %d bytes allocated", height, size[i], out.writes/2, spent[i])
	}
	if size[1] < 3*size[0] {
		t.Errorf("output sizes %v: the taller picture should be about four times the bytes", size)
	}
	// The same but for the list of the picture's colours, which the
	// taller one has more of (8 bytes each).
	if spent[1] > spent[0]+512<<10 {
		t.Errorf("allocations %v grow with the height of the picture", spent)
	}
	// A sanity cap on the fixed cost (colour table, write buffer, one
	// band's worth of working rows, the colour list): a few megabytes,
	// whatever the picture's size.
	if limit := uint64(4 << 20); spent[0] > limit {
		t.Errorf("WriteSixel allocated %d bytes for an 800x300 picture, want at most %d", spent[0], limit)
	}
}
