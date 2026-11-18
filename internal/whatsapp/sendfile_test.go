package whatsapp

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"

	"github.com/codebyoketch/wa-cli/internal/termimg"
)

func TestDetectFile(t *testing.T) {
	jpegHead := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F', 0}
	pngHead := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	cases := []struct {
		name string
		head []byte
		kind fileKind
		mime string
	}{
		{"photo.jpg", nil, fileImage, "image/jpeg"},
		{"PHOTO.JPEG", nil, fileImage, "image/jpeg"},
		{"shot.png", nil, fileImage, "image/png"},
		{"pic.webp", nil, fileImage, "image/webp"},
		{"anim.gif", nil, fileImage, "image/gif"},
		{"scan.bmp", nil, fileImage, "image/bmp"},
		{"clip.mp4", nil, fileVideo, "video/mp4"},
		{"clip.MOV", nil, fileVideo, "video/quicktime"},
		{"old.3gp", nil, fileVideo, "video/3gpp"},
		{"note.ogg", nil, fileAudio, oggOpus},
		{"note.opus", nil, fileAudio, oggOpus},
		{"song.mp3", nil, fileAudio, "audio/mpeg"},
		{"memo.m4a", nil, fileAudio, "audio/mp4"},
		{"beep.wav", nil, fileAudio, "audio/wav"},
		// Formats WhatsApp's apps can't play inline go as documents.
		{"screen.webm", nil, fileDocument, "video/webm"},
		{"film.mkv", nil, fileDocument, "video/x-matroska"},
		{"album.flac", nil, fileDocument, "audio/flac"},
		{"report.pdf", nil, fileDocument, "application/pdf"},
		{"data.zip", nil, fileDocument, "application/zip"},
		// No (useful) extension: look at the content.
		{"IMG_0001", jpegHead, fileImage, "image/jpeg"},
		{"download.tmp", pngHead, fileImage, "image/png"},
		{"notes", []byte("just some text\n"), fileDocument, "text/plain; charset=utf-8"},
		{"blob.xyz123", []byte{0, 1, 2, 3, 4}, fileDocument, "application/octet-stream"},
	}
	for _, c := range cases {
		kind, mime := detectFile(c.name, c.head)
		if kind != c.kind || mime != c.mime {
			t.Errorf("%s: got %v %q, want %v %q", c.name, kind, mime, c.kind, c.mime)
		}
	}
}

// gradient is a test picture with some detail in it.
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255})
		}
	}
	return img
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func jpegBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withOrientation inserts an EXIF block saying the JPEG must be shown
// rotated (orientation 1..8), as phone cameras do for portrait photos.
func withOrientation(jpegData []byte, orientation uint16) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0} // little-endian TIFF, first IFD at 8
	tiff = append(tiff, 1, 0)                   // one entry
	entry := make([]byte, 12)
	binary.LittleEndian.PutUint16(entry[0:], 0x0112) // Orientation
	binary.LittleEndian.PutUint16(entry[2:], 3)      // SHORT
	binary.LittleEndian.PutUint32(entry[4:], 1)      // one value
	binary.LittleEndian.PutUint16(entry[8:], orientation)
	tiff = append(tiff, entry...)
	tiff = append(tiff, 0, 0, 0, 0) // no further IFD

	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	segment = append(segment, payload...)

	out := append([]byte{}, jpegData[:2]...) // SOI
	out = append(out, segment...)
	return append(out, jpegData[2:]...)
}

func TestPrepareFile_Images(t *testing.T) {
	upright := jpegBytes(t, gradient(640, 480))

	t.Run("an upright JPEG is sent byte for byte", func(t *testing.T) {
		path := writeFile(t, "photo.jpg", upright)
		p, err := prepareFile(Outgoing{FilePath: path})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileImage || p.mime != "image/jpeg" || p.data != nil || p.path != path {
			t.Errorf("prepared = kind %v mime %q data %d bytes path %q, want the file itself as an image", p.kind, p.mime, len(p.data), p.path)
		}
		if p.width != 640 || p.height != 480 || p.size != int64(len(upright)) {
			t.Errorf("size %dx%d, %d bytes; want 640x480, %d bytes", p.width, p.height, p.size, len(upright))
		}
		thumb, err := jpeg.DecodeConfig(bytes.NewReader(p.thumb))
		if err != nil || max(thumb.Width, thumb.Height) != 100 {
			t.Errorf("thumbnail: %v %+v, want a JPEG 100 pixels long", err, thumb)
		}
	})

	t.Run("a PNG is sent as it is", func(t *testing.T) {
		data := pngBytes(t, gradient(300, 200))
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "shot.png", data)})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileImage || p.mime != "image/png" || p.data != nil || p.width != 300 || p.height != 200 || len(p.thumb) == 0 {
			t.Errorf("prepared = %+v", summary(p))
		}
	})

	t.Run("the real format wins over a wrong extension", func(t *testing.T) {
		data := pngBytes(t, gradient(50, 50))
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "actually-png.jpg", data)})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileImage || p.mime != "image/png" {
			t.Errorf("prepared = %+v, want image/png", summary(p))
		}
	})

	t.Run("a photo rotated by EXIF is turned upright and re-encoded", func(t *testing.T) {
		rotated := withOrientation(upright, 6) // 640x480 stored, 480x640 shown
		info, err := termimg.ProbeBytes(rotated)
		if err != nil || info.Orientation != 6 {
			t.Fatalf("test image: orientation %d, err %v", info.Orientation, err)
		}
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "portrait.jpg", rotated)})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileImage || p.mime != "image/jpeg" || p.data == nil {
			t.Fatalf("prepared = %+v, want a re-encoded JPEG", summary(p))
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(p.data))
		if err != nil || cfg.Width != 480 || cfg.Height != 640 || p.width != 480 || p.height != 640 {
			t.Errorf("re-encoded as %dx%d (reported %dx%d), err %v; want 480x640", cfg.Width, cfg.Height, p.width, p.height, err)
		}
		if again, _ := termimg.ProbeBytes(p.data); again.Orientation > 1 {
			t.Errorf("the re-encoded file still carries orientation %d", again.Orientation)
		}
		if p.size != int64(len(p.data)) || len(p.thumb) == 0 {
			t.Errorf("size %d for %d bytes, thumb %d bytes", p.size, len(p.data), len(p.thumb))
		}
	})

	t.Run("an image longer than the limit is scaled down", func(t *testing.T) {
		data := pngBytes(t, gradient(5000, 20))
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "panorama.png", data)})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileImage || p.mime != "image/jpeg" || p.data == nil || p.width != maxImageSide || p.height != 16 {
			t.Errorf("prepared = %+v, want a %dx16 JPEG", summary(p), maxImageSide)
		}
	})

	t.Run("a GIF becomes a still JPEG", func(t *testing.T) {
		pal := image.NewPaletted(image.Rect(0, 0, 40, 30), color.Palette{color.Black, color.White})
		var buf bytes.Buffer
		if err := gif.Encode(&buf, pal, nil); err != nil {
			t.Fatal(err)
		}
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "anim.gif", buf.Bytes())})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileImage || p.mime != "image/jpeg" || p.data == nil || p.width != 40 || p.height != 30 {
			t.Errorf("prepared = %+v", summary(p))
		}
	})

	t.Run("AsDocument sends the bytes untouched", func(t *testing.T) {
		rotated := withOrientation(upright, 6)
		path := writeFile(t, "portrait.jpg", rotated)
		p, err := prepareFile(Outgoing{FilePath: path, AsDocument: true})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileDocument || p.data != nil || p.path != path || p.name != "portrait.jpg" || p.mime != "image/jpeg" || p.size != int64(len(rotated)) {
			t.Errorf("prepared = %+v", summary(p))
		}
	})

	t.Run("something that isn't an image goes as a document", func(t *testing.T) {
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "broken.jpg", []byte("this is not a jpeg"))})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileDocument || p.data != nil || p.name != "broken.jpg" {
			t.Errorf("prepared = %+v", summary(p))
		}
	})
}

func summary(p *preparedFile) map[string]any {
	return map[string]any{
		"kind": p.kind.String(), "mime": p.mime, "name": p.name, "size": p.size,
		"reencoded": p.data != nil, "w": p.width, "h": p.height, "voice": p.voice, "seconds": p.seconds, "thumb": len(p.thumb),
	}
}

// oggPage is a minimal Ogg page whose position says how many samples
// have played by its end.
func oggPage(granule uint64) []byte {
	page := []byte("OggS\x00\x04")
	page = binary.LittleEndian.AppendUint64(page, granule)
	return append(page, make([]byte, 13)...) // serial, sequence, checksum, no segments
}

func TestPrepareFile_AudioVideoAndOthers(t *testing.T) {
	voice := append(oggPage(0), oggPage(48000*12+100)...) // 12.002 s

	t.Run("an ogg file sent as a voice note", func(t *testing.T) {
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "note.ogg", voice), Voice: true})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileAudio || !p.voice || p.mime != oggOpus || p.seconds != 13 || p.data != nil {
			t.Errorf("prepared = %+v", summary(p))
		}
		msg := p.message("", nil, nil).GetAudioMessage()
		if !msg.GetPTT() || msg.GetSeconds() != 13 || msg.GetMimetype() != oggOpus {
			t.Errorf("message = %+v", msg)
		}
	})

	t.Run("only ogg can be a voice note", func(t *testing.T) {
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "song.mp3", []byte("ID3 not really audio")), Voice: true})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileAudio || p.voice || p.mime != "audio/mpeg" {
			t.Errorf("prepared = %+v", summary(p))
		}
	})

	t.Run("a video is streamed as it is", func(t *testing.T) {
		data := bytes.Repeat([]byte("mp4"), 1000)
		path := writeFile(t, "clip.mp4", data)
		p, err := prepareFile(Outgoing{FilePath: path})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileVideo || p.data != nil || p.path != path || p.size != 3000 {
			t.Errorf("prepared = %+v", summary(p))
		}
		if p.kind.mediaType() != whatsmeow.MediaVideo {
			t.Errorf("upload type = %q", p.kind.mediaType())
		}
	})

	t.Run("anything else is a document with its name", func(t *testing.T) {
		p, err := prepareFile(Outgoing{FilePath: writeFile(t, "Q3 report.pdf", []byte("%PDF-1.7"))})
		if err != nil {
			t.Fatal(err)
		}
		if p.kind != fileDocument || p.name != "Q3 report.pdf" || p.mime != "application/pdf" {
			t.Errorf("prepared = %+v", summary(p))
		}
		plain := p.message("", nil, nil)
		if doc := plain.GetDocumentMessage(); doc.GetFileName() != "Q3 report.pdf" || doc.GetFileLength() != 8 || doc.GetDirectPath() != "" {
			t.Errorf("placeholder document = %+v", doc)
		}
		// With a caption it travels in the envelope WhatsApp shows captions from.
		captioned := p.message("please read", nil, &whatsmeow.UploadResponse{
			URL: "https://mmg/x", DirectPath: "/v/doc", MediaKey: []byte("k"), FileEncSHA256: []byte("e"), FileSHA256: []byte("s"), FileLength: 8,
		})
		doc := captioned.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage()
		if doc.GetCaption() != "please read" || doc.GetDirectPath() != "/v/doc" || string(doc.GetMediaKey()) != "k" || doc.GetMediaKeyTimestamp() == 0 {
			t.Errorf("captioned document = %+v (message %+v)", doc, captioned)
		}
		if a := analyze(unwrapMessage(captioned)); a.Kind != KindDocument || a.Text != "please read" || !hasMediaRef(unwrapMessage(captioned)) {
			t.Errorf("captioned document reads back as %+v", a)
		}
	})

	t.Run("files that can't be sent", func(t *testing.T) {
		if _, err := prepareFile(Outgoing{FilePath: filepath.Join(t.TempDir(), "missing.jpg")}); err == nil {
			t.Error("a missing file was accepted")
		}
		if _, err := prepareFile(Outgoing{FilePath: writeFile(t, "empty.txt", nil)}); err == nil {
			t.Error("an empty file was accepted")
		}
		if _, err := prepareFile(Outgoing{FilePath: t.TempDir()}); err == nil {
			t.Error("a folder was accepted")
		}
	})
}

func TestImageMessageFields(t *testing.T) {
	p := &preparedFile{kind: fileImage, mime: "image/jpeg", size: 1234, width: 800, height: 600, thumb: []byte("thumb")}
	up := &whatsmeow.UploadResponse{
		URL: "https://mmg/y", DirectPath: "/v/img", MediaKey: []byte("key"),
		FileEncSHA256: []byte("enc"), FileSHA256: []byte("sha"), FileLength: 1234,
	}
	img := p.message("caption", nil, up).GetImageMessage()
	if img.GetURL() != "https://mmg/y" || img.GetDirectPath() != "/v/img" || string(img.GetMediaKey()) != "key" ||
		string(img.GetFileEncSHA256()) != "enc" || string(img.GetFileSHA256()) != "sha" {
		t.Errorf("upload references missing: %+v", img)
	}
	if img.GetFileLength() != 1234 || img.GetMimetype() != "image/jpeg" || img.GetWidth() != 800 || img.GetHeight() != 600 ||
		string(img.GetJPEGThumbnail()) != "thumb" || img.GetCaption() != "caption" {
		t.Errorf("image details missing: %+v", img)
	}
	// Before the upload: everything known locally, nothing pointing at a file.
	placeholder := p.message("", nil, nil)
	if hasMediaRef(placeholder) || placeholder.GetImageMessage().GetWidth() != 800 || placeholder.GetImageMessage().Caption != nil {
		t.Errorf("placeholder = %+v", placeholder.GetImageMessage())
	}
}

func TestOggDuration(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"one second", oggPage(48000), 1},
		{"rounds up", append(oggPage(0), oggPage(48000*3+1)...), 4},
		{"not ogg", []byte("RIFF....WAVE"), 0},
		{"empty", nil, 0},
		{"cut-off page", []byte("OggS\x00\x04\x01"), 0},
	}
	for _, c := range cases {
		if got := oggDuration(bytes.NewReader(c.data), int64(len(c.data))); got != c.want {
			t.Errorf("%s: %d seconds, want %d", c.name, got, c.want)
		}
	}
}

func TestScaledSize(t *testing.T) {
	budget := int64(termimg.DefaultBudget)
	cases := []struct{ w, h int }{{5000, 20}, {8000, 6000}, {4624, 3468}, {3000, 9000}, {20000, 20000}}
	for _, c := range cases {
		w, h := scaledSize(c.w, c.h, budget)
		if max(w, h) > maxImageSide || w < 1 || h < 1 {
			t.Errorf("%dx%d scaled to %dx%d: longer than %d", c.w, c.h, w, h, maxImageSide)
		}
		if int64(w)*int64(h)*4 > budget {
			t.Errorf("%dx%d scaled to %dx%d: the copy alone exceeds the memory budget", c.w, c.h, w, h)
		}
		// Proportions are kept (within rounding).
		if got, want := float64(w)/float64(h), float64(c.w)/float64(c.h); got/want < 0.95 || got/want > 1.05 {
			t.Errorf("%dx%d scaled to %dx%d: proportions changed", c.w, c.h, w, h)
		}
	}
	if w, h := scaledSize(5000, 20, budget); w != maxImageSide || h != 16 {
		t.Errorf("a thin panorama should keep the full %d: got %dx%d", maxImageSide, w, h)
	}
}
