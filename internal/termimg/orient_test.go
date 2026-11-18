package termimg

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"slices"
	"testing"
)

// letters builds a picture from rows of letters, one pixel each, every
// letter a colour of its own.
func letters(rows ...string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, len(rows[0]), len(rows)))
	for y, row := range rows {
		for x, c := range []byte(row) {
			img.SetRGBA(x, y, color.RGBA{R: c, G: 255 - c, B: c * 7, A: 255})
		}
	}
	return img
}

// lettersOf reads a picture made by letters back, through At.
func lettersOf(img image.Image) []string {
	b := img.Bounds()
	rows := make([]string, b.Dy())
	for y := range rows {
		row := make([]byte, b.Dx())
		for x := range row {
			row[x] = rgbaAt(img, x, y).R
		}
		rows[y] = string(row)
	}
	return rows
}

// storedAs is how a camera stores the picture
//
//	ABC
//	DEF
//
// for each EXIF orientation. The orientation says where the first stored
// row and the first stored column belong in the upright picture: for 6,
// "row 0 is the right side, column 0 is the top", so the first stored row
// reads the right side top to bottom: C, F.
var storedAs = map[int][]string{
	1: {"ABC", "DEF"},     // row 0 top, column 0 left
	2: {"CBA", "FED"},     // row 0 top, column 0 right: mirrored
	3: {"FED", "CBA"},     // row 0 bottom, column 0 right: upside down
	4: {"DEF", "ABC"},     // row 0 bottom, column 0 left: flipped
	5: {"AD", "BE", "CF"}, // row 0 left, column 0 top
	6: {"CF", "BE", "AD"}, // row 0 right, column 0 top: camera turned clockwise
	7: {"FC", "EB", "DA"}, // row 0 right, column 0 bottom
	8: {"DA", "EB", "FC"}, // row 0 left, column 0 bottom: camera turned anticlockwise
}

func TestOrient(t *testing.T) {
	upright := []string{"ABC", "DEF"}
	for o := 1; o <= 8; o++ {
		stored := letters(storedAs[o]...)
		before := append([]byte{}, stored.Pix...)

		// In every kind of image, and as a window on a larger one.
		nrgba := image.NewNRGBA(stored.Rect)
		draw.Draw(nrgba, nrgba.Rect, stored, image.Point{}, draw.Src)
		big := image.NewRGBA(image.Rect(0, 0, 9, 9))
		draw.Draw(big, stored.Rect.Add(image.Pt(4, 3)), stored, image.Point{}, draw.Src)
		for name, src := range map[string]image.Image{
			"RGBA":      stored,
			"NRGBA":     nrgba,
			"At only":   atOnly{stored},
			"sub-image": big.SubImage(stored.Rect.Add(image.Pt(4, 3))),
		} {
			got := Orient(src, o)
			wantBounds := image.Rect(0, 0, 3, 2)
			if o == 1 {
				wantBounds = src.Bounds() // the image itself, wherever it starts
			}
			if got.Bounds() != wantBounds {
				t.Errorf("orientation %d, %s: bounds %v, want %v", o, name, got.Bounds(), wantBounds)
				continue
			}
			if rows := lettersOf(got); !slices.Equal(rows, upright) {
				t.Errorf("orientation %d, %s: %q, want %q", o, name, rows, upright)
			}
			// Scale understands the view (and takes a faster way).
			if rows := lettersOf(Scale(got, 3, 2)); !slices.Equal(rows, upright) {
				t.Errorf("orientation %d, %s, scaled: %q, want %q", o, name, rows, upright)
			}
			if got.ColorModel() != src.ColorModel() {
				t.Errorf("orientation %d, %s: colour model changed", o, name)
			}
			// Both ways of reading a pixel agree.
			if v, ok := got.(image.RGBA64Image); ok {
				r, g, b, a := got.At(2, 1).RGBA()
				if c := v.RGBA64At(2, 1); uint32(c.R) != r || uint32(c.G) != g || uint32(c.B) != b || uint32(c.A) != a {
					t.Errorf("orientation %d, %s: RGBA64At = %v, At = %v", o, name, c, got.At(2, 1))
				}
			}
		}
		if (Orient(stored, o) == image.Image(stored)) != (o == 1) {
			t.Errorf("orientation %d: only orientation 1 returns the image itself", o)
		}
		if !bytes.Equal(stored.Pix, before) {
			t.Errorf("orientation %d: Orient or Scale changed the source", o)
		}
	}

	// Anything that isn't an orientation leaves the image alone.
	img := letters("AB")
	for _, o := range []int{0, -1, 9, 100} {
		if Orient(img, o) != image.Image(img) {
			t.Errorf("Orient(img, %d) is not img", o)
		}
	}
	if Orient(nil, 6) != nil {
		t.Error("Orient(nil, 6) is not nil")
	}
}

// TestOrientThenScale: turning first and scaling after gives the same
// pixels as scaling the turned picture the slow way, for every orientation
// and for shrinking, enlarging and the same size.
func TestOrientThenScale(t *testing.T) {
	src := photo(41, 29)
	jpg := photoYCbCr(40, 28, image.YCbCrSubsampleRatio420)
	for o := 1; o <= 8; o++ {
		for _, img := range []image.Image{src, jpg} {
			view := Orient(img, o)
			vb := view.Bounds()
			want := vb.Size()
			if got := img.Bounds().Size(); o >= 5 {
				want = image.Pt(got.Y, got.X)
			} else if want != got {
				t.Fatalf("orientation %d: view is %v, source %v", o, want, got)
			}
			if vb.Size() != want {
				t.Fatalf("orientation %d: view is %v, want %v", o, vb.Size(), want)
			}

			// A plain copy of the view, made pixel by pixel.
			copied := image.NewRGBA(vb)
			for y := 0; y < vb.Dy(); y++ {
				for x := 0; x < vb.Dx(); x++ {
					copied.Set(x, y, view.At(x, y))
				}
			}
			for _, size := range []image.Point{vb.Size(), {13, 9}, {9, 13}, {60, 70}, {1, 1}} {
				got := Scale(view, size.X, size.Y)
				if got.Rect.Size() != size {
					t.Fatalf("orientation %d to %v: size %v", o, size, got.Rect.Size())
				}
				if _, isJPEG := img.(*image.YCbCr); isJPEG {
					// Scaling the YCbCr planes isn't the same sum as
					// scaling RGB pixels; near enough is enough here,
					// the exact check is done on the RGBA picture.
					if worst := worstRGBADiff(got, Scale(copied, size.X, size.Y)); worst > 12 {
						t.Errorf("orientation %d, YCbCr to %v: off by %d", o, size, worst)
					}
					continue
				}
				if !bytes.Equal(got.Pix, Scale(copied, size.X, size.Y).Pix) {
					t.Errorf("orientation %d to %v: differs from scaling a turned copy", o, size)
				}
			}
		}
	}
}

func worstRGBADiff(a, b *image.RGBA) int {
	worst := 0
	for i := range a.Pix {
		worst = max(worst, int(a.Pix[i])-int(b.Pix[i]), int(b.Pix[i])-int(a.Pix[i]))
	}
	return worst
}

// TestOrientNested: a view of a view works, and turning back and forth
// gives the original.
func TestOrientNested(t *testing.T) {
	img := letters("ABC", "DEF")
	// Orientation 6 and 8 undo each other, and so do 5 with 5, 7 with 7...
	for _, pair := range [][2]int{{6, 8}, {8, 6}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {7, 7}} {
		// "Store" the picture with one orientation, view it with the other.
		back := Orient(Orient(img, pair[0]), pair[1])
		if rows := lettersOf(back); !slices.Equal(rows, []string{"ABC", "DEF"}) {
			t.Errorf("orientations %v: %q", pair, rows)
		}
		if rows := lettersOf(Scale(back, 3, 2)); !slices.Equal(rows, []string{"ABC", "DEF"}) {
			t.Errorf("orientations %v, scaled: %q", pair, rows)
		}
	}
}
