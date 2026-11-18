package termimg

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// parseITerm takes an iTerm2 inline-image escape apart: its arguments, and
// the file it carries.
func parseITerm(t *testing.T, out []byte) (args map[string]string, order []string, file []byte) {
	t.Helper()
	s := string(out)
	rest, ok := strings.CutPrefix(s, "\x1b]1337;File=")
	if !ok {
		t.Fatalf("starts with %q, want ESC ] 1337 ; File =", s[:min(len(s), 20)])
	}
	rest, ok = strings.CutSuffix(rest, "\a")
	if !ok {
		t.Fatalf("ends with %q, want BEL", s[max(len(s)-10, 0):])
	}
	head, payload, ok := strings.Cut(rest, ":")
	if !ok {
		t.Fatal("no colon between the arguments and the file")
	}
	if strings.ContainsAny(rest, "\x1b\a\n") {
		t.Fatal("a control character inside the escape")
	}
	args = map[string]string{}
	for _, kv := range strings.Split(head, ";") {
		k, v, ok := strings.Cut(kv, "=")
		if _, dup := args[k]; !ok || dup {
			t.Fatalf("bad argument %q in %q", kv, head)
		}
		args[k] = v
		order = append(order, k)
	}
	file, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	return args, order, file
}

func TestWriteITerm(t *testing.T) {
	n := noise(99)
	random := func(size int) []byte {
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(n.next())
		}
		return b
	}
	tests := []struct {
		name          string
		file          []byte
		fileName      string
		width, height int
		wantW, wantH  string
	}{
		{"a JPEG", encodeJPEG(t, gradient(40, 30)), "photo.jpg", 80, 24, "80", "24"},
		{"empty file", nil, "nothing", 10, 5, "10", "5"},
		{"1 byte", random(1), "a", 10, 5, "10", "5"},
		{"2 bytes", random(2), "a", 10, 5, "10", "5"}, // every amount of base64 padding
		{"3 bytes", random(3), "a", 10, 5, "10", "5"},
		{"more than any buffer", random(300<<10 + 1), "big.bin", 200, 60, "200", "60"},
		{"no name", random(100), "", 10, 5, "10", "5"},
		{"a name that needs encoding", random(100), "Señor;Q=1:π.jpg", 10, 5, "10", "5"},
		{"width only", random(100), "x", 40, 0, "40", "auto"},
		{"height only", random(100), "x", -1, 12, "auto", "12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			// More in the reader than size: only size bytes belong to
			// the file. And a reader that returns less than asked for,
			// as files and sockets may.
			src := iotest.HalfReader(io.MultiReader(bytes.NewReader(tt.file), strings.NewReader("TRAILING")))
			if err := WriteITerm(&buf, src, int64(len(tt.file)), tt.fileName, tt.width, tt.height); err != nil {
				t.Fatal(err)
			}
			args, order, file := parseITerm(t, buf.Bytes())
			if !bytes.Equal(file, tt.file) {
				t.Errorf("payload is %d bytes, want the %d bytes of the file", len(file), len(tt.file))
			}
			want := map[string]string{
				"size": strconv.Itoa(len(tt.file)), "width": tt.wantW, "height": tt.wantH,
				"preserveAspectRatio": "1", "inline": "1",
			}
			if tt.fileName != "" {
				want["name"] = base64.StdEncoding.EncodeToString([]byte(tt.fileName))
			}
			if len(args) != len(want) {
				t.Errorf("arguments %v, want %v", order, want)
			}
			for k, v := range want {
				if args[k] != v {
					t.Errorf("%s=%q, want %q", k, args[k], v)
				}
			}
		})
	}
}

func TestWriteITermErrors(t *testing.T) {
	// The file ends early: an error, but the escape is closed properly so
	// the terminal isn't left swallowing everything that follows.
	var buf bytes.Buffer
	err := WriteITerm(&buf, strings.NewReader("only this"), 100, "x", 10, 5)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short file: err = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, _, file := parseITerm(t, buf.Bytes()); string(file) != "only this" {
		t.Errorf("short file: payload %q", file)
	}

	// The reader fails.
	buf.Reset()
	boom := errors.New("disk on fire")
	err = WriteITerm(&buf, io.MultiReader(strings.NewReader("abc"), errReader{boom}), 100, "x", 10, 5)
	if !errors.Is(err, boom) {
		t.Errorf("failing reader: err = %v", err)
	}
	parseITerm(t, buf.Bytes()) // still well-formed

	// The writer fails.
	if err := WriteITerm(&failingWriter{}, bytes.NewReader(make([]byte, 100<<10)), 100<<10, "x", 10, 5); !errors.Is(err, errWriteFailed) {
		t.Errorf("failing writer: err = %v", err)
	}
	if err := WriteITerm(&buf, strings.NewReader(""), -1, "x", 10, 5); err == nil {
		t.Error("negative size: no error")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// kittyImage is what a Kitty graphics transmission says and carries.
type kittyImage struct {
	keys   map[string]string // the control keys of the first escape
	data   []byte            // the pixel bytes, all chunks joined
	chunks int
}

// parseKitty takes a chunked Kitty graphics transmission apart, checking
// the protocol's rules for chunks: each escape is ESC _G <keys> ; <base64>
// ESC \, with at most 4096 characters of base64, a multiple of four in all
// but the last; every escape but the last says m=1, the last m=0; and only
// the first carries other keys.
func parseKitty(t *testing.T, out []byte) kittyImage {
	t.Helper()
	img := kittyImage{keys: map[string]string{}}
	rest := string(out)
	last := false
	for rest != "" {
		if last {
			t.Fatal("data after the chunk marked as last (m=0)")
		}
		body, after, ok := strings.Cut(rest, "\x1b\\")
		if !ok || !strings.HasPrefix(body, "\x1b_G") {
			t.Fatalf("chunk %d is not ESC _G ... ESC \\: %q", img.chunks, rest[:min(len(rest), 30)])
		}
		rest = after
		control, payload, ok := strings.Cut(body[3:], ";")
		if !ok {
			t.Fatalf("chunk %d has no payload", img.chunks)
		}
		keys := map[string]string{}
		for _, kv := range strings.Split(control, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if _, dup := keys[k]; !ok || dup {
				t.Fatalf("chunk %d: bad key %q in %q", img.chunks, kv, control)
			}
			keys[k] = v
		}
		switch keys["m"] {
		case "1":
			if len(payload)%4 != 0 {
				t.Fatalf("chunk %d: %d characters, not a multiple of four, but more follow", img.chunks, len(payload))
			}
		case "0":
			last = true
		default:
			t.Fatalf("chunk %d: m=%q", img.chunks, keys["m"])
		}
		if len(payload) > 4096 || len(payload) == 0 {
			t.Fatalf("chunk %d: %d characters of payload, want 1..4096", img.chunks, len(payload))
		}
		if img.chunks == 0 {
			img.keys = keys
		} else if len(keys) != 1 {
			t.Fatalf("chunk %d: keys %q, want only m", img.chunks, control)
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatalf("chunk %d: payload is not base64 on its own: %v", img.chunks, err)
		}
		img.data = append(img.data, data...)
		img.chunks++
	}
	if !last {
		t.Fatal("no chunk marked as last (m=0)")
	}
	return img
}

func TestWriteKitty(t *testing.T) {
	translucent := image.NewNRGBA(image.Rect(0, 0, 33, 21))
	for y := 0; y < 21; y++ {
		for x := 0; x < 33; x++ {
			translucent.SetNRGBA(x, y, color.NRGBA{uint8(x * 7), uint8(y * 12), 200, [4]uint8{255, 0, 128, 64}[(x+y)%4]})
		}
	}
	big := gradient(400, 300)
	tests := []struct {
		name       string
		img        image.Image
		format     string // f=: bits per pixel
		wantChunks int
	}{
		{"opaque RGBA", gradient(50, 40), "24", 2},
		{"one pixel", gradient(1, 1), "24", 1},
		{"exactly one chunk", gradient(32, 32), "24", 1},          // 3072 bytes
		{"one byte more than a chunk", gradient(41, 25), "24", 2}, // 3075 bytes
		{"exactly two chunks", gradient(64, 32), "24", 2},
		{"a row longer than a chunk", gradient(2000, 3), "24", 6},
		{"with transparency", translucent, "32", 1},
		{"a JPEG's pixels", photoYCbCr(64, 48, image.YCbCrSubsampleRatio420), "24", 3},
		{"grey", image.NewGray(image.Rect(0, 0, 10, 10)), "24", 1},
		{"window on a larger image", big.SubImage(image.Rect(10, 20, 110, 80)), "24", 6},
		{"turned on its side", Orient(gradient(50, 40), 6), "24", 2},
		{"unknown kind of image", atOnly{gradient(8, 8)}, "32", 1}, // can't know it is opaque
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteKitty(&buf, tt.img); err != nil {
				t.Fatal(err)
			}
			got := parseKitty(t, buf.Bytes())
			b := tt.img.Bounds()
			want := map[string]string{
				"a": "T", "f": tt.format, "s": strconv.Itoa(b.Dx()), "v": strconv.Itoa(b.Dy()), "q": "2", "m": got.keys["m"],
			}
			if len(got.keys) != len(want) {
				t.Errorf("keys %v, want %v", got.keys, want)
			}
			for k, v := range want {
				if got.keys[k] != v {
					t.Errorf("%s=%q, want %q", k, got.keys[k], v)
				}
			}
			if got.chunks != tt.wantChunks {
				t.Errorf("%d chunks, want %d", got.chunks, tt.wantChunks)
			}
			bpp := 3
			if tt.format == "32" {
				bpp = 4
			}
			if len(got.data) != b.Dx()*b.Dy()*bpp {
				t.Fatalf("%d bytes of pixels, want %d", len(got.data), b.Dx()*b.Dy()*bpp)
			}
			for y := 0; y < b.Dy(); y++ {
				for x := 0; x < b.Dx(); x++ {
					p := got.data[(y*b.Dx()+x)*bpp:]
					if bpp == 3 {
						// Opaque: exactly the pixel.
						if c, want := (color.RGBA{p[0], p[1], p[2], 255}), rgbaAt(tt.img, x, y); c != want {
							t.Fatalf("pixel %d,%d = %v, want %v", x, y, c, want)
						}
						continue
					}
					// With alpha, Kitty wants the colour not multiplied
					// by it: compare with the image's own such values.
					// Getting there through premultiplied 8-bit numbers
					// loses precision as alpha gets small.
					want := color.NRGBAModel.Convert(tt.img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
					if p[3] != want.A {
						t.Fatalf("pixel %d,%d: alpha %d, want %d", x, y, p[3], want.A)
					}
					tol := 0
					if want.A != 255 && want.A != 0 {
						tol = 255/int(want.A) + 1
					}
					if want.A == 0 {
						want = color.NRGBA{} // a transparent pixel has no colour
					}
					if d := maxDiff(color.RGBA{p[0], p[1], p[2], p[3]}, color.RGBA(want)); d > tol {
						t.Fatalf("pixel %d,%d = %v, want %v within %d", x, y, p[:4], want, tol)
					}
				}
			}
		})
	}
}

func TestWriteKittyErrors(t *testing.T) {
	for _, img := range []image.Image{nil, image.NewRGBA(image.Rect(0, 0, 4, 0))} {
		var buf bytes.Buffer
		if err := WriteKitty(&buf, img); err == nil || buf.Len() != 0 {
			t.Errorf("WriteKitty(%v) = %v with %d bytes written, want an error and nothing", img, err, buf.Len())
		}
	}
	for _, after := range []int{0, 1, 4} {
		if err := WriteKitty(&failingWriter{after: after}, gradient(400, 300)); !errors.Is(err, errWriteFailed) {
			t.Errorf("writer failing after %d writes: err = %v", after, err)
		}
	}
	// Streaming: the picture arrives in pieces, and what is allocated is
	// a few buffers, nowhere near the 4 MB of pixels (5.3 MB of base64).
	var out chunkCounter
	img := gradient(1400, 1000)
	spent := allocatedBytes(func() {
		if err := WriteKitty(&out, img); err != nil {
			t.Fatal(err)
		}
	})
	if out.largest > 64<<10 || out.bytes < 5<<20 {
		t.Errorf("%d bytes arrived in %d writes, the largest %d", out.bytes, out.writes, out.largest)
	}
	if spent > 128<<10 {
		t.Errorf("WriteKitty allocated %d bytes for %d bytes of output", spent, out.bytes)
	}
}
