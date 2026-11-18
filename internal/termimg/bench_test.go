package termimg

import (
	"bufio"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

// The sizes that matter: a WhatsApp "HD" photo, and a large terminal (200x50
// cells of 10x20 pixels).
const (
	hdWidth, hdHeight         = 4096, 3072
	screenWidth, screenHeight = 2000, 1000
)

// hdJPEG writes a photo-like 4096x3072 JPEG (baseline, 4:2:0, as cameras and
// WhatsApp make them) to a temporary file and returns its path.
func hdJPEG(tb testing.TB) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "hd.jpg")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	w := bufio.NewWriter(f)
	if err := jpeg.Encode(w, photoYCbCr(hdWidth, hdHeight, image.YCbCrSubsampleRatio420), &jpeg.Options{Quality: 85}); err != nil {
		tb.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
	return path
}

// showHD is the whole way from a file to the terminal: decode, turn, scale
// to the screen, write as Sixel.
func showHD(tb testing.TB, path string, out io.Writer) {
	img, info, err := Load(path, DefaultBudget)
	if err != nil {
		tb.Fatal(err)
	}
	img = Orient(img, info.Orientation)
	b := img.Bounds()
	w, h := Fit(b.Dx(), b.Dy(), screenWidth, screenHeight)
	small := Scale(img, w, h)
	img = nil // the decoded pixels are garbage from here on
	if err := WriteSixel(out, small, 256); err != nil {
		tb.Fatal(err)
	}
}

// TestPipelineMemory checks the promise the package exists for: showing a
// 12-megapixel photo full screen stays inside the budget. It counts every
// byte allocated on the way, garbage included, which is an upper bound for
// what is ever held at once; and watches the heap while it runs under the
// app's own garbage collector settings.
func TestPipelineMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("encodes and decodes a 12-megapixel JPEG")
	}
	path := hdJPEG(t)
	info, err := Probe(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()

	// The app runs with a soft limit on the heap; the test's own garbage
	// (the picture the JPEG was made from) is gone by now.
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(70 << 20))
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	start := time.Now()
	var out chunkCounter
	showHD(t, path, &out)
	elapsed := time.Since(start)
	close(stop)
	<-done

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	total := after.TotalAlloc - base.TotalAlloc
	held := int64(peak.Load()) - int64(base.HeapAlloc)
	t.Logf("%dx%d JPEG of %d kB -> %d kB of Sixel in %v", info.Width, info.Height, info.Size>>10, out.bytes>>10, elapsed.Round(time.Millisecond))
	t.Logf("allocated in total: %.1f MB; most on the heap at once (sampled): %.1f MB; budget: %d MB",
		float64(total)/(1<<20), float64(held)/(1<<20), DefaultBudget>>20)
	t.Logf("Cost() estimate for the decode alone: %.1f MB", float64(Cost(info.Width, info.Height, info.Size, info.Format))/(1<<20))
	if total > DefaultBudget {
		t.Errorf("the pipeline allocated %d bytes in total, over the budget of %d", total, DefaultBudget)
	}
}

func BenchmarkScaleYCbCr(b *testing.B) {
	src := photoYCbCr(hdWidth, hdHeight, image.YCbCrSubsampleRatio420)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scale(src, screenWidth, screenHeight)
	}
}

func BenchmarkScaleRGBA(b *testing.B) {
	src := photo(hdWidth/2, hdHeight/2) // 2048x1536: a PNG screenshot
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scale(src, screenWidth/2, screenHeight/2)
	}
}

func BenchmarkWriteSixel(b *testing.B) {
	src := photo(screenWidth, screenHeight)
	var out chunkCounter
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := WriteSixel(&out, src, 256); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(out.bytes)/float64(b.N)/(1<<20), "MB-out")
}

// BenchmarkWriteSixelSmooth is a picture without noise: fewer colours per
// band and long runs, so less to write.
func BenchmarkWriteSixelSmooth(b *testing.B) {
	src := gradient(screenWidth, screenHeight)
	var out chunkCounter
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := WriteSixel(&out, src, 256); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(out.bytes)/float64(b.N)/(1<<20), "MB-out")
}

func BenchmarkWriteKitty(b *testing.B) {
	src := photo(screenWidth, screenHeight)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := WriteKitty(io.Discard, src); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHalfBlocks draws a full 200x60 terminal from a picture that is
// already about that size (a cached preview)...
func BenchmarkHalfBlocks(b *testing.B) {
	src := photo(200, 120)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HalfBlocks(src, 200, 60)
	}
}

// ...and from a picture that first has to be shrunk: the scaled photo of
// the Sixel benchmarks.
func BenchmarkHalfBlocksFromScreenSize(b *testing.B) {
	src := photo(screenWidth, screenHeight)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		HalfBlocks(src, 200, 60)
	}
}

func BenchmarkThumbnail(b *testing.B) {
	src := photoYCbCr(hdWidth, hdHeight, image.YCbCrSubsampleRatio420)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Thumbnail(src, 100, 50); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProbe(b *testing.B) {
	path := hdJPEG(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Probe(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPipeline is everything, from the file on disk to the last byte
// of Sixel: Load(4096x3072 JPEG) -> Scale(2000x1000 box) -> WriteSixel.
// B/op is the total allocated per picture, garbage included.
func BenchmarkPipeline(b *testing.B) {
	path := hdJPEG(b)
	runtime.GC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		showHD(b, path, io.Discard)
	}
}
