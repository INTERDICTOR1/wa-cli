package termimg

import (
	"bytes"
	"image"
	"image/jpeg"
)

// EncodeJPEG encodes img as a JPEG at the given quality (1..100, clamped).
// JPEG has no transparency: transparent pixels come out as they are stored,
// which for the images Scale returns means composited on black.
//
// *image.RGBA (what Scale returns), *image.YCbCr and *image.Gray are encoded
// straight from their buffers; other types pixel by pixel, which is slow for
// big pictures.
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	if img == nil {
		return nil, errEmptyImage
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, errEmptyImage
	}
	var buf bytes.Buffer
	// Start near the usual size (a photo is about a bit per pixel at
	// ordinary quality) so the buffer doesn't double its way up from 64
	// bytes; a wrong guess only costs a regrow.
	buf.Grow(min(b.Dx()*b.Dy()/8+1024, 4<<20))
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: min(max(quality, 1), 100)}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Thumbnail returns a small JPEG of img whose longest side is at most
// maxSide, for embedding in a WhatsApp message (typical: maxSide 100,
// quality 50). A picture that is already small enough keeps its size.
func Thumbnail(img image.Image, maxSide, quality int) ([]byte, error) {
	if img == nil {
		return nil, errEmptyImage
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, errEmptyImage
	}
	w, h := Fit(b.Dx(), b.Dy(), maxSide, maxSide)
	return EncodeJPEG(Scale(img, w, h), quality)
}
