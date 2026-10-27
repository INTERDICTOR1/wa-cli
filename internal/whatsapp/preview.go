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

// maxPreviewDownload caps the size of a full image downloaded only to draw
// a small preview, to keep memory use bounded.
const maxPreviewDownload = 8 << 20

// ImagePreview returns a picture to preview an image or video message:
// the small thumbnail embedded in the message when there is one,
// otherwise (images only) the full image downloaded from WhatsApp.
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
		if img.GetFileLength() > maxPreviewDownload {
			return nil, waerrors.New("image too large to preview")
		}
		var err error
		if data, err = c.WA.Download(ctx, img); err != nil {
			return nil, waerrors.Wrap(err, "downloading image")
		}
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, waerrors.Wrap(err, "decoding image")
	}
	return decoded, nil
}
