package termimg

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/image/bmp"
)

// Tiny real files for the formats Go can't write: a 24x16 gradient saved
// with Pillow as a progressive JPEG and as three kinds of WebP. The two
// "alpha" ones are transparent in their left half.
const (
	progressiveJPEG   = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAA0JCgsKCA0LCgsODg0PEyAVExISEyccHhcgLikxMC4pLSwzOko+MzZGNywtQFdBRkxOUlNSMj5aYVpQYEpRUk//2wBDAQ4ODhMREyYVFSZPNS01T09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT09PT0//wgARCAAQABgDASIAAhEBAxEB/8QAFgABAQEAAAAAAAAAAAAAAAAABQAE/8QAFgEBAQEAAAAAAAAAAAAAAAAAAwIF/9oADAMBAAIQAxAAAAErQjooDpmDW//EABYQAAMAAAAAAAAAAAAAAAAAAAABA//aAAgBAQABBQJTFMUxTFMUz//EABgRAAIDAAAAAAAAAAAAAAAAAAAEAgMh/9oACAEDAQE/AaGSDOH/xAAXEQADAQAAAAAAAAAAAAAAAAAAAQMC/9oACAECAQE/AdXHc//EABQQAQAAAAAAAAAAAAAAAAAAACD/2gAIAQEABj8CX//EABUQAQEAAAAAAAAAAAAAAAAAAABh/9oACAEBAAE/IZpppppv/9oADAMBAAIAAwAAABDH3//EABcRAQADAAAAAAAAAAAAAAAAAAAhMVH/2gAIAQMBAT8Qplvf/8QAFxEBAAMAAAAAAAAAAAAAAAAAACExYf/aAAgBAgEBPxDdLb//xAAWEAEBAQAAAAAAAAAAAAAAAAAQMQD/2gAIAQEAAT8QJJKSWf/Z"
	lossyWebP         = "UklGRm4AAABXRUJQVlA4IGIAAABwBACdASoYABAAPp1KnEmlpKKhMAgAsBOJbACdMoR3N6SAYSZTe4VuA5sOAAD+9uKQg5uSN92J1to7z+5MVMIK3tUJ9dhqA7Ba8f8oP0mIr9XsY6aUGTZ+qLYWOMxjL6HAAA=="
	losslessAlphaWebP = "UklGRj4AAABXRUJQVlA4TDIAAAAvF8ADELkyRPQ/dhHR/wCRtk0t3L/f4UMyaZv8ne35HkABQZH/o00AguoX0YA0QL0TAA=="
	lossyAlphaWebP    = "UklGRpwAAABXRUJQVlA4WAoAAAAQAAAAFwAADwAAQUxQSBQAAAABDzD/ERFCSED4v9YD3YKI/gc2A1ZQOCBiAAAAMAQAnQEqGAAQAD6dSpxJpaSioTAIALATiWoAnTKEcDegAThJoDxDEyLAAP7+4mvCP71mFzPqP/grIOCMUnf//rfeXjXWYDY62w45dfp3cvkn7OrRJoPT6iVgKI7jplgAAAA="
)

func fixture(t testing.TB, b64 string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// gradient is a smooth w x h picture: red grows to the right, green
// downwards, blue along the diagonal.
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / max(w-1, 1)),
				G: uint8(y * 255 / max(h-1, 1)),
				B: uint8((x + y) * 255 / max(w+h-2, 1)),
				A: 0xFF,
			})
		}
	}
	return img
}

// noise is a small fast random number generator, so the test pictures are
// the same on every run.
type noise uint32

func (n *noise) next() uint32 {
	*n ^= *n << 13
	*n ^= *n >> 17
	*n ^= *n << 5
	return uint32(*n)
}

// photo is a picture with the statistics of a photograph, more or less:
// smooth gradients with noise on top, so there are far more colours than a
// palette holds and no long runs of equal pixels.
func photo(w, h int) *image.RGBA {
	img := gradient(w, h)
	n := noise(2463534242)
	for i := 0; i < len(img.Pix); i += 4 {
		r := n.next()
		for c := 0; c < 3; c++ {
			v := int(img.Pix[i+c]) + int(r>>(8*c)&0x1F) - 16
			img.Pix[i+c] = uint8(min(max(v, 0), 255))
		}
	}
	return img
}

// photoYCbCr is photo in the form a JPEG decodes to, built without ever
// holding an RGBA image of that size.
func photoYCbCr(w, h int, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), ratio)
	n := noise(88172645)
	for y := 0; y < h; y++ {
		row := img.Y[y*img.YStride : y*img.YStride+w]
		for x := range row {
			v := 32 + (x+y)*180/(w+h) + int(n.next()&0x1F)
			row[x] = uint8(v)
		}
	}
	cw, ch := img.CStride, len(img.Cb)/img.CStride
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			img.Cb[y*cw+x] = uint8(64 + x*128/cw)
			img.Cr[y*cw+x] = uint8(64 + y*128/ch)
		}
	}
	return img
}

func encodeJPEG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodePNG(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeGIF(t testing.TB, frames ...*image.Paletted) []byte {
	t.Helper()
	var buf bytes.Buffer
	g := &gif.GIF{Image: frames, Delay: make([]int, len(frames))}
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeBMP(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTemp(t testing.TB, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rgbaAt returns pixel (x, y) of img, counted from the top left of its
// bounds, as 8-bit premultiplied RGBA.
func rgbaAt(img image.Image, x, y int) color.RGBA {
	b := img.Bounds()
	r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8)}
}

// maxDiff is the largest difference in any channel between two colours.
func maxDiff(a, b color.RGBA) int {
	d := func(x, y uint8) int { return max(int(x)-int(y), int(y)-int(x)) }
	return max(d(a.R, b.R), d(a.G, b.G), d(a.B, b.B), d(a.A, b.A))
}

// atOnly hides everything about an image but At, to exercise the slowest
// path of the pixel readers.
type atOnly struct{ img image.Image }

func (a atOnly) ColorModel() color.Model { return a.img.ColorModel() }
func (a atOnly) Bounds() image.Rectangle { return a.img.Bounds() }
func (a atOnly) At(x, y int) color.Color { return a.img.At(x, y) }

// deepGradient is an opaque picture with 16 bits per channel, using values
// that 8 bits can't hold.
func deepGradient(w, h int) *image.RGBA64 {
	img := image.NewRGBA64(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA64(x, y, color.RGBA64{
				R: uint16(x * 65535 / max(w-1, 1)),
				G: uint16(y * 65535 / max(h-1, 1)),
				B: 0x1234,
				A: 0xFFFF,
			})
		}
	}
	return img
}

func bufioReader(data []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(data)) }

// allocatedBytes is how many bytes f allocates in total, garbage included.
func allocatedBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
