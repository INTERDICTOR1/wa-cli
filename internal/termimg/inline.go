package termimg

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"strconv"
)

// WriteITerm writes an iTerm2 inline-image escape (also understood by
// WezTerm and others):
//
//	ESC ] 1337 ; File = name=<base64>;size=<bytes>;width=<cells>;height=<cells>;
//	                    preserveAspectRatio=1;inline=1 : <the file in base64> BEL
//
// The terminal decodes the file itself (so it must be a format it knows:
// JPEG, PNG, GIF...) and fits it into widthCells x heightCells cells, keeping
// its shape. A width or height of zero or less lets the terminal choose that
// side.
//
// Exactly size bytes are read from r and encoded on the way through, a chunk
// at a time. If r ends early the escape is still closed, so the terminal is
// not left waiting, and an error is returned.
func WriteITerm(w io.Writer, r io.Reader, size int64, name string, widthCells, heightCells int) error {
	if size < 0 {
		return fmt.Errorf("termimg: invalid file size %d", size)
	}
	cells := func(n int) string {
		if n <= 0 {
			return "auto"
		}
		return strconv.Itoa(n)
	}
	bw := bufio.NewWriterSize(w, 32<<10)
	bw.WriteString("\x1b]1337;File=")
	if name != "" {
		bw.WriteString("name=" + base64.StdEncoding.EncodeToString([]byte(name)) + ";")
	}
	fmt.Fprintf(bw, "size=%d;width=%s;height=%s;preserveAspectRatio=1;inline=1:",
		size, cells(widthCells), cells(heightCells))

	// A write error sticks to bw and comes back from Flush, so the
	// results of the writes in between need no checking.
	enc := base64.NewEncoder(base64.StdEncoding, bw)
	n, copyErr := io.CopyBuffer(enc, io.LimitReader(r, size), make([]byte, 24<<10))
	enc.Close()
	bw.WriteByte('\a')
	if err := bw.Flush(); err != nil {
		return err
	}
	if copyErr != nil {
		return copyErr
	}
	if n != size {
		return fmt.Errorf("termimg: the file ended after %d of %d bytes: %w", n, size, io.ErrUnexpectedEOF)
	}
	return nil
}

// WriteKitty writes img using the Kitty graphics protocol (Kitty, Ghostty,
// WezTerm, Konsole): the raw pixels, three bytes each (f=24), or four with
// straight alpha (f=32) when the image may have transparency, in base64,
// split over escapes of at most 4096 characters:
//
//	ESC _G a=T,f=24,s=<width>,v=<height>,q=2,m=1 ; <base64> ESC \
//	ESC _G m=1 ; <base64> ESC \       (as many as needed)
//	ESC _G m=0 ; <base64> ESC \       (the last one)
//
// a=T means transmit and show at the cursor; q=2 tells the terminal not to
// answer, since an answer would arrive as keyboard input. img is shown at
// its own size, so scale it first. The escapes are produced as the rows are
// read, never built in memory whole.
func WriteKitty(w io.Writer, img image.Image) error {
	if img == nil {
		return errEmptyImage
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= 0 || height <= 0 {
		return errEmptyImage
	}
	bpp := 4
	if isOpaque(img) {
		bpp = 3
	}
	k := &kittyWriter{
		bw:   bufio.NewWriterSize(w, 16<<10),
		left: int64(width) * int64(height) * int64(bpp),
	}
	k.bw.WriteString("\x1b_Ga=T,f=" + strconv.Itoa(8*bpp) +
		",s=" + strconv.Itoa(width) + ",v=" + strconv.Itoa(height) + ",q=2,")

	rows := rgbaRows(img)
	out := make([]byte, bpp*width)
	for y := 0; y < height; y++ {
		row := rows(y)
		if bpp == 3 {
			for x := 0; x < width; x++ {
				copy(out[3*x:3*x+3], row[4*x:4*x+3])
			}
		} else {
			// Our rows are premultiplied; Kitty wants the colour as it
			// was before it was multiplied by alpha.
			for x := 0; x < width; x++ {
				p, o := row[4*x:4*x+4:4*x+4], out[4*x:4*x+4:4*x+4]
				switch a := uint32(p[3]); a {
				case 0xFF, 0:
					copy(o, p)
				default:
					o[0] = uint8(min((uint32(p[0])*255+a/2)/a, 255))
					o[1] = uint8(min((uint32(p[1])*255+a/2)/a, 255))
					o[2] = uint8(min((uint32(p[2])*255+a/2)/a, 255))
					o[3] = p[3]
				}
			}
		}
		if err := k.write(out); err != nil {
			return err
		}
	}
	return k.bw.Flush()
}

// kittyWriter cuts a stream of pixel bytes into Kitty's escapes. The first
// one has been started by the caller, up to its last comma.
type kittyWriter struct {
	bw   *bufio.Writer
	left int64      // bytes still to come, so the last chunk can be marked
	raw  [3072]byte // 3072 bytes are exactly 4096 base64 characters
	n    int        // bytes waiting in raw
	b64  [4096]byte
	more bool // an escape has been written already
}

func (k *kittyWriter) write(p []byte) error {
	for len(p) > 0 {
		c := copy(k.raw[k.n:], p)
		p = p[c:]
		k.n += c
		k.left -= int64(c)
		if k.n < len(k.raw) && k.left > 0 {
			break // wait for a full chunk
		}
		if k.more {
			k.bw.WriteString("\x1b_G")
		}
		k.more = true
		if k.left > 0 {
			k.bw.WriteString("m=1;")
		} else {
			k.bw.WriteString("m=0;")
		}
		base64.StdEncoding.Encode(k.b64[:], k.raw[:k.n])
		k.bw.Write(k.b64[:base64.StdEncoding.EncodedLen(k.n)])
		if _, err := k.bw.WriteString("\x1b\\"); err != nil {
			return err
		}
		k.n = 0
	}
	return nil
}
