package termimg

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"testing"
)

func TestFit(t *testing.T) {
	tests := []struct {
		w, h, maxW, maxH int
		wantW, wantH     int
	}{
		{100, 50, 200, 200, 100, 50},   // never enlarged
		{200, 100, 200, 100, 200, 100}, // exactly fits
		{100, 50, 50, 50, 50, 25},      // width decides
		{50, 100, 50, 50, 25, 50},      // height decides
		{100, 100, 30, 20, 20, 20},
		{4096, 3072, 2000, 1000, 1333, 1000},
		{3072, 4096, 2000, 1000, 750, 1000},
		{1920, 1080, 100, 100, 100, 56},
		{3, 2, 2, 2, 2, 1},
		{1000, 1, 10, 10, 10, 1}, // never thinner than a pixel
		{1, 1000, 10, 10, 1, 10},
		{1, 1, 5, 5, 1, 1},
		{0, 0, 10, 10, 1, 1}, // nonsense in, 1x1 out
		{-4, 7, 10, 10, 1, 7},
		{10, 10, 0, 0, 1, 1},
		{10, 10, -3, 5, 1, 1},
		{1 << 30, 1 << 30, 100, 50, 50, 50}, // no overflow
	}
	for _, tt := range tests {
		w, h := Fit(tt.w, tt.h, tt.maxW, tt.maxH)
		if w != tt.wantW || h != tt.wantH {
			t.Errorf("Fit(%d, %d, %d, %d) = %d, %d; want %d, %d", tt.w, tt.h, tt.maxW, tt.maxH, w, h, tt.wantW, tt.wantH)
		}
	}

	// For any input: inside the limits, at least 1x1, not enlarged; and
	// when shrunk, one side at its limit and the other within half a pixel
	// of the exact shape (or at the one-pixel minimum).
	n := noise(7)
	for i := 0; i < 20000; i++ {
		w, h := int(n.next()%5000)+1, int(n.next()%5000)+1
		maxW, maxH := int(n.next()%3000)+1, int(n.next()%3000)+1
		fw, fh := Fit(w, h, maxW, maxH)
		var ok bool
		if w <= maxW && h <= maxH {
			ok = fw == w && fh == h
		} else {
			inside := fw >= 1 && fh >= 1 && fw <= maxW && fh <= maxH && fw <= w && fh <= h
			exactH := float64(h) * float64(fw) / float64(w)
			exactW := float64(w) * float64(fh) / float64(h)
			ok = inside && (fw == maxW && (math.Abs(float64(fh)-exactH) <= 0.5 || fh == 1) ||
				fh == maxH && (math.Abs(float64(fw)-exactW) <= 0.5 || fw == 1))
		}
		if !ok {
			t.Fatalf("Fit(%d, %d, %d, %d) = %d, %d", w, h, maxW, maxH, fw, fh)
		}
	}
}

// sources returns the same picture (more or less: the lossy kinds differ) in
// every kind of image Scale has a path for.
func sources(w, h int) map[string]image.Image {
	rgba := photo(w, h)
	out := map[string]image.Image{"RGBA": rgba}

	translucent := image.NewNRGBA(rgba.Rect)
	for i := 0; i < len(rgba.Pix); i += 4 {
		copy(translucent.Pix[i:i+3], rgba.Pix[i:i+3])
		translucent.Pix[i+3] = [4]uint8{255, 255, 128, 0}[i/4/7%4]
	}
	out["NRGBA"] = translucent

	convert := func(dst draw.Image) image.Image {
		draw.Draw(dst, dst.Bounds(), rgba, image.Point{}, draw.Src)
		return dst
	}
	out["Gray"] = convert(image.NewGray(rgba.Rect))
	out["RGBA64"] = convert(image.NewRGBA64(rgba.Rect)) // read pixel by pixel
	out["CMYK"] = convert(image.NewCMYK(rgba.Rect))
	out["At only"] = atOnly{translucent}

	pal := make(color.Palette, 0, 256)
	for i := 0; i < 255; i++ {
		pal = append(pal, color.RGBA{uint8(i % 6 * 51), uint8(i / 6 % 6 * 51), uint8(i / 36 * 36), 255})
	}
	pal = append(pal, color.RGBA{}) // and a transparent one
	paletted := image.NewPaletted(rgba.Rect, pal)
	draw.Draw(paletted, rgba.Rect, rgba, image.Point{}, draw.Src)
	for i := 0; i < len(paletted.Pix); i += 11 {
		paletted.Pix[i] = 255
	}
	out["Paletted"] = paletted

	// The YCbCr kinds come from a calmer picture: mid-range colours, and
	// noise in the brightness only. Scale averages them before converting
	// to RGB, where the pixel-by-pixel reference converts first; the two
	// only agree while no pixel's colour gets clipped at 0 or 255. And
	// Scale interpolates the half-size colour planes where At repeats
	// samples, which only agrees while the colour changes gently.
	calm := calmPhoto(w, h)
	for name, ratio := range map[string]image.YCbCrSubsampleRatio{
		"YCbCr 4:4:4": image.YCbCrSubsampleRatio444, "YCbCr 4:2:2": image.YCbCrSubsampleRatio422,
		"YCbCr 4:2:0": image.YCbCrSubsampleRatio420, "YCbCr 4:4:0": image.YCbCrSubsampleRatio440,
		"YCbCr 4:1:1": image.YCbCrSubsampleRatio411, "YCbCr 4:1:0": image.YCbCrSubsampleRatio410,
	} {
		out[name] = toYCbCr(calm, ratio)
	}

	alpha := image.NewNYCbCrA(rgba.Rect, image.YCbCrSubsampleRatio420)
	y := toYCbCr(calm, image.YCbCrSubsampleRatio420)
	copy(alpha.Y, y.Y)
	copy(alpha.Cb, y.Cb)
	copy(alpha.Cr, y.Cr)
	for i := range alpha.A {
		alpha.A[i] = uint8(i * 5)
	}
	out["NYCbCrA"] = alpha
	return out
}

// calmPhoto is a gradient in the middle of the colour range with noise in
// its brightness only.
func calmPhoto(w, h int) *image.RGBA {
	img := gradient(w, h)
	n := noise(362436069)
	for i := 0; i < len(img.Pix); i += 4 {
		d := int(n.next()&0x0F) - 8
		for c := 0; c < 3; c++ {
			img.Pix[i+c] = uint8(64 + int(img.Pix[i+c])/2 + d)
		}
	}
	return img
}

// isYCbCr reports whether a kind of sources is one of the YCbCr ones, and
// if so how many pixels one of its colour samples spans across and down.
func isYCbCr(name string) (ok bool, hs, vs int) {
	switch name {
	case "YCbCr 4:4:4":
		return true, 1, 1
	case "YCbCr 4:2:2":
		return true, 2, 1
	case "YCbCr 4:2:0":
		return true, 2, 2
	case "YCbCr 4:4:0":
		return true, 1, 2
	case "YCbCr 4:1:1":
		return true, 4, 1
	case "YCbCr 4:1:0":
		return true, 4, 2
	}
	return false, 0, 0
}

// toYCbCr converts a picture the way a JPEG encoder would: one chroma
// sample is the average of the pixels it covers.
func toYCbCr(src *image.RGBA, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	dst := image.NewYCbCr(src.Rect, ratio)
	hs, vs := chromaStep(ratio)
	b := src.Rect
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := src.RGBAAt(x, y)
			dst.Y[dst.YOffset(x, y)], _, _ = color.RGBToYCbCr(c.R, c.G, c.B)
		}
	}
	for y := b.Min.Y / vs * vs; y < b.Max.Y; y += vs {
		for x := b.Min.X / hs * hs; x < b.Max.X; x += hs {
			var cb, cr, n int
			for yy := max(y, b.Min.Y); yy < min(y+vs, b.Max.Y); yy++ {
				for xx := max(x, b.Min.X); xx < min(x+hs, b.Max.X); xx++ {
					c := src.RGBAAt(xx, yy)
					_, u, v := color.RGBToYCbCr(c.R, c.G, c.B)
					cb, cr, n = cb+int(u), cr+int(v), n+1
				}
			}
			o := dst.COffset(max(x, b.Min.X), max(y, b.Min.Y))
			dst.Cb[o], dst.Cr[o] = uint8((cb+n/2)/n), uint8((cr+n/2)/n)
		}
	}
	return dst
}

// referenceShrink is the definition of what Scale does when shrinking,
// written the slow and obvious way: each output pixel is the average of the
// source area under it, edge pixels counted by the fraction covered.
func referenceShrink(img image.Image, w, h int) [][4]float64 {
	sw, sh := img.Bounds().Dx(), img.Bounds().Dy()
	out := make([][4]float64, w*h)
	overlap := func(lo, hi float64, i int) float64 {
		return math.Max(0, math.Min(hi, float64(i+1))-math.Max(lo, float64(i)))
	}
	for y := 0; y < h; y++ {
		y0, y1 := float64(y)*float64(sh)/float64(h), float64(y+1)*float64(sh)/float64(h)
		for x := 0; x < w; x++ {
			x0, x1 := float64(x)*float64(sw)/float64(w), float64(x+1)*float64(sw)/float64(w)
			var sum [4]float64
			var total float64
			for sy := int(y0); sy < sh && float64(sy) < y1; sy++ {
				for sx := int(x0); sx < sw && float64(sx) < x1; sx++ {
					wgt := overlap(x0, x1, sx) * overlap(y0, y1, sy)
					c := rgbaAt(img, sx, sy) // the standard 8-bit value of the pixel
					sum[0] += wgt * float64(c.R)
					sum[1] += wgt * float64(c.G)
					sum[2] += wgt * float64(c.B)
					sum[3] += wgt * float64(c.A)
					total += wgt
				}
			}
			for c := range sum {
				out[y*w+x][c] = sum[c] / total
			}
		}
	}
	return out
}

// worstError is the largest difference in any channel of any pixel.
func worstError(got *image.RGBA, want [][4]float64) float64 {
	var worst float64
	for i, w := range want {
		for c := range w {
			worst = math.Max(worst, math.Abs(float64(got.Pix[4*i+c])-w[c]))
		}
	}
	return worst
}

func TestScaleShrinkMatchesReference(t *testing.T) {
	sizes := []struct{ sw, sh, w, h int }{
		{64, 48, 32, 24}, // exactly half
		{64, 48, 16, 6},  // whole pixels per pixel
		{64, 48, 23, 17}, // edges cut through pixels
		{64, 48, 63, 47}, // barely smaller: every pixel is split
		{64, 48, 1, 1},
		{64, 48, 64, 5}, // one direction only
		{64, 48, 7, 48},
		{61, 37, 20, 11}, // odd sizes: the last chroma sample is half outside
		{300, 2, 7, 1},
	}
	for _, size := range sizes {
		for name, src := range sources(size.sw, size.sh) {
			// The only difference allowed is rounding to whole numbers.
			tol := 0.51
			if ok, hs, vs := isYCbCr(name); ok {
				// Rounding happens before and after the conversion
				// to RGB, in different places than in the reference.
				tol = 2.5
				if size.sw%hs != 0 || size.sh%vs != 0 || size.w*hs > size.sw || size.h*vs > size.sh {
					// The colour planes are enlarged (interpolated,
					// where At repeats samples), or their last sample
					// hangs over the edge of an odd-sized picture.
					tol = 6
				}
			}
			got := Scale(src, size.w, size.h)
			if got.Rect != image.Rect(0, 0, size.w, size.h) {
				t.Fatalf("%s: bounds %v", name, got.Rect)
			}
			if worst := worstError(got, referenceShrink(src, size.w, size.h)); worst > tol {
				t.Errorf("%s %dx%d -> %dx%d: off by %.2f, want at most %.2f", name, size.sw, size.sh, size.w, size.h, worst, tol)
			}
		}
	}
}

// TestScaleSameSize: scaling to the same size is a plain conversion, exact
// for every kind of image. The one exception is deliberate: colour planes
// stored at a lower resolution are interpolated up, not repeated, which in
// this gentle gradient makes a difference of a few levels at most.
func TestScaleSameSize(t *testing.T) {
	const w, h = 160, 120
	for name, src := range sources(w, h) {
		got := Scale(src, w, h)
		tol := 0
		if ok, hs, vs := isYCbCr(name); ok && hs*vs > 1 {
			tol = 6
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				want := rgbaAt(src, x, y)
				if d := maxDiff(got.RGBAAt(x, y), want); d > tol {
					t.Fatalf("%s: pixel %d,%d = %v, want %v", name, x, y, got.RGBAAt(x, y), want)
				}
			}
		}
	}
}

// TestScaleFlat: a single colour stays exactly that colour whatever the
// sizes, because the weights are whole numbers.
func TestScaleFlat(t *testing.T) {
	flat := func(img draw.Image, c color.Color) image.Image {
		draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
		return img
	}
	r := image.Rect(0, 0, 37, 23)
	ycbcr := image.NewYCbCr(r, image.YCbCrSubsampleRatio420)
	for i := range ycbcr.Y {
		ycbcr.Y[i] = 150
	}
	for i := range ycbcr.Cb {
		ycbcr.Cb[i], ycbcr.Cr[i] = 100, 170
	}
	srcs := map[string]image.Image{
		"RGBA":        flat(image.NewRGBA(r), color.RGBA{201, 99, 3, 255}),
		"RGBA dim":    flat(image.NewRGBA(r), color.RGBA{100, 50, 3, 130}),
		"NRGBA":       flat(image.NewNRGBA(r), color.NRGBA{201, 99, 3, 77}),
		"Gray":        flat(image.NewGray(r), color.Gray{123}),
		"Paletted":    flat(image.NewPaletted(r, color.Palette{color.RGBA{9, 8, 7, 255}, color.RGBA{201, 99, 3, 255}}), color.RGBA{201, 99, 3, 255}),
		"RGBA64":      flat(image.NewRGBA64(r), color.RGBA{201, 99, 3, 255}),
		"YCbCr 4:2:0": ycbcr,
	}
	for name, src := range srcs {
		want := rgbaAt(src, 0, 0)
		for _, size := range []image.Point{{10, 7}, {36, 22}, {1, 1}, {37, 23}, {100, 71}, {38, 24}, {5, 60}} {
			got := Scale(src, size.X, size.Y)
			for i := 0; i < len(got.Pix); i += 4 {
				if c := (color.RGBA{got.Pix[i], got.Pix[i+1], got.Pix[i+2], got.Pix[i+3]}); c != want {
					t.Fatalf("%s to %v: pixel %d = %v, want %v", name, size, i/4, c, want)
				}
			}
		}
	}
}

func TestScaleKnownValues(t *testing.T) {
	px := func(vals ...uint8) *image.Gray {
		return &image.Gray{Pix: vals, Stride: len(vals), Rect: image.Rect(0, 0, len(vals), 1)}
	}
	row := func(img *image.RGBA) []uint8 {
		var out []uint8
		for i := 0; i < len(img.Pix); i += 4 {
			out = append(out, img.Pix[i])
		}
		return out
	}
	tests := []struct {
		name string
		src  *image.Gray
		w    int
		want []uint8
	}{
		{"halve", px(0, 100, 50, 150), 2, []uint8{50, 100}},
		{"quarter", px(0, 100, 50, 150), 1, []uint8{75}},
		// 3 -> 2: each output covers one pixel and half of the middle one.
		{"two thirds", px(0, 90, 255), 2, []uint8{30, 200}},
		// Enlarging blends neighbours by distance; the ends repeat.
		{"double", px(0, 255), 4, []uint8{0, 64, 191, 255}},
		{"triple", px(0, 90), 6, []uint8{0, 0, 30, 60, 90, 90}},
		{"same", px(1, 2, 3), 3, []uint8{1, 2, 3}},
		{"from one", px(77), 5, []uint8{77, 77, 77, 77, 77}},
	}
	for _, tt := range tests {
		if got := row(Scale(tt.src, tt.w, 1)); !bytes.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}

	// Both directions at once: 4x4 -> 2x2 averages each quarter.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < 16; i++ {
		img.SetRGBA(i%4, i/4, color.RGBA{uint8(i * 10), uint8(i), 0, 255})
	}
	got := Scale(img, 2, 2)
	for i, want := range []color.RGBA{{25, 3, 0, 255}, {45, 5, 0, 255}, {105, 11, 0, 255}, {125, 13, 0, 255}} {
		if c := got.RGBAAt(i%2, i/2); c != want {
			t.Errorf("quarter %d = %v, want %v", i, c, want)
		}
	}

	// Transparency: colour must be weighted by alpha, so the white of an
	// opaque pixel isn't greyed by the black of a transparent neighbour.
	n := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	n.SetNRGBA(0, 0, color.NRGBA{255, 255, 255, 255})
	n.SetNRGBA(1, 0, color.NRGBA{0, 0, 0, 0})
	if c := Scale(n, 1, 1).RGBAAt(0, 0); c != (color.RGBA{128, 128, 128, 128}) {
		t.Errorf("white + transparent = %v, want half-transparent white (128,128,128,128)", c)
	}
}

// TestScaleSubImage: an image that is a window on a larger one (bounds not
// at the origin, rows longer than the picture) scales like a copy of it.
func TestScaleSubImage(t *testing.T) {
	window := image.Rect(5, 7, 5+120, 7+78)
	for name, big := range sources(160, 120) {
		sub, ok := big.(interface {
			SubImage(image.Rectangle) image.Image
		})
		if !ok {
			continue
		}
		view := sub.SubImage(window)
		// The reference reads pixel by pixel through At, which knows
		// nothing of strides and offsets.
		for _, size := range []image.Point{window.Size(), {39, 27}, {120, 15}, {17, 78}} {
			got := Scale(view, size.X, size.Y)
			var worst float64
			if size == window.Size() {
				for y := 0; y < size.Y; y++ {
					for x := 0; x < size.X; x++ {
						worst = math.Max(worst, float64(maxDiff(got.RGBAAt(x, y), rgbaAt(view, x, y))))
					}
				}
			} else {
				worst = worstError(got, referenceShrink(view, size.X, size.Y))
			}
			tol := 0.51
			if ok, hs, vs := isYCbCr(name); ok && hs*vs > 1 {
				// As in TestScaleSameSize; and the window starts at
				// odd coordinates, in the middle of colour samples.
				tol = 6
			} else if ok {
				tol = 2.5
			}
			if size == window.Size() && tol < 3 {
				tol = 0
			}
			if worst > tol {
				t.Errorf("%s window to %v: off by %.2f, want at most %.2f", name, size, worst, tol)
			}
		}
	}
}

func TestScaleEdgeCases(t *testing.T) {
	src := gradient(8, 8)
	before := append([]byte{}, src.Pix...)
	for _, size := range []image.Point{{0, 5}, {5, 0}, {0, 0}, {-3, 4}, {4, -3}} {
		if got := Scale(src, size.X, size.Y); got == nil || !got.Rect.Empty() {
			t.Errorf("Scale to %v = %v, want an empty image", size, got)
		}
	}
	for _, empty := range []image.Image{nil, image.NewRGBA(image.Rectangle{}), image.NewYCbCr(image.Rect(0, 0, 0, 4), image.YCbCrSubsampleRatio420)} {
		got := Scale(empty, 3, 2)
		if got.Rect != image.Rect(0, 0, 3, 2) || !bytes.Equal(got.Pix, make([]byte, 24)) {
			t.Errorf("Scale(%v) = %v %v, want 3x2 transparent", empty, got.Rect, got.Pix)
		}
	}
	// Extreme shapes.
	if got := Scale(gradient(1000, 1), 1, 1).RGBAAt(0, 0); maxDiff(got, color.RGBA{127, 0, 127, 255}) > 1 {
		t.Errorf("1000x1 -> 1x1 = %v", got)
	}
	if got := Scale(gradient(1, 1000), 3, 2).RGBAAt(2, 1); maxDiff(got, color.RGBA{0, 191, 191, 255}) > 1 {
		t.Errorf("1x1000 -> 3x2 = %v", got)
	}
	Scale(src, 3, 5)
	Scale(src, 30, 50)
	if !bytes.Equal(src.Pix, before) {
		t.Error("Scale changed its source")
	}
}

// TestAxisWeights: every output sample's weights add up to the same total,
// stay inside the source, and between them use each source sample equally
// when shrinking (nothing is skipped, nothing counted twice).
func TestAxisWeights(t *testing.T) {
	for _, tt := range [][2]int{{10, 3}, {3, 10}, {7, 7}, {4096, 2000}, {3072, 1000}, {5, 4}, {1, 9}, {9, 1}, {1000, 999}, {2, 3}} {
		src, dst := tt[0], tt[1]
		a := newAxis(src, dst)
		used := make([]uint64, src)
		for i, sp := range a.spans {
			if sp.n < 1 || sp.start < 0 || int(sp.start+sp.n) > src {
				t.Fatalf("%d->%d: span %d = %+v is outside the source", src, dst, i, sp)
			}
			var sum uint64
			for k := int32(0); k < sp.n; k++ {
				w := a.mid
				switch k {
				case 0:
					w = uint64(sp.wFirst)
				case sp.n - 1:
					w = uint64(sp.wLast)
				}
				sum += w
				used[sp.start+k] += w
			}
			if sum != a.total {
				t.Fatalf("%d->%d: span %d = %+v weighs %d, want %d", src, dst, i, sp, sum, a.total)
			}
		}
		if dst < src {
			for k, w := range used {
				if w != uint64(dst) {
					t.Fatalf("%d->%d: source sample %d is used with weight %d, want %d", src, dst, k, w, dst)
				}
			}
		}
	}
}

func ExampleFit() {
	// A 4096x3072 photo on a terminal of 200x50 cells of 10x20 pixels.
	w, h := Fit(4096, 3072, 200*10, 50*20)
	fmt.Println(w, h)
	// Output: 1333 1000
}
