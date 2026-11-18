package termimg

import (
	"image"
	"image/color"
	"strconv"
	"strings"
	"testing"
)

// blockCell is one terminal cell of a half-block picture: the colour of its
// top pixel (the text colour) and of its bottom pixel (the background).
type blockCell struct{ top, bottom color.RGBA }

// parseBlocks reads HalfBlocks output back the way a terminal would. It
// fails the test on anything but 24-bit colour sequences, "▀" and the reset
// at the end of each line; on a cell drawn before both colours are set; and
// on a sequence that sets a colour to what it already is (colours are to be
// sent only when they change). It returns the cells line by line, and how
// many colour sequences there were.
func parseBlocks(t *testing.T, s string) (lines [][]blockCell, sequences int) {
	t.Helper()
	for n, line := range strings.Split(s, "\n") {
		rest, ok := strings.CutSuffix(line, "\x1b[0m")
		if !ok {
			t.Fatalf("line %d doesn't end with a reset: %q", n, line)
		}
		var cells []blockCell
		var fg, bg *color.RGBA
		for rest != "" {
			if after, ok := strings.CutPrefix(rest, "▀"); ok {
				if fg == nil || bg == nil {
					t.Fatalf("line %d: a cell before both colours are set", n)
				}
				cells = append(cells, blockCell{*fg, *bg})
				rest = after
				continue
			}
			after, ok := strings.CutPrefix(rest, "\x1b[")
			end := strings.IndexByte(after, 'm')
			if !ok || end < 0 {
				t.Fatalf("line %d: unexpected %q", n, rest[:min(len(rest), 20)])
			}
			sequences++
			var params []int
			for _, p := range strings.Split(after[:end], ";") {
				v, err := strconv.Atoi(p)
				if err != nil || v < 0 || v > 255 {
					t.Fatalf("line %d: bad sequence %q", n, after[:end])
				}
				params = append(params, v)
			}
			if len(params) == 0 || len(params)%5 != 0 {
				t.Fatalf("line %d: bad sequence %q", n, after[:end])
			}
			for ; len(params) > 0; params = params[5:] {
				c := color.RGBA{uint8(params[2]), uint8(params[3]), uint8(params[4]), 255}
				var which **color.RGBA
				switch {
				case params[0] == 38 && params[1] == 2:
					which = &fg
				case params[0] == 48 && params[1] == 2:
					which = &bg
				default:
					t.Fatalf("line %d: bad sequence %q", n, after[:end])
				}
				if *which != nil && **which == c {
					t.Fatalf("line %d: colour %v set again though it didn't change", n, c)
				}
				*which = &c
			}
			rest = after[end+1:]
		}
		lines = append(lines, cells)
	}
	return lines, sequences
}

func TestHalfBlocksShape(t *testing.T) {
	tests := []struct {
		name       string
		img        image.Image
		cols, rows int // the space offered
		wantCols   int // the cells used
		wantRows   int
	}{
		{"wide", gradient(400, 100), 40, 20, 40, 5},
		{"tall", gradient(100, 400), 40, 20, 10, 20},
		{"square", gradient(300, 300), 80, 24, 48, 24},
		{"square in a narrow space", gradient(300, 300), 10, 24, 10, 5},
		{"fits exactly", gradient(64, 24), 32, 12, 32, 6},
		{"exactly the pixels offered", gradient(32, 24), 32, 12, 32, 12},
		{"small: not enlarged", gradient(16, 16), 32, 12, 16, 8},
		{"odd height: last cell filled", gradient(10, 5), 32, 12, 10, 3},
		{"one pixel", gradient(1, 1), 10, 10, 1, 1},
		{"one row of pixels", gradient(50, 1), 10, 10, 10, 1},
		{"one column of pixels", gradient(1, 50), 10, 10, 1, 10},
		{"one cell offered", gradient(300, 200), 1, 1, 1, 1},
		{"a JPEG's pixels", photoYCbCr(800, 600, image.YCbCrSubsampleRatio420), 200, 60, 160, 60},
		{"turned on its side", Orient(gradient(400, 100), 6), 40, 20, 10, 20},
		{"window on a larger image", gradient(400, 300).SubImage(image.Rect(100, 100, 300, 200)), 50, 50, 50, 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := HalfBlocks(tt.img, tt.cols, tt.rows)
			if strings.HasSuffix(out, "\n") || strings.HasPrefix(out, "\n") {
				t.Error("output starts or ends with a newline")
			}
			lines, _ := parseBlocks(t, out)
			if len(lines) != tt.wantRows {
				t.Fatalf("%d lines, want %d", len(lines), tt.wantRows)
			}
			// The cells are exactly the picture scaled to two pixels
			// per cell.
			want := Scale(tt.img, tt.wantCols, 2*tt.wantRows)
			for y, cells := range lines {
				if len(cells) != tt.wantCols {
					t.Fatalf("line %d is %d cells wide, want %d", y, len(cells), tt.wantCols)
				}
				for x, c := range cells {
					top, bottom := want.RGBAAt(x, 2*y), want.RGBAAt(x, 2*y+1)
					top.A, bottom.A = 255, 255
					if c.top != top || c.bottom != bottom {
						t.Fatalf("cell %d,%d = %v over %v, want %v over %v", x, y, c.top, c.bottom, top, bottom)
					}
				}
			}
		})
	}
}

func TestHalfBlocksEmpty(t *testing.T) {
	img := gradient(10, 10)
	for name, got := range map[string]string{
		"nil image":     HalfBlocks(nil, 10, 10),
		"no pixels":     HalfBlocks(image.NewRGBA(image.Rect(5, 5, 5, 9)), 10, 10),
		"no columns":    HalfBlocks(img, 0, 10),
		"no rows":       HalfBlocks(img, 10, 0),
		"negative size": HalfBlocks(img, -1, -1),
	} {
		if got != "" {
			t.Errorf("%s: %q, want nothing", name, got)
		}
	}
}

func TestHalfBlocksColours(t *testing.T) {
	// One flat colour: a single sequence per line sets both colours.
	flat := image.NewRGBA(image.Rect(0, 0, 3, 4))
	for i := 0; i < len(flat.Pix); i += 4 {
		copy(flat.Pix[i:], []byte{255, 128, 0, 255})
	}
	line := "\x1b[38;2;255;128;0;48;2;255;128;0m▀▀▀\x1b[0m"
	if got, want := HalfBlocks(flat, 80, 24), line+"\n"+line; got != want {
		t.Errorf("flat image:\n%q\nwant\n%q", got, want)
	}

	// Columns of colour over a flat bottom: only the text colour changes,
	// and only where the column changes.
	img := image.NewRGBA(image.Rect(0, 0, 6, 2))
	for x := 0; x < 6; x++ {
		img.SetRGBA(x, 0, color.RGBA{uint8(x / 2 * 100), 7, 9, 255})
		img.SetRGBA(x, 1, color.RGBA{1, 2, 3, 255})
	}
	want := "\x1b[38;2;0;7;9;48;2;1;2;3m▀▀" + "\x1b[38;2;100;7;9m▀▀" + "\x1b[38;2;200;7;9m▀▀" + "\x1b[0m"
	if got := HalfBlocks(img, 80, 24); got != want {
		t.Errorf("columns:\n%q\nwant\n%q", got, want)
	}

	// And the other way round: only the background changes.
	for x := 0; x < 6; x++ {
		img.SetRGBA(x, 1, img.RGBAAt(x, 0))
		img.SetRGBA(x, 0, color.RGBA{1, 2, 3, 255})
	}
	want = "\x1b[38;2;1;2;3;48;2;0;7;9m▀▀" + "\x1b[48;2;100;7;9m▀▀" + "\x1b[48;2;200;7;9m▀▀" + "\x1b[0m"
	if got := HalfBlocks(img, 80, 24); got != want {
		t.Errorf("rows:\n%q\nwant\n%q", got, want)
	}

	// A busy picture: as many sequences as there are changes of colour,
	// counting the start of each line. parseBlocks has already refused
	// sequences that change nothing; this refuses changes that are missing.
	busy := photo(40, 30)
	lines, sequences := parseBlocks(t, HalfBlocks(busy, 40, 15))
	changes := 0
	for _, cells := range lines {
		for x, c := range cells {
			if x == 0 || c != cells[x-1] {
				changes++
			}
		}
	}
	if sequences != changes {
		t.Errorf("%d colour sequences for %d changes of colour", sequences, changes)
	}

	// Transparent pixels are black; half-transparent ones darker.
	n := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	n.SetNRGBA(0, 0, color.NRGBA{255, 255, 255, 0})
	n.SetNRGBA(1, 0, color.NRGBA{255, 255, 255, 128})
	n.SetNRGBA(0, 1, color.NRGBA{200, 100, 50, 255})
	n.SetNRGBA(1, 1, color.NRGBA{200, 100, 50, 0})
	lines, _ = parseBlocks(t, HalfBlocks(n, 10, 10))
	got := lines[0]
	black := color.RGBA{0, 0, 0, 255}
	if got[0].top != black || got[1].bottom != black || got[0].bottom != (color.RGBA{200, 100, 50, 255}) || got[1].top != (color.RGBA{128, 128, 128, 255}) {
		t.Errorf("transparency: %v", got)
	}
}
