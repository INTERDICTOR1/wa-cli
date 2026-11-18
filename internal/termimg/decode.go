// Package termimg shows photos in a terminal and prepares images for
// sending, without ever holding more than a known amount of memory.
//
// The usual way to show a file:
//
//	img, info, err := termimg.Load(path, termimg.DefaultBudget)
//	img = termimg.Orient(img, info.Orientation) // free: a view, no copy
//	b := img.Bounds()
//	w, h := termimg.Fit(b.Dx(), b.Dy(), maxW, maxH)
//	small := termimg.Scale(img, w, h) // the only allocation that grows with the screen
//	img = nil                         // let the decoded pixels go
//	err = termimg.WriteSixel(out, small, 256)
//
// The package depends on nothing but the standard library and
// golang.org/x/image, and knows nothing about the rest of the app.
package termimg

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // register the decoders image.Decode chooses from
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// DefaultBudget is how many bytes one decode may cost. The whole app must
// stay under 100 MB and idles at 30-40 MB, so this is what is left for a
// picture.
const DefaultBudget = 48 << 20

// ErrTooLarge is returned (wrapped) by Load and Decode for images that
// can't be decoded within the budget. Nothing was decoded.
var ErrTooLarge = errors.New("image too large to decode within the memory budget")

// Info is what an image file says about itself without decoding pixels.
type Info struct {
	Width, Height int    // as stored (before applying Orientation)
	Format        string // "jpeg", "png", "gif", "webp", "bmp"
	Size          int64  // file size in bytes
	Orientation   int    // EXIF orientation 1..8; 1 when absent or not JPEG
}

// Cost estimates the bytes needed to hold the file plus its decoded pixels
// (JPEG: 3 bytes/pixel, everything else: 4).
//
// It is a rule of thumb for deciding from a message's metadata whether a
// download is worth it. Load and Decode check again from the real header,
// which can reveal a costlier file (see Load).
func Cost(w, h int, fileSize int64, format string) int64 {
	bpp := int64(4)
	if format == "jpeg" {
		bpp = 3
	}
	return satAdd(max(fileSize, 0), pixelBytes(w, h, bpp))
}

// pixelBytes is w*h*bpp, or the largest int64 if that doesn't fit: headers
// can claim absurd sizes, and those must come out as "too large", not as a
// small number that wrapped around.
func pixelBytes(w, h int, bpp int64) int64 {
	if w <= 0 || h <= 0 {
		return 0
	}
	if w > math.MaxInt32 || h > math.MaxInt32 {
		return math.MaxInt64
	}
	px := int64(w) * int64(h) // fits: both are below 2^31
	if px > math.MaxInt64/bpp {
		return math.MaxInt64
	}
	return px * bpp
}

func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Probe reads only the headers of the image file at path (plus the EXIF
// orientation for a JPEG).
func Probe(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	h, err := probe(f, st.Size())
	return h.Info, err
}

// ProbeBytes is Probe for a file already in memory.
func ProbeBytes(data []byte) (Info, error) {
	h, err := probe(bytes.NewReader(data), int64(len(data)))
	return h.Info, err
}

// Load decodes the image file at path if it fits the budget, else returns
// ErrTooLarge (wrapped) without decoding. The file is streamed from disk,
// never held in memory whole. Info is filled in whenever the header could be
// read, also when the image is too large.
//
// The pixels come back as stored; call Orient with info.Orientation to get
// an upright image. For a GIF it is the first frame.
//
// The budget is checked against Cost, and against what the header shows the
// decoder will really use when that is more:
//
//   - a progressive JPEG: 7.5 bytes/pixel (15 without chroma subsampling),
//     because the decoder keeps every coefficient until the last scan
//   - a CMYK or RGB JPEG: 4 more bytes/pixel for the conversion
//   - a PNG with 16 bits per channel: 8 bytes/pixel; an interlaced one: half
//     as much again
//   - a WebP that is lossless or has transparency: 8 bytes/pixel
//
// A budget of zero or less means DefaultBudget.
func Load(path string, budget int64) (image.Image, Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, Info{}, err
	}
	return decode(f, st.Size(), budget)
}

// Decode is Load for a file already in memory. data itself counts towards
// the budget, since the caller is holding it.
func Decode(data []byte, budget int64) (image.Image, Info, error) {
	return decode(bytes.NewReader(data), int64(len(data)), budget)
}

func decode(r io.ReadSeeker, size, budget int64) (image.Image, Info, error) {
	h, err := probe(r, size)
	if err != nil {
		return nil, Info{}, err
	}
	if budget <= 0 {
		budget = DefaultBudget
	}
	if h.need > budget {
		return nil, h.Info, fmt.Errorf("%dx%d %s needs about %d MB to decode, the budget is %d MB: %w",
			h.Width, h.Height, h.kind, mb(h.need), mb(budget), ErrTooLarge)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, h.Info, err
	}
	img, _, err := image.Decode(bufio.NewReaderSize(r, 64<<10))
	if err != nil {
		return nil, h.Info, fmt.Errorf("decoding %s image: %w", h.Format, err)
	}
	return img, h.Info, nil
}

// mb rounds a byte count up to whole megabytes, for messages.
func mb(n int64) int64 { return n>>20 + min(n&(1<<20-1), 1) }

// header is what probe learns from the start of a file: the public Info,
// plus what decoding it will really cost.
type header struct {
	Info
	need int64  // bytes to hold the file and decode it: Cost, or more
	kind string // the format in words, for messages: "progressive jpeg"
}

// probe reads the headers of the image in r, which is size bytes long.
func probe(r io.ReadSeeker, size int64) (header, error) {
	cfg, format, err := image.DecodeConfig(bufio.NewReader(r))
	if err != nil {
		return header{}, fmt.Errorf("reading image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return header{}, errors.New("reading image header: the image has no pixels")
	}
	h := header{
		Info: Info{Width: cfg.Width, Height: cfg.Height, Format: format, Size: size, Orientation: 1},
		need: Cost(cfg.Width, cfg.Height, size, format),
		kind: format,
	}

	// decoded is what the decoder will allocate, where the header shows
	// it is more than Cost's rule of thumb.
	var decoded int64
	switch format {
	case "jpeg":
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return header{}, err
		}
		j := scanJPEG(bufio.NewReader(r))
		h.Orientation = j.orientation
		decoded = j.decodedBytes()
		if j.progressive {
			h.kind = "progressive jpeg"
		}
	case "png":
		// Fixed layout: 8 bytes of signature, then the IHDR chunk with the
		// bit depth at byte 24 and the interlace method at byte 28.
		var b [29]byte
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return header{}, err
		}
		if _, err := io.ReadFull(r, b[:]); err == nil {
			bpp := int64(4)
			if b[24] == 16 {
				bpp = 8
				h.kind = "16-bit png"
			}
			decoded = pixelBytes(cfg.Width, cfg.Height, bpp)
			if b[28] != 0 {
				// Each interlace pass is decoded into its own image
				// and merged; the last pass is half the picture.
				decoded = satAdd(decoded, decoded/2)
			}
		}
	case "webp":
		// A lossless WebP may be decoded into one buffer and transformed
		// into a second; a lossy one with transparency decodes its alpha
		// as a whole lossless image first.
		if cfg.ColorModel != color.YCbCrModel {
			decoded = pixelBytes(cfg.Width, cfg.Height, 8)
		}
	}
	h.need = max(h.need, satAdd(max(size, 0), decoded))
	return h, nil
}
