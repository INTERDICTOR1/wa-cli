package termimg

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestEncodeJPEG(t *testing.T) {
	src := gradient(80, 60)
	var sizes []int
	for _, quality := range []int{-5, 1, 50, 90, 100, 1000} {
		data, err := EncodeJPEG(src, quality)
		if err != nil {
			t.Fatalf("quality %d: %v", quality, err)
		}
		sizes = append(sizes, len(data))
		info, err := ProbeBytes(data)
		if err != nil || info.Format != "jpeg" || info.Width != 80 || info.Height != 60 {
			t.Fatalf("quality %d: probes as %+v, %v", quality, info, err)
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("quality %d: %v", quality, err)
		}
		// At a decent quality the picture survives.
		if quality >= 90 && quality <= 100 {
			for _, p := range []image.Point{{5, 5}, {70, 10}, {10, 50}, {75, 55}, {40, 30}} {
				if d := maxDiff(rgbaAt(img, p.X, p.Y), src.RGBAAt(p.X, p.Y)); d > 12 {
					t.Errorf("quality %d: pixel %v = %v, want %v", quality, p, rgbaAt(img, p.X, p.Y), src.RGBAAt(p.X, p.Y))
				}
			}
		}
	}
	// Out-of-range qualities are clamped, not refused; and more quality
	// is more bytes.
	if sizes[0] != sizes[1] || sizes[4] != sizes[5] {
		t.Errorf("sizes %v: quality -5 should encode as 1, and 1000 as 100", sizes)
	}
	if !(sizes[1] < sizes[2] && sizes[2] < sizes[3] && sizes[3] < sizes[4]) {
		t.Errorf("sizes %v don't grow with quality", sizes)
	}

	// Any kind of image can be encoded.
	for name, img := range sources(24, 16) {
		data, err := EncodeJPEG(img, 80)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width != 24 || cfg.Height != 16 {
			t.Errorf("%s: decodes as %dx%d, %v", name, cfg.Width, cfg.Height, err)
		}
	}
	turned, err := EncodeJPEG(Orient(src, 6), 80)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := jpeg.DecodeConfig(bytes.NewReader(turned)); cfg.Width != 60 || cfg.Height != 80 {
		t.Errorf("turned picture encodes as %dx%d, want 60x80", cfg.Width, cfg.Height)
	}

	for _, empty := range []image.Image{nil, image.NewRGBA(image.Rectangle{})} {
		if data, err := EncodeJPEG(empty, 80); err == nil || data != nil {
			t.Errorf("EncodeJPEG(%v) = %d bytes, %v; want an error", empty, len(data), err)
		}
		if data, err := Thumbnail(empty, 100, 50); err == nil || data != nil {
			t.Errorf("Thumbnail(%v) = %d bytes, %v; want an error", empty, len(data), err)
		}
	}
}

func TestThumbnail(t *testing.T) {
	tests := []struct {
		name         string
		img          image.Image
		maxSide      int
		wantW, wantH int
	}{
		{"landscape", gradient(400, 300), 100, 100, 75},
		{"portrait", gradient(300, 400), 100, 75, 100},
		{"square", gradient(256, 256), 100, 100, 100},
		{"already small", gradient(50, 40), 100, 50, 40},
		{"exactly the limit", gradient(100, 20), 100, 100, 20},
		{"very wide", gradient(1000, 10), 100, 100, 1},
		{"very tall", gradient(3, 900), 100, 1, 100},
		{"smaller limit", gradient(400, 300), 32, 32, 24},
		{"a JPEG's pixels", photoYCbCr(640, 480, image.YCbCrSubsampleRatio420), 100, 100, 75},
		{"turned on its side", Orient(gradient(400, 300), 6), 100, 75, 100},
		{"with transparency", image.NewNRGBA(image.Rect(0, 0, 512, 512)), 100, 100, 100},
		{"no limit to speak of", gradient(40, 30), 0, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := Thumbnail(tt.img, tt.maxSide, 50)
			if err != nil {
				t.Fatal(err)
			}
			info, err := ProbeBytes(data)
			if err != nil {
				t.Fatalf("not an image: %v", err)
			}
			if info.Format != "jpeg" || info.Width != tt.wantW || info.Height != tt.wantH {
				t.Errorf("got a %dx%d %s, want a %dx%d jpeg", info.Width, info.Height, info.Format, tt.wantW, tt.wantH)
			}
			if max(info.Width, info.Height) > max(tt.maxSide, 1) {
				t.Errorf("longest side %d is over %d", max(info.Width, info.Height), tt.maxSide)
			}
			if _, _, err := Decode(data, DefaultBudget); err != nil {
				t.Errorf("doesn't decode: %v", err)
			}
		})
	}

	// It is the same picture: the corners of a thumbnail of the gradient
	// are the corners of the gradient.
	data, err := Thumbnail(gradient(400, 300), 100, 90)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := Decode(data, DefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[image.Point]color.RGBA{
		{1, 1}:   {3, 4, 4, 255},
		{98, 1}:  {251, 4, 144, 255},
		{1, 73}:  {3, 250, 109, 255},
		{98, 73}: {251, 250, 250, 255},
	} {
		if got := rgbaAt(img, p.X, p.Y); maxDiff(got, want) > 14 {
			t.Errorf("pixel %v = %v, want about %v", p, got, want)
		}
	}
	// A typical WhatsApp thumbnail is a few kilobytes at most.
	small, err := Thumbnail(photo(1600, 1200), 100, 50)
	if err != nil || len(small) > 6<<10 {
		t.Errorf("thumbnail of a photo: %d bytes, %v; want at most 6 kB", len(small), err)
	}
}
