package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow"

	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/termimg"
)

// previewMemoryBudget bounds what drawing one preview may cost: the
// downloaded file plus its decoded pixels. Measured: a 4096x3072 JPEG
// (WhatsApp HD) costs ~30 MB, a 4096x4096 PNG ~134 MB.
const previewMemoryBudget = 50 << 20

// decodeCost estimates the memory to hold a file of fileSize bytes plus its
// decoded w x h pixels: JPEG up to 3 bytes/pixel (usually 1.5), others 4.
func decodeCost(w, h, fileSize int, isJPEG bool) int {
	bpp := 4
	if isJPEG {
		bpp = 3
	}
	return fileSize + w*h*bpp
}

// maxBlindDownload is the largest image downloaded for a preview when
// the message doesn't say how many pixels it has.
const maxBlindDownload = 8 << 20

// Thumbnail returns a small picture for an image, video, sticker or
// document message. The thumbnail embedded in the message is used when
// there is one (no network). Otherwise an image or sticker is downloaded
// and decoded, but only if the message's own description of itself says
// that fits the memory budget; anything else is an error.
func (c *Client) Thumbnail(ctx context.Context, m msgstore.Message) (image.Image, error) {
	a := analyze(contentOf(m))
	if a.Class != classRow {
		return nil, waerrors.New("no preview available")
	}
	budget := int64(termimg.DefaultBudget)
	if len(a.Thumb) > 0 {
		img, _, err := termimg.Decode(a.Thumb, budget)
		if err == nil {
			return img, nil
		}
		// A broken embedded thumbnail: fall through to the real thing.
	}
	if (a.Kind != KindImage && a.Kind != KindSticker) || a.Download == nil || a.Media == nil {
		return nil, waerrors.New("no preview available")
	}
	if a.Download.GetDirectPath() == "" {
		return nil, waerrors.New("the image isn't uploaded yet")
	}

	// Check before downloading, from what the message says about itself.
	size := int(a.Media.Size)
	if a.Media.Width > 0 && a.Media.Height > 0 {
		isJPEG := a.Media.Mime == "image/jpeg"
		if int64(decodeCost(a.Media.Width, a.Media.Height, size, isJPEG)) > budget {
			return nil, fmt.Errorf("image too large to preview: %w", termimg.ErrTooLarge)
		}
	} else if size > maxBlindDownload {
		return nil, fmt.Errorf("image too large to preview: %w", termimg.ErrTooLarge)
	}
	if c.WA == nil {
		return nil, waerrors.New("not connected")
	}
	data, err := c.WA.Download(ctx, a.Download)
	if err != nil {
		return nil, mediaError(err, "downloading image")
	}
	// termimg checks again, from the real header, before decoding pixels.
	img, _, err := termimg.Decode(data, budget)
	if err != nil {
		return nil, waerrors.Wrap(err, "decoding image")
	}
	return img, nil
}

// ImagePreview is the old name of Thumbnail.
func (c *Client) ImagePreview(ctx context.Context, m msgstore.Message) (image.Image, error) {
	return c.Thumbnail(ctx, m)
}

// mediaError explains a failed media download.
func mediaError(err error, doing string) error {
	if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) {
		return waerrors.New("this media is no longer on WhatsApp's servers (it expired): ask for it to be sent again")
	}
	return waerrors.Wrap(err, doing)
}

// DownloadMedia reconstructs a stored message's media content from its
// RawProto and downloads it via WhatsApp's encrypted media CDN. Returns
// the raw bytes and the mimetype recorded with the message. The whole
// file is held in memory: prefer SaveMedia for anything large.
func (c *Client) DownloadMedia(ctx context.Context, quoted msgstore.Message) ([]byte, string, error) {
	msg := contentOf(quoted)
	if msg == nil {
		return nil, "", waerrors.New("original message content isn't available (too old, or not captured)")
	}
	a := analyze(msg)
	if a.Class != classRow || a.Download == nil || a.Media == nil {
		return nil, "", waerrors.New("this message doesn't contain downloadable media")
	}
	data, err := c.WA.Download(ctx, a.Download)
	if err != nil {
		return nil, "", mediaError(err, "downloading media")
	}
	return data, a.Media.Mime, nil
}

// SaveMedia downloads msg's attachment into dir, streaming to disk, and
// returns the file's path. The name is the document's own file name when
// it has one, otherwise "<kind>-<time>-<id>.<ext>"; a file already there
// with the right size is returned without downloading again.
func (c *Client) SaveMedia(ctx context.Context, m msgstore.Message, dir string) (string, error) {
	a := analyze(contentOf(m))
	if a.Class != classRow || a.Download == nil || a.Media == nil {
		return "", waerrors.New("this message has no attachment to save")
	}
	if a.Download.GetDirectPath() == "" {
		return "", waerrors.New("the attachment isn't uploaded yet")
	}
	if dir == "" {
		return "", waerrors.New("no folder to save into")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", waerrors.Wrap(err, "creating download folder")
	}

	size := int64(a.Media.Size)
	names := mediaFileNames(m, a)
	target := ""
	for i, name := range names {
		path := filepath.Join(dir, name)
		if rel, err := filepath.Rel(dir, path); err != nil || rel != name {
			continue // never outside dir
		}
		st, err := os.Stat(path)
		if err == nil && st.Mode().IsRegular() && st.Size() > 0 && (size == 0 || st.Size() == size) {
			return path, nil // already downloaded
		}
		if errors.Is(err, os.ErrNotExist) || (err == nil && st.Mode().IsRegular() && i == len(names)-1) {
			// Free — or the last name, which only this message produces:
			// what is there is a stale copy (say, from before its HD
			// version arrived) and is replaced.
			target = path
			break
		}
		// Taken by a different file: try the next, more specific name.
	}
	if target == "" {
		return "", waerrors.New("couldn't find a free file name in " + dir)
	}
	if c.WA == nil {
		return "", waerrors.New("not connected")
	}

	tmp, err := os.CreateTemp(dir, ".wa-download-*.part")
	if err != nil {
		return "", waerrors.Wrap(err, "creating download file")
	}
	tmpName := tmp.Name()
	err = c.WA.DownloadToFile(ctx, a.Download, tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmpName)
		return "", mediaError(err, "downloading media")
	}
	os.Remove(target) // Windows doesn't rename over an existing file
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return "", waerrors.Wrap(err, "saving download")
	}
	markFromInternet(target)
	return target, nil
}

// markFromInternet tags a downloaded file the way browsers do on Windows
// (the "mark of the web"): Windows then checks a program before running
// it, and Office opens a document read-only with its macros blocked. The
// tag is a second, named data stream of the file; the file's own content
// is untouched. Best effort - other systems, and file systems without
// named streams, are left alone.
func markFromInternet(path string) {
	if runtime.GOOS != "windows" {
		return
	}
	_ = os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644)
}

// mediaFileNames returns the names to save m's attachment under, in
// order of preference. They are deterministic, so a second SaveMedia
// finds the first one's file.
func mediaFileNames(m msgstore.Message, a analysis) []string {
	ext := extensionFor(a.Media.Mime)
	id := shortID(m.ID)
	stamp := time.UnixMilli(m.Timestamp).Local().Format("20060102-150405")
	kind := strings.ReplaceAll(a.Kind, " ", "-")
	generated := sanitizeFileName(fmt.Sprintf("%s-%s-%s", kind, stamp, id) + ext)

	own := sanitizeFileName(a.Media.FileName)
	if own == "" {
		return []string{generated}
	}
	if filepath.Ext(own) == "" {
		own += ext
	}
	// The same name used by a different document: tell them apart by ID.
	base := strings.TrimSuffix(own, filepath.Ext(own))
	return []string{own, sanitizeFileName(base + "-" + id + filepath.Ext(own)), generated}
}

// shortID returns up to the last 8 characters of a message ID, reduced
// to letters and digits.
func shortID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 8 {
		s = s[len(s)-8:]
	}
	if s == "" {
		s = "file"
	}
	return s
}

// mimeExtensions are the extensions for the types WhatsApp media and
// documents usually have. The list is closed on purpose and holds only
// types that are shown or played: the type is whatever the sender claims,
// and looking it up in the system's table would let a "video" of type
// application/hta be saved as a .hta file, which Windows runs.
var mimeExtensions = map[string]string{
	"image/jpeg":                    ".jpg",
	"image/png":                     ".png",
	"image/webp":                    ".webp",
	"image/gif":                     ".gif",
	"image/bmp":                     ".bmp",
	"image/heic":                    ".heic",
	"image/heif":                    ".heif",
	"image/tiff":                    ".tiff",
	"image/avif":                    ".avif",
	"video/mp4":                     ".mp4",
	"video/3gpp":                    ".3gp",
	"video/quicktime":               ".mov",
	"video/webm":                    ".webm",
	"video/x-matroska":              ".mkv",
	"video/x-msvideo":               ".avi",
	"audio/ogg":                     ".ogg",
	"audio/opus":                    ".opus",
	"audio/mpeg":                    ".mp3",
	"audio/mp4":                     ".m4a",
	"audio/x-m4a":                   ".m4a",
	"audio/aac":                     ".aac",
	"audio/amr":                     ".amr",
	"audio/wav":                     ".wav",
	"audio/x-wav":                   ".wav",
	"audio/flac":                    ".flac",
	"application/pdf":               ".pdf",
	"application/zip":               ".zip",
	"text/plain":                    ".txt",
	"text/csv":                      ".csv",
	"application/json":              ".json",
	"application/rtf":               ".rtf",
	"text/rtf":                      ".rtf",
	"text/vcard":                    ".vcf",
	"text/x-vcard":                  ".vcf",
	"application/epub+zip":          ".epub",
	"application/msword":            ".doc",
	"application/vnd.ms-excel":      ".xls",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/vnd.oasis.opendocument.text":                                   ".odt",
	"application/vnd.oasis.opendocument.spreadsheet":                            ".ods",
	"application/vnd.oasis.opendocument.presentation":                           ".odp",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
}

// extensionFor returns the file extension (with its dot) for a MIME
// type, ".bin" for one that isn't in mimeExtensions.
func extensionFor(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	base = strings.ToLower(strings.TrimSpace(base))
	if ext, ok := mimeExtensions[base]; ok {
		return ext
	}
	return ".bin"
}

// reservedNames are file names Windows keeps for devices.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeFileName turns a name received from someone else into a safe
// file name: no directories, no characters or names a file system
// rejects, not hidden, and of reasonable length. "" when nothing usable
// is left.
func sanitizeFileName(name string) string {
	// Keep only the last path element, whichever separator was used.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r):
			// control and invisible formatting characters
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	name = strings.TrimLeft(name, ". ")
	if name == "" {
		return ""
	}

	const maxRunes = 120
	if runes := []rune(name); len(runes) > maxRunes {
		ext := filepath.Ext(name)
		if extRunes := []rune(ext); len(extRunes) < 16 {
			name = strings.TrimRight(string(runes[:maxRunes-len(extRunes)]), ". ") + ext
		} else {
			name = string(runes[:maxRunes])
		}
	}
	stem, _, _ := strings.Cut(name, ".")
	if reservedNames[strings.ToUpper(stem)] {
		name = "_" + name
	}
	return name
}
