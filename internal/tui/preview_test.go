package tui

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func solid(w, h int, fill func(x, y int) color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill(x, y))
		}
	}
	return img
}

func TestHalfBlocks_Colours(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	// Top row red, bottom row blue: one text row of "▀" red-on-blue.
	img := solid(4, 2, func(x, y int) color.Color {
		if y == 0 {
			return color.RGBA{255, 0, 0, 255}
		}
		return color.RGBA{0, 0, 255, 255}
	})
	out := halfBlocks(img, previewCols, previewRows)
	if strings.Count(out, "▀") != 4 || strings.Contains(out, "\n") {
		t.Fatalf("want one row of 4 half blocks, got %q", out)
	}
	if !strings.Contains(out, "38;2;255;0;0") || !strings.Contains(out, "48;2;0;0;255") {
		t.Fatalf("want red text on blue background, got %q", out)
	}
}

func TestHalfBlocks_FitsLimits(t *testing.T) {
	grey := func(x, y int) color.Color { return color.Gray{128} }
	for _, size := range [][2]int{{1600, 1200}, {300, 3000}, {5000, 100}, {1, 1}} {
		out := halfBlocks(solid(size[0], size[1], grey), previewCols, previewRows)
		lines := strings.Split(out, "\n")
		if len(lines) > previewRows {
			t.Errorf("%dx%d: %d rows, max %d", size[0], size[1], len(lines), previewRows)
		}
		for _, l := range lines {
			if w := ansi.StringWidth(l); w > previewCols || w == 0 {
				t.Errorf("%dx%d: row width %d, want 1..%d", size[0], size[1], w, previewCols)
			}
		}
	}
}
