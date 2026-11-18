package termimg

import (
	"image"
	"image/color"
)

// Fit returns the largest size that fits in maxW x maxH while keeping w:h.
// It never upscales, and never returns less than 1x1.
func Fit(w, h, maxW, maxH int) (int, int) {
	w, h, maxW, maxH = max(w, 1), max(h, 1), max(maxW, 1), max(maxH, 1)
	if w <= maxW && h <= maxH {
		return w, h
	}
	// Which side reaches its limit first: is w/maxW or h/maxH larger?
	// Cross-multiplied to stay in whole numbers.
	if int64(w)*int64(maxH) >= int64(h)*int64(maxW) {
		return maxW, max(1, roundDiv(int64(h)*int64(maxW), int64(w)))
	}
	return max(1, roundDiv(int64(w)*int64(maxH), int64(h))), maxH
}

func roundDiv(a, b int64) int { return int((a + b/2) / b) }

// Scale resamples src to exactly w x h. Shrinking averages all the source
// pixels under each output pixel (so fine patterns blur instead of turning
// into moiré); enlarging interpolates between neighbours.
//
// It reads straight from src's own pixel format, row by row, and never
// converts the whole source to RGBA first: beyond the result it allocates a
// few rows. *image.YCbCr (what a JPEG decodes to), *image.Gray, *image.RGBA,
// *image.NRGBA and *image.Paletted are read directly from their buffers;
// anything else pixel by pixel.
//
// The result is alpha-premultiplied like every *image.RGBA, which also means
// its R, G, B are the picture composited on black. A nil or empty src gives
// a transparent image.
func Scale(src image.Image, w, h int) *image.RGBA {
	w, h = max(w, 0), max(h, 0)
	if v, ok := src.(*oriented); ok {
		// Scale the stored picture, then turn the small result.
		if v.o >= 5 {
			w, h = h, w
		}
		return orientRGBA(Scale(v.src, w, h), v.o)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if src == nil || w == 0 || h == 0 {
		return dst
	}
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return dst
	}
	xs, ys := newAxis(b.Dx(), w), newAxis(b.Dy(), h)
	switch s := src.(type) {
	case *image.YCbCr:
		if scaleYCbCr(dst, s, xs, ys) {
			return dst
		}
	case *image.Gray:
		scaleGray(dst, s, xs, ys)
		return dst
	}
	scaleRGBA(dst, rgbaRows(src), xs, ys)
	return dst
}

// span says which source samples one output sample is made of.
type span struct {
	start         int32  // the first source sample
	n             int32  // how many
	wFirst, wLast uint32 // weight of the first and of the last (when n > 1)
}

// axis is the plan for resampling one dimension: a span per output sample.
// Weights are whole numbers, so a flat colour stays exactly that colour.
type axis struct {
	spans []span
	mid   uint64 // weight of the samples between a span's first and last
	total uint64 // what the weights of any one span add up to
}

// newAxis plans the resampling of src samples to dst samples.
func newAxis(src, dst int) *axis {
	a := &axis{spans: make([]span, dst)}
	s, d := int64(src), int64(dst)
	if dst < src {
		// Shrinking: output sample i covers the source from i*src/dst to
		// (i+1)*src/dst. Measured in units of 1/dst of a source sample,
		// that is from i*src to (i+1)*src, and source sample k covers
		// k*dst to (k+1)*dst; the weights are the overlaps.
		a.mid, a.total = uint64(d), uint64(s)
		for i := range a.spans {
			lo := int64(i) * s
			hi := lo + s
			first, last := lo/d, (hi-1)/d
			sp := span{start: int32(first), n: int32(last - first + 1)}
			if sp.n == 1 {
				sp.wFirst = uint32(s)
			} else {
				sp.wFirst = uint32((first+1)*d - lo)
				sp.wLast = uint32(hi - last*d)
			}
			a.spans[i] = sp
		}
		return a
	}
	// Enlarging (or the same size): blend the two source samples on either
	// side of the output sample's centre, which lies at (i+0.5)*src/dst-0.5.
	// In units of 1/(2*dst) that is (2i+1)*src - dst.
	a.total = uint64(2 * d)
	for i := range a.spans {
		pos := (2*int64(i)+1)*s - d
		sp := span{n: 1, wFirst: uint32(2 * d)}
		if pos > 0 {
			first, frac := pos/(2*d), pos%(2*d)
			sp.start = int32(first)
			if frac != 0 && first < s-1 {
				sp.n, sp.wFirst, sp.wLast = 2, uint32(2*d-frac), uint32(frac)
			}
		}
		a.spans[i] = sp
	}
	return a
}

// planeScaler resamples one plane of 8-bit samples (nch interleaved
// channels: 1 or 4), one output row at a time.
//
// Each source row is first resampled horizontally into a row of sums, then
// those rows are blended vertically. The two newest horizontal results are
// kept, because neighbouring output rows share a source row when shrinking
// and both of them when enlarging.
type planeScaler struct {
	nch    int
	xs, ys *axis
	row    func(y int) []byte // source row y; may be reused by the next call

	slot  [2][]uint64 // horizontally resampled rows...
	slotY [2]int      // ...and which source rows they are
	old   int         // the slot to overwrite next
	acc   []uint64    // the output row being built
}

func newPlaneScaler(nch int, xs, ys *axis, row func(y int) []byte) *planeScaler {
	n := nch * len(xs.spans)
	return &planeScaler{
		nch: nch, xs: xs, ys: ys, row: row,
		slot:  [2][]uint64{make([]uint64, n), make([]uint64, n)},
		slotY: [2]int{-1, -1},
		acc:   make([]uint64, n),
	}
}

// scale is what to multiply a sum from outRow by to get back to 0..255: one
// over the total weight it carries.
func (p *planeScaler) scale() float64 { return 1 / (float64(p.xs.total) * float64(p.ys.total)) }

// horizontal returns source row y resampled to the output width.
func (p *planeScaler) horizontal(y int) []uint64 {
	for i, have := range p.slotY {
		if have == y {
			p.old = 1 - i
			return p.slot[i]
		}
	}
	i := p.old
	p.old = 1 - i
	p.slotY[i] = y
	if p.nch == 1 {
		reduce1(p.slot[i], p.row(y), p.xs)
	} else {
		reduce4(p.slot[i], p.row(y), p.xs)
	}
	return p.slot[i]
}

// outRow returns output row y as weighted sums. The slice is reused by the
// next call.
func (p *planeScaler) outRow(y int) []uint64 {
	sp := p.ys.spans[y]
	acc := p.acc
	for i := int32(0); i < sp.n; i++ {
		wgt := p.ys.mid
		switch i {
		case 0:
			wgt = uint64(sp.wFirst)
		case sp.n - 1:
			wgt = uint64(sp.wLast)
		}
		row := p.horizontal(int(sp.start + i))[:len(acc)]
		if i == 0 {
			for j, v := range row {
				acc[j] = v * wgt
			}
		} else {
			for j, v := range row {
				acc[j] += v * wgt
			}
		}
	}
	return acc
}

// reduce1 resamples a row of single-channel samples along a.
func reduce1(dst []uint64, src []byte, a *axis) {
	dst = dst[:len(a.spans)]
	for i, sp := range a.spans {
		s := src[sp.start : sp.start+sp.n]
		sum := uint64(s[0]) * uint64(sp.wFirst)
		if last := len(s) - 1; last > 0 {
			var mid uint32
			for _, v := range s[1:last] {
				mid += uint32(v)
			}
			sum += uint64(mid)*a.mid + uint64(s[last])*uint64(sp.wLast)
		}
		dst[i] = sum
	}
}

// reduce4 is reduce1 for four interleaved channels.
func reduce4(dst []uint64, src []byte, a *axis) {
	dst = dst[:4*len(a.spans)]
	for i, sp := range a.spans {
		s := src[4*sp.start : 4*(sp.start+sp.n)]
		wf := uint64(sp.wFirst)
		c0, c1, c2, c3 := uint64(s[0])*wf, uint64(s[1])*wf, uint64(s[2])*wf, uint64(s[3])*wf
		if last := len(s) - 4; last > 0 {
			var m0, m1, m2, m3 uint32
			for mid := s[4:last]; len(mid) >= 4; mid = mid[4:] {
				m0 += uint32(mid[0])
				m1 += uint32(mid[1])
				m2 += uint32(mid[2])
				m3 += uint32(mid[3])
			}
			wl, e := uint64(sp.wLast), s[last:last+4]
			c0 += uint64(m0)*a.mid + uint64(e[0])*wl
			c1 += uint64(m1)*a.mid + uint64(e[1])*wl
			c2 += uint64(m2)*a.mid + uint64(e[2])*wl
			c3 += uint64(m3)*a.mid + uint64(e[3])*wl
		}
		d := dst[4*i : 4*i+4 : 4*i+4]
		d[0], d[1], d[2], d[3] = c0, c1, c2, c3
	}
}

// sample turns a weighted sum back into a sample, rounded. A sum that is an
// exact multiple (a flat area) comes out exact: the 0.5 hides the tiny error
// of multiplying by 1/total instead of dividing.
func sample(sum uint64, scale float64) uint8 {
	return uint8(float64(int64(sum))*scale + 0.5)
}

// scaleRGBA resamples a source given as rows of premultiplied RGBA.
func scaleRGBA(dst *image.RGBA, rows func(y int) []byte, xs, ys *axis) {
	p := newPlaneScaler(4, xs, ys, rows)
	scale := p.scale()
	w, h := dst.Rect.Dx(), dst.Rect.Dy()
	for y := 0; y < h; y++ {
		sums := p.outRow(y)
		out := dst.Pix[y*dst.Stride : y*dst.Stride+4*w]
		for i, v := range sums[:len(out)] {
			out[i] = sample(v, scale)
		}
	}
}

// scaleGray resamples the one plane of a grey image.
func scaleGray(dst *image.RGBA, s *image.Gray, xs, ys *axis) {
	b := s.Rect
	sw := b.Dx()
	p := newPlaneScaler(1, xs, ys, func(y int) []byte {
		o := s.PixOffset(b.Min.X, b.Min.Y+y)
		return s.Pix[o : o+sw]
	})
	scale := p.scale()
	w, h := dst.Rect.Dx(), dst.Rect.Dy()
	for y := 0; y < h; y++ {
		sums := p.outRow(y)
		out := dst.Pix[y*dst.Stride : y*dst.Stride+4*w]
		for x, v := range sums[:w] {
			g := sample(v, scale)
			o := out[4*x : 4*x+4 : 4*x+4]
			o[0], o[1], o[2], o[3] = g, g, g, 0xFF
		}
	}
}

// scaleYCbCr resamples the three planes of a YCbCr image separately (the
// colour planes are usually stored at half the size, so that is a quarter of
// the work for them) and converts to RGB only once per output pixel. It
// reports false for layouts it doesn't know, leaving dst untouched.
func scaleYCbCr(dst *image.RGBA, s *image.YCbCr, xs, ys *axis) bool {
	b := s.Rect
	hs, vs := chromaStep(s.SubsampleRatio)
	if hs == 0 || b.Min.X < 0 || b.Min.Y < 0 {
		return false
	}
	sw := b.Dx()
	w, h := dst.Rect.Dx(), dst.Rect.Dy()

	luma := newPlaneScaler(1, xs, ys, func(y int) []byte {
		o := s.YOffset(b.Min.X, b.Min.Y+y)
		return s.Y[o : o+sw]
	})

	// The chroma samples that cover the picture: columns Min.X/hs up to
	// (Max.X-1)/hs, and the same for rows.
	cw := (b.Max.X-1)/hs - b.Min.X/hs + 1
	ch := (b.Max.Y-1)/vs - b.Min.Y/vs + 1
	cxs, cys := xs, ys
	if hs > 1 {
		cxs = newAxis(cw, w)
	}
	if vs > 1 {
		cys = newAxis(ch, h)
	}
	firstRow := b.Min.Y / vs
	chroma := func(plane []byte) *planeScaler {
		return newPlaneScaler(1, cxs, cys, func(y int) []byte {
			o := s.COffset(b.Min.X, (firstRow+y)*vs)
			return plane[o : o+cw]
		})
	}
	cb, cr := chroma(s.Cb), chroma(s.Cr)

	lumaScale, chromaScale := luma.scale(), cb.scale()
	for y := 0; y < h; y++ {
		ly, lb, lr := luma.outRow(y)[:w], cb.outRow(y)[:w], cr.outRow(y)[:w]
		out := dst.Pix[y*dst.Stride : y*dst.Stride+4*w]
		for x := range ly {
			r, g, bl := color.YCbCrToRGB(sample(ly[x], lumaScale), sample(lb[x], chromaScale), sample(lr[x], chromaScale))
			o := out[4*x : 4*x+4 : 4*x+4]
			o[0], o[1], o[2], o[3] = r, g, bl, 0xFF
		}
	}
	return true
}

// chromaStep says how many luma samples one chroma sample spans, across and
// down, for a subsampling ratio; 0, 0 for one it doesn't know.
func chromaStep(r image.YCbCrSubsampleRatio) (hs, vs int) {
	switch r {
	case image.YCbCrSubsampleRatio444:
		return 1, 1
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	case image.YCbCrSubsampleRatio410:
		return 4, 2
	}
	return 0, 0
}
