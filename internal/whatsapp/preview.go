package whatsapp

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg" // register decoders for image.Decode
	_ "image/png"

	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
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

// ImagePreview returns a picture to preview an image or video message:
// the small thumbnail embedded in the message when there is one,
// otherwise (images only) the full image downloaded from WhatsApp, if it
// fits previewMemoryBudget.
func (c *Client) ImagePreview(ctx context.Context, m msgstore.Message) (image.Image, error) {
	msg := decodeRawProto(m.RawProto)
	img, vid := msg.GetImageMessage(), msg.GetVideoMessage()

	data := img.GetJPEGThumbnail()
	if len(data) == 0 {
		data = vid.GetJPEGThumbnail()
	}
	if len(data) == 0 {
		if img == nil {
			return nil, waerrors.New("no preview available")
		}
		// Check before downloading, from what the message says about itself.
		isJPEG := img.GetMimetype() == "image/jpeg"
		if decodeCost(int(img.GetWidth()), int(img.GetHeight()), int(img.GetFileLength()), isJPEG) > previewMemoryBudget {
			return nil, waerrors.New("image too large to preview")
		}
		var err error
		if data, err = c.WA.Download(ctx, img); err != nil {
			return nil, waerrors.Wrap(err, "downloading image")
		}
	}
	// Check again from the real header before decoding all the pixels.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, waerrors.Wrap(err, "reading image header")
	}
	if decodeCost(cfg.Width, cfg.Height, len(data), format == "jpeg") > previewMemoryBudget {
		return nil, waerrors.New("image too large to preview")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, waerrors.Wrap(err, "decoding image")
	}
	return decoded, nil
}
