package termimg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"math"
	"runtime"
	"testing"
)

func TestCost(t *testing.T) {
	tests := []struct {
		w, h   int
		size   int64
		format string
		want   int64
	}{
		{100, 50, 1000, "jpeg", 1000 + 100*50*3},
		{100, 50, 1000, "png", 1000 + 100*50*4},
		{100, 50, 1000, "webp", 1000 + 100*50*4},
		{100, 50, 0, "", 100 * 50 * 4},
		{4096, 3072, 2 << 20, "jpeg", 2<<20 + 4096*3072*3},
		{0, 50, 7, "png", 7},
		{-3, 50, 7, "png", 7},
		// Absurd sizes must saturate, not wrap around to something small.
		{math.MaxInt32, math.MaxInt32, 1, "png", math.MaxInt64},
		{1 << 40, 1 << 40, 1, "jpeg", math.MaxInt64},
		{1, 1, math.MaxInt64, "png", math.MaxInt64},
	}
	for _, tt := range tests {
		if got := Cost(tt.w, tt.h, tt.size, tt.format); got != tt.want {
			t.Errorf("Cost(%d, %d, %d, %q) = %d, want %d", tt.w, tt.h, tt.size, tt.format, got, tt.want)
		}
	}
}

// formats is one small file per supported format, with what decoding it
// must give.
func formats(t *testing.T) []struct {
	name, format string
	data         []byte
	w, h         int
	kind         string // the Go type it decodes to
} {
	pal := image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}})
	return []struct {
		name, format string
		data         []byte
		w, h         int
		kind         string
	}{
		{"baseline.jpg", "jpeg", encodeJPEG(t, gradient(40, 30)), 40, 30, "*image.YCbCr"},
		{"progressive.jpg", "jpeg", fixture(t, progressiveJPEG), 24, 16, "*image.YCbCr"},
		{"gray.jpg", "jpeg", encodeJPEG(t, image.NewGray(image.Rect(0, 0, 9, 7))), 9, 7, "*image.Gray"},
		{"rgb.png", "png", encodePNG(t, gradient(33, 21)), 33, 21, "*image.RGBA"},
		{"rgba.png", "png", encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 5, 6))), 5, 6, "*image.NRGBA"},
		{"deep.png", "png", encodePNG(t, deepGradient(5, 6)), 5, 6, "*image.RGBA64"},
		{"two.gif", "gif", encodeGIF(t, pal), 20, 10, "*image.Paletted"},
		{"plain.bmp", "bmp", encodeBMP(t, gradient(12, 34)), 12, 34, "*image.RGBA"},
		{"lossy.webp", "webp", fixture(t, lossyWebP), 24, 16, "*image.YCbCr"},
		{"lossless.webp", "webp", fixture(t, losslessAlphaWebP), 24, 16, "*image.NRGBA"},
		{"alpha.webp", "webp", fixture(t, lossyAlphaWebP), 24, 16, "*image.NYCbCrA"},
	}
}

func TestProbeAndDecodeFormats(t *testing.T) {
	for _, f := range formats(t) {
		t.Run(f.name, func(t *testing.T) {
			want := Info{Width: f.w, Height: f.h, Format: f.format, Size: int64(len(f.data)), Orientation: 1}
			path := writeTemp(t, f.name, f.data)

			if got, err := ProbeBytes(f.data); err != nil || got != want {
				t.Errorf("ProbeBytes = %+v, %v; want %+v", got, err, want)
			}
			if got, err := Probe(path); err != nil || got != want {
				t.Errorf("Probe = %+v, %v; want %+v", got, err, want)
			}

			check := func(what string, img image.Image, info Info, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if info != want {
					t.Errorf("%s info = %+v, want %+v", what, info, want)
				}
				if got := img.Bounds().Size(); got != image.Pt(f.w, f.h) {
					t.Errorf("%s size = %v, want %dx%d", what, got, f.w, f.h)
				}
				if got := fmt.Sprintf("%T", img); got != f.kind {
					t.Errorf("%s type = %s, want %s", what, got, f.kind)
				}
			}
			img, info, err := Decode(f.data, DefaultBudget)
			check("Decode", img, info, err)
			img, info, err = Load(path, 0) // 0 = the default budget
			check("Load", img, info, err)

			// Whatever came out, the rest of the package must be able
			// to read it.
			if got := Scale(img, 4, 3).Bounds().Size(); got != image.Pt(4, 3) {
				t.Errorf("Scale size = %v", got)
			}
		})
	}
}

func TestDecodedPixels(t *testing.T) {
	// A PNG is lossless: the exact gradient must come back.
	src := gradient(33, 21)
	img, _, err := Decode(encodePNG(t, src), DefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 21; y++ {
		for x := 0; x < 33; x++ {
			if got, want := rgbaAt(img, x, y), src.RGBAAt(x, y); got != want {
				t.Fatalf("pixel %d,%d = %v, want %v", x, y, got, want)
			}
		}
	}

	// The WebP fixtures are the left-to-right red, top-to-bottom green
	// gradient; the alpha ones transparent on the left.
	for _, f := range []struct {
		name, data string
		alpha      bool
	}{{"lossy", lossyWebP, false}, {"lossless", losslessAlphaWebP, true}, {"lossy+alpha", lossyAlphaWebP, true}} {
		img, _, err := Decode(fixture(t, f.data), DefaultBudget)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		right := rgbaAt(img, 22, 2)
		if right.R < 200 || right.G > 80 || right.A != 255 {
			t.Errorf("%s: top right = %v, want mostly red and opaque", f.name, right)
		}
		if got := rgbaAt(img, 14, 14); got.G < 190 {
			t.Errorf("%s: bottom = %v, want a lot of green", f.name, got)
		}
		if left := rgbaAt(img, 2, 8); f.alpha && left != (color.RGBA{}) {
			t.Errorf("%s: left = %v, want transparent", f.name, left)
		}
	}
}

func TestGIFFirstFrame(t *testing.T) {
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	red := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
	blue := image.NewPaletted(image.Rect(0, 0, 8, 8), pal)
	for i := range blue.Pix {
		blue.Pix[i] = 1
	}
	img, info, err := Decode(encodeGIF(t, red, blue), DefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "gif" || info.Width != 8 || info.Height != 8 {
		t.Errorf("info = %+v", info)
	}
	if got := rgbaAt(img, 3, 3); got != (color.RGBA{255, 0, 0, 255}) {
		t.Errorf("pixel = %v, want the red of the first frame", got)
	}
}

func TestNotAnImage(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("hello, this is text"), {0xFF, 0xD8, 0xFF}} {
		if _, err := ProbeBytes(data); err == nil {
			t.Errorf("ProbeBytes(%q) succeeded", data)
		}
		if img, _, err := Decode(data, DefaultBudget); err == nil || img != nil || errors.Is(err, ErrTooLarge) {
			t.Errorf("Decode(%q) = %v, %v", data, img, err)
		}
	}
	if _, err := Probe(writeTemp(t, "gone", nil) + ".missing"); err == nil {
		t.Error("Probe of a missing file succeeded")
	}
	if _, _, err := Load(writeTemp(t, "gone", nil)+".missing", 0); err == nil {
		t.Error("Load of a missing file succeeded")
	}
}

// pngHeader is the start of a PNG file that claims the given size: the
// signature and a correct IHDR chunk, and nothing else.
func pngHeader(w, h uint32, depth, interlace byte) []byte {
	ihdr := make([]byte, 4, 17)
	copy(ihdr, "IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, depth, 6 /* RGBA */, 0, 0, interlace)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, 13)
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}

// jpegHeaderBytes is the start of a JPEG file that claims the given size
// and layout: enough for the header to be read, with no pixel data.
// sampling is one byte per component, as in the file: 0x22 is 2x2.
func jpegHeaderBytes(w, h int, progressive bool, sampling ...byte) []byte {
	out := []byte{0xFF, 0xD8}
	out = append(out, 0xFF, 0xE0, 0, 16)
	out = append(out, "JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"...)
	sof := byte(0xC0)
	if progressive {
		sof = 0xC2
	}
	out = append(out, 0xFF, sof, 0, byte(8+3*len(sampling)), 8, byte(h>>8), byte(h), byte(w>>8), byte(w), byte(len(sampling)))
	for i, s := range sampling {
		out = append(out, byte(i+1), s, 0)
	}
	return out
}

// TestBudget checks that the budget is applied from the header alone:
// nothing may be decoded (or even allocated) for an image that is too large.
func TestBudget(t *testing.T) {
	isTooLarge := func(err error) bool { return errors.Is(err, ErrTooLarge) }

	t.Run("low budget", func(t *testing.T) {
		data := encodePNG(t, gradient(600, 400)) // costs 960 kB plus the file
		for _, load := range []func(budget int64) (image.Image, Info, error){
			func(budget int64) (image.Image, Info, error) { return Decode(data, budget) },
			func() func(int64) (image.Image, Info, error) {
				path := writeTemp(t, "big.png", data)
				return func(budget int64) (image.Image, Info, error) { return Load(path, budget) }
			}(),
		} {
			img, info, err := load(900 << 10)
			if img != nil || !isTooLarge(err) {
				t.Errorf("900 kB budget: got %v, %v; want ErrTooLarge", img, err)
			}
			if info.Width != 600 || info.Height != 400 || info.Format != "png" {
				t.Errorf("900 kB budget: info = %+v, want it filled in", info)
			}
			if img, _, err = load(2 << 20); img == nil || err != nil {
				t.Errorf("2 MB budget: got %v, %v; want the image", img, err)
			}
		}

		// The refusal must come from the header: it may not cost
		// anything like the picture.
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := 0; i < 10; i++ {
			Decode(data, 900<<10)
		}
		runtime.ReadMemStats(&after)
		if spent := (after.TotalAlloc - before.TotalAlloc) / 10; spent > 64<<10 {
			t.Errorf("refusing an image allocated %d bytes", spent)
		}
	})

	t.Run("exactly the cost", func(t *testing.T) {
		data := encodePNG(t, gradient(100, 100))
		cost := Cost(100, 100, int64(len(data)), "png")
		if _, _, err := Decode(data, cost); err != nil {
			t.Errorf("budget == cost: %v", err)
		}
		if _, _, err := Decode(data, cost-1); !isTooLarge(err) {
			t.Errorf("budget == cost-1: %v, want ErrTooLarge", err)
		}
	})

	// Files that are nothing but a header claiming a size. If one were
	// decoded the error would be about the missing data, not ErrTooLarge
	// (or, for the huge one, an attempt to allocate 3.6 GB).
	headers := []struct {
		name     string
		data     []byte
		tooLarge bool
	}{
		{"png 30000x30000", pngHeader(30000, 30000, 8, 0), true},
		{"png 65535x65535", pngHeader(65535, 65535, 8, 0), true},
		{"png 3200x3200", pngHeader(3200, 3200, 8, 0), false},                                            // 41 MB
		{"png 3200x3200 interlaced", pngHeader(3200, 3200, 8, 1), true},                                  // half as much again
		{"png 3200x3200 16-bit", pngHeader(3200, 3200, 16, 0), true},                                     // 8 bytes per pixel
		{"jpeg 4096x3072 baseline 4:2:0", jpegHeaderBytes(4096, 3072, false, 0x22, 0x11, 0x11), false},   // 19 MB
		{"jpeg 4096x3072 baseline 4:4:4", jpegHeaderBytes(4096, 3072, false, 0x11, 0x11, 0x11), false},   // 38 MB
		{"jpeg 4096x3072 progressive 4:2:0", jpegHeaderBytes(4096, 3072, true, 0x22, 0x11, 0x11), true},  // 94 MB
		{"jpeg 2000x1500 progressive 4:2:0", jpegHeaderBytes(2000, 1500, true, 0x22, 0x11, 0x11), false}, // 23 MB
		{"jpeg 2000x1500 progressive 4:4:4", jpegHeaderBytes(2000, 1500, true, 0x11, 0x11, 0x11), false}, // 45 MB
		{"jpeg 2300x1500 progressive 4:4:4", jpegHeaderBytes(2300, 1500, true, 0x11, 0x11, 0x11), true},  // 52 MB
		{"jpeg 4096x3072 CMYK", jpegHeaderBytes(4096, 3072, false, 0x11, 0x11, 0x11, 0x11), true},        // 101 MB
		// Really 20 MB, but Cost says 60: the header may only make the
		// check stricter than Cost, never looser.
		{"jpeg 5000x4000 gray", jpegHeaderBytes(5000, 4000, false, 0x11), true},
	}

	for _, h := range headers {
		t.Run(h.name, func(t *testing.T) {
			info, err := ProbeBytes(h.data)
			if err != nil {
				t.Fatalf("ProbeBytes: %v", err)
			}
			img, _, err := Decode(h.data, DefaultBudget)
			if img != nil || err == nil {
				t.Fatalf("Decode = %v, %v; want an error", img, err)
			}
			if got := isTooLarge(err); got != h.tooLarge {
				t.Errorf("Decode: %v; too large = %v, want %v (%dx%d)", err, got, h.tooLarge, info.Width, info.Height)
			}
		})
	}
}

// TestJPEGDecodedBytes pins the estimate of what image/jpeg allocates to
// numbers measured with real files (runtime.MemStats around jpeg.Decode of
// 4096x3072 photos): 19.0, 94.5, 37.8 and 188.8 MB.
func TestJPEGDecodedBytes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want float64 // MB
	}{
		{"baseline 4:2:0", jpegHeaderBytes(4096, 3072, false, 0x22, 0x11, 0x11), 18.9},
		{"progressive 4:2:0", jpegHeaderBytes(4096, 3072, true, 0x22, 0x11, 0x11), 94.4},
		{"baseline 4:4:4", jpegHeaderBytes(4096, 3072, false, 0x11, 0x11, 0x11), 37.7},
		{"progressive 4:4:4", jpegHeaderBytes(4096, 3072, true, 0x11, 0x11, 0x11), 188.7},
		{"baseline 4:2:2", jpegHeaderBytes(4096, 3072, false, 0x21, 0x11, 0x11), 25.2},
		{"gray", jpegHeaderBytes(4096, 3072, false, 0x11), 12.6},
		{"progressive gray", jpegHeaderBytes(4096, 3072, true, 0x11), 62.9},
		// Not a multiple of the 16x16 block group: rounded up to 4112x3088.
		{"baseline 4:2:0 odd size", jpegHeaderBytes(4097, 3073, false, 0x22, 0x11, 0x11), 19.05},
	}
	for _, tt := range tests {
		h, err := probe(bytes.NewReader(tt.data), int64(len(tt.data)))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		j := scanJPEG(bufioReader(tt.data))
		if got := float64(j.decodedBytes()) / 1e6; math.Abs(got-tt.want) > 0.1 {
			t.Errorf("%s: decodedBytes = %.2f MB, want %.2f", tt.name, got, tt.want)
		}
		if h.need < Cost(h.Width, h.Height, h.Size, "jpeg") {
			t.Errorf("%s: need %d is below Cost", tt.name, h.need)
		}
	}

	// The real progressive file: found to be progressive, and decodable.
	data := fixture(t, progressiveJPEG)
	j := scanJPEG(bufioReader(data))
	if !j.progressive || j.width != 24 || j.height != 16 || j.ncomp != 3 || j.h[0] != 2 || j.v[0] != 2 {
		t.Errorf("progressive fixture scanned as %+v", j)
	}
	// 24x16 is covered by 2x1 groups of 16x16 pixels: 6 blocks each, at
	// 256 bytes of coefficients, plus the 32x16 planes.
	if got, want := j.decodedBytes(), int64(2*6*256+32*16+2*16*8); got != want {
		t.Errorf("progressive fixture: decodedBytes = %d, want %d", got, want)
	}
	if j := scanJPEG(bufioReader(encodeJPEG(t, gradient(40, 30)))); j.progressive || j.width != 40 || j.height != 30 {
		t.Errorf("baseline file scanned as %+v", j)
	}
}

// byteOrder is binary.LittleEndian or binary.BigEndian, for reading and
// for appending.
type byteOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// exifBlock builds the payload of an EXIF segment whose directory holds a
// width tag and then the orientation, stored as the given TIFF type.
func exifBlock(order byteOrder, orientation uint32, typ uint16) []byte {
	tiff := []byte("II")
	if order == binary.BigEndian {
		tiff = []byte("MM")
	}
	tiff = order.AppendUint16(tiff, 42)
	tiff = order.AppendUint32(tiff, 8) // the directory follows at once
	tiff = order.AppendUint16(tiff, 2) // two entries
	entry := func(tag, typ uint16, value uint32) {
		tiff = order.AppendUint16(tiff, tag)
		tiff = order.AppendUint16(tiff, typ)
		tiff = order.AppendUint32(tiff, 1)
		// Values shorter than four bytes are left-aligned in the field.
		switch typ {
		case 1:
			tiff = append(tiff, byte(value), 0, 0, 0)
		case 3:
			tiff = order.AppendUint16(tiff, uint16(value))
			tiff = append(tiff, 0, 0)
		default:
			tiff = order.AppendUint32(tiff, value)
		}
	}
	entry(0x0100, 4, 640)
	entry(0x0112, typ, orientation)
	tiff = order.AppendUint32(tiff, 0) // no further directory
	return append([]byte("Exif\x00\x00"), tiff...)
}

// withSegments inserts segments (marker byte, payload) right after the
// start-of-image marker of a JPEG file.
func withSegments(jpg []byte, segs ...any) []byte {
	out := append([]byte{}, jpg[:2]...)
	for i := 0; i < len(segs); i += 2 {
		payload := segs[i+1].([]byte)
		out = append(out, 0xFF, segs[i].(byte), byte((len(payload)+2)>>8), byte(len(payload)+2))
		out = append(out, payload...)
	}
	return append(out, jpg[2:]...)
}

func TestEXIFOrientation(t *testing.T) {
	jpg := encodeJPEG(t, gradient(16, 8))
	const app0, app1, com = byte(0xE0), byte(0xE1), byte(0xFE)
	jfif := []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	xmp := []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>")

	for _, order := range []byteOrder{binary.LittleEndian, binary.BigEndian} {
		for o := uint32(1); o <= 8; o++ {
			data := withSegments(jpg, app1, exifBlock(order, o, 3))
			info, err := ProbeBytes(data)
			if err != nil || info.Orientation != int(o) {
				t.Errorf("%v orientation %d: got %d, %v", order, o, info.Orientation, err)
			}
			if info.Width != 16 || info.Height != 8 {
				t.Errorf("%v orientation %d: size %dx%d, want it as stored: 16x8", order, o, info.Width, info.Height)
			}
			// Decode and Load report it too, and leave the pixels as stored.
			img, info, err := Decode(data, DefaultBudget)
			if err != nil || info.Orientation != int(o) || img.Bounds().Dx() != 16 {
				t.Errorf("Decode %v orientation %d: %+v, %v", order, o, info, err)
			}
			if info, err = Probe(writeTemp(t, "o.jpg", data)); err != nil || info.Orientation != int(o) {
				t.Errorf("Probe %v orientation %d: %+v, %v", order, o, info, err)
			}
		}
	}

	big := make([]byte, 60000) // a segment to skip, larger than any read buffer
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{"no EXIF", jpg, 1},
		{"after JFIF, a comment and XMP", withSegments(jpg, app0, jfif, com, big, app1, xmp, app1, exifBlock(binary.BigEndian, 6, 3)), 6},
		{"stored as a LONG", withSegments(jpg, app1, exifBlock(binary.LittleEndian, 8, 4)), 8},
		{"stored as a BYTE", withSegments(jpg, app1, exifBlock(binary.BigEndian, 3, 1)), 3},
		{"value 0", withSegments(jpg, app1, exifBlock(binary.LittleEndian, 0, 3)), 1},
		{"value 9", withSegments(jpg, app1, exifBlock(binary.LittleEndian, 9, 3)), 1},
		{"first of two wins", withSegments(jpg, app1, exifBlock(binary.LittleEndian, 5, 3), app1, exifBlock(binary.LittleEndian, 7, 3)), 5},
		{"cut short", withSegments(jpg, app1, exifBlock(binary.LittleEndian, 6, 3)[:20]), 1},
		{"bad byte order", withSegments(jpg, app1, append([]byte("Exif\x00\x00XX"), exifBlock(binary.LittleEndian, 6, 3)[8:]...)), 1},
		{"directory offset beyond the end", withSegments(jpg, app1, []byte("Exif\x00\x00II\x2a\x00\xff\xff\xff\xff")), 1},
		{"not EXIF", withSegments(jpg, app1, []byte("Exiv\x00\x00II")), 1},
	}
	for _, tt := range tests {
		info, err := ProbeBytes(tt.data)
		if err != nil || info.Orientation != tt.want {
			t.Errorf("%s: orientation %d, %v; want %d", tt.name, info.Orientation, err, tt.want)
		}
	}

	// Other formats never have one.
	if info, _ := ProbeBytes(encodePNG(t, gradient(4, 4))); info.Orientation != 1 {
		t.Errorf("png orientation = %d", info.Orientation)
	}
}

// TestHeaderCosts checks the stricter-than-Cost cases on real small files.
func TestHeaderCosts(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int64 // decoded bytes on top of the file; 0 = just Cost
		kind string
	}{
		{"baseline jpeg", encodeJPEG(t, gradient(40, 30)), 0, "jpeg"},
		{"8-bit png", encodePNG(t, gradient(40, 30)), 0, "png"},
		{"16-bit png", encodePNG(t, image.NewRGBA64(image.Rect(0, 0, 40, 30))), 40 * 30 * 8, "16-bit png"},
		{"interlaced 16-bit png header", pngHeader(40, 30, 16, 1), 40 * 30 * 12, "16-bit png"},
		{"lossy webp", fixture(t, lossyWebP), 0, "webp"},
		{"lossless webp", fixture(t, losslessAlphaWebP), 24 * 16 * 8, "webp"},
		{"lossy webp with alpha", fixture(t, lossyAlphaWebP), 24 * 16 * 8, "webp"},
		{"progressive jpeg", fixture(t, progressiveJPEG), 2*6*256 + 32*16 + 2*16*8, "progressive jpeg"},
	}
	for _, tt := range tests {
		h, err := probe(bytes.NewReader(tt.data), int64(len(tt.data)))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		want := Cost(h.Width, h.Height, h.Size, h.Format)
		if tt.want != 0 {
			want = max(want, h.Size+tt.want)
		}
		if h.need != want || h.kind != tt.kind {
			t.Errorf("%s: need %d (%s), want %d (%s)", tt.name, h.need, h.kind, want, tt.kind)
		}
	}
}
