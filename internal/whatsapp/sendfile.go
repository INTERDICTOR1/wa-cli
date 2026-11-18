package whatsapp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/termimg"
)

// fileKind is the kind of message a file is sent as.
type fileKind int

const (
	fileDocument fileKind = iota
	fileImage
	fileVideo
	fileAudio
)

func (k fileKind) String() string {
	return [...]string{"document", "image", "video", "audio"}[k]
}

func (k fileKind) mediaType() whatsmeow.MediaType {
	switch k {
	case fileImage:
		return whatsmeow.MediaImage
	case fileVideo:
		return whatsmeow.MediaVideo
	case fileAudio:
		return whatsmeow.MediaAudio
	}
	return whatsmeow.MediaDocument
}

type fileType struct {
	kind fileKind
	mime string
}

// oggOpus is the MIME type WhatsApp uses for voice notes.
const oggOpus = "audio/ogg; codecs=opus"

// fileTypes maps a file extension to how the file is sent. Only formats
// WhatsApp's apps show or play inline are sent as media; the rest
// (WebM, Matroska, FLAC, ...) go as documents so they arrive intact.
var fileTypes = map[string]fileType{
	".jpg":  {fileImage, "image/jpeg"},
	".jpeg": {fileImage, "image/jpeg"},
	".jpe":  {fileImage, "image/jpeg"},
	".png":  {fileImage, "image/png"},
	".webp": {fileImage, "image/webp"},
	".gif":  {fileImage, "image/gif"},
	".bmp":  {fileImage, "image/bmp"},

	".mp4": {fileVideo, "video/mp4"},
	".m4v": {fileVideo, "video/mp4"},
	".mov": {fileVideo, "video/quicktime"},
	".3gp": {fileVideo, "video/3gpp"},

	".ogg":  {fileAudio, oggOpus},
	".opus": {fileAudio, oggOpus},
	".oga":  {fileAudio, "audio/ogg"},
	".mp3":  {fileAudio, "audio/mpeg"},
	".m4a":  {fileAudio, "audio/mp4"},
	".aac":  {fileAudio, "audio/aac"},
	".wav":  {fileAudio, "audio/wav"},
	".amr":  {fileAudio, "audio/amr"},

	".webm": {fileDocument, "video/webm"},
	".mkv":  {fileDocument, "video/x-matroska"},
	".avi":  {fileDocument, "video/x-msvideo"},
	".flac": {fileDocument, "audio/flac"},
	".pdf":  {fileDocument, "application/pdf"},
	".txt":  {fileDocument, "text/plain"},
	".zip":  {fileDocument, "application/zip"},
	".doc":  {fileDocument, "application/msword"},
	".docx": {fileDocument, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	".xls":  {fileDocument, "application/vnd.ms-excel"},
	".xlsx": {fileDocument, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	".ppt":  {fileDocument, "application/vnd.ms-powerpoint"},
	".pptx": {fileDocument, "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	".csv":  {fileDocument, "text/csv"},
	".apk":  {fileDocument, "application/vnd.android.package-archive"},
}

// sniffTypes maps what http.DetectContentType reports to how the file is
// sent, for files whose extension says nothing.
var sniffTypes = map[string]fileType{
	"image/jpeg":      {fileImage, "image/jpeg"},
	"image/png":       {fileImage, "image/png"},
	"image/webp":      {fileImage, "image/webp"},
	"image/gif":       {fileImage, "image/gif"},
	"image/bmp":       {fileImage, "image/bmp"},
	"video/mp4":       {fileVideo, "video/mp4"},
	"application/ogg": {fileAudio, oggOpus},
	"audio/mpeg":      {fileAudio, "audio/mpeg"},
	"audio/wave":      {fileAudio, "audio/wav"},
}

// detectFile decides how to send a file from its name, falling back to
// its first bytes (head) when the extension is unknown.
func detectFile(name string, head []byte) (fileKind, string) {
	ext := strings.ToLower(filepath.Ext(name))
	if ft, ok := fileTypes[ext]; ok {
		return ft.kind, ft.mime
	}
	sniffed := http.DetectContentType(head)
	if ft, ok := sniffTypes[sniffed]; ok {
		return ft.kind, ft.mime
	}
	if byExt := mime.TypeByExtension(ext); byExt != "" {
		return fileDocument, byExt
	}
	return fileDocument, sniffed // "application/octet-stream" at worst
}

// maxImageSide is the longest side an image may have to be sent as it
// is, about what WhatsApp itself sends as "HD". maxImageBytes is the
// largest file sent as an image rather than a document.
const (
	maxImageSide  = 4096
	maxImageBytes = 16 << 20
)

// preparedFile is a file ready to upload.
type preparedFile struct {
	kind  fileKind
	mime  string
	name  string // base name, for documents
	path  string // the file to stream, when data is nil
	data  []byte // re-encoded image to upload instead of the file
	size  int64
	voice bool

	width, height int
	seconds       int
	thumb         []byte // JPEG
}

// prepareFile inspects the file an Outgoing names and decides how it is
// sent. Only images are ever decoded (within termimg's memory budget);
// everything else is streamed from disk untouched.
func prepareFile(out Outgoing) (*preparedFile, error) {
	f, err := os.Open(out.FilePath)
	if err != nil {
		return nil, waerrors.Wrap(err, "opening file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, waerrors.Wrap(err, "reading file")
	}
	if st.IsDir() {
		return nil, waerrors.New(out.FilePath + " is a folder, not a file")
	}
	if st.Size() == 0 {
		return nil, waerrors.New(out.FilePath + " is empty")
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	p := &preparedFile{path: out.FilePath, name: filepath.Base(out.FilePath), size: st.Size()}
	p.kind, p.mime = detectFile(p.name, head)
	if out.AsDocument {
		p.kind = fileDocument
	}
	switch p.kind {
	case fileImage:
		if !p.prepareImage() {
			// Not an image after all, or too large to process in memory:
			// it still arrives intact as a document.
			p.kind, p.data = fileDocument, nil
		}
	case fileAudio:
		if strings.HasPrefix(p.mime, "audio/ogg") {
			p.seconds = oggDuration(f, st.Size())
			// A voice note is Opus in Ogg; nothing else plays as one.
			if out.Voice {
				p.voice, p.mime = true, oggOpus
			}
		}
	}
	return p, nil
}

// prepareImage fills in the image's details and reports whether the file
// can go as an image message. "HD" here means the original pixels: a
// JPEG or PNG that is upright and at most maxImageSide pixels long is
// sent byte for byte. Anything else (rotated by EXIF, larger, WebP, BMP,
// GIF) is decoded, turned upright, scaled down if needed and re-encoded
// as a high-quality JPEG.
func (p *preparedFile) prepareImage() bool {
	info, err := termimg.Probe(p.path)
	if err != nil || info.Width <= 0 || info.Height <= 0 {
		return false
	}
	format := strings.ToLower(info.Format)
	asIs := (format == "jpeg" || format == "png") &&
		info.Orientation <= 1 &&
		max(info.Width, info.Height) <= maxImageSide &&
		p.size <= maxImageBytes

	img, _, err := termimg.Load(p.path, int64(termimg.DefaultBudget))
	if err != nil {
		if !asIs {
			return false
		}
		// Sent as it is, just without a thumbnail.
		p.mime, p.width, p.height = "image/"+format, info.Width, info.Height
		return true
	}
	if asIs {
		p.mime, p.width, p.height = "image/"+format, info.Width, info.Height
		p.thumb, _ = termimg.Thumbnail(img, 100, 50)
		return true
	}

	img = termimg.Orient(img, info.Orientation)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if max(w, h) > maxImageSide {
		w, h = scaledSize(w, h, int64(termimg.DefaultBudget))
		img = termimg.Scale(img, w, h)
	}
	data, err := termimg.EncodeJPEG(img, 92)
	if err != nil {
		return false
	}
	p.data, p.size = data, int64(len(data))
	p.mime, p.width, p.height = "image/jpeg", w, h
	p.thumb, _ = termimg.Thumbnail(img, 100, 50)
	return true
}

// scaledSize returns the size to scale a too-large w x h image down to:
// at most maxImageSide on its longest side, and less when the scaled
// copy (4 bytes per pixel, held next to the decoded original) would not
// fit the memory budget.
func scaledSize(w, h int, budget int64) (int, int) {
	side := maxImageSide
	for {
		sw, sh := termimg.Fit(w, h, side, side)
		if int64(sw)*int64(sh)*4 <= budget*3/4 || side <= 1600 {
			return sw, sh
		}
		side -= 256
	}
}

// oggDuration returns the length in seconds of an Ogg Opus file, read
// from the last page's position (Opus always counts 48000 per second),
// or 0 if it can't tell.
func oggDuration(f io.ReaderAt, size int64) int {
	const tail = 64 << 10
	start := max(size-tail, 0)
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return 0
	}
	at := bytes.LastIndex(buf, []byte("OggS"))
	if at < 0 || at+14 > len(buf) {
		return 0
	}
	granule := int64(binary.LittleEndian.Uint64(buf[at+6 : at+14]))
	if granule <= 0 {
		return 0
	}
	return int((granule + 47999) / 48000)
}

// message builds the message for the file: with the upload's references
// once up is set, or without them as the placeholder stored while the
// upload runs.
func (p *preparedFile) message(caption string, ci *waProto.ContextInfo, up *whatsmeow.UploadResponse) *waProto.Message {
	var (
		url, directPath               *string
		mediaKey, fileEncSHA, fileSHA []byte
		keyTimestamp                  *int64
		length                        = proto.Uint64(uint64(p.size))
		mimetype                      = proto.String(p.mime)
		captionField                  *string
		width, height, seconds        *uint32
	)
	if up != nil {
		url, directPath = proto.String(up.URL), proto.String(up.DirectPath)
		mediaKey, fileEncSHA, fileSHA = up.MediaKey, up.FileEncSHA256, up.FileSHA256
		keyTimestamp = proto.Int64(time.Now().Unix())
		if up.FileLength > 0 {
			length = proto.Uint64(up.FileLength)
		}
	}
	if caption != "" {
		captionField = proto.String(caption)
	}
	if p.width > 0 && p.height > 0 {
		width, height = proto.Uint32(uint32(p.width)), proto.Uint32(uint32(p.height))
	}
	if p.seconds > 0 {
		seconds = proto.Uint32(uint32(p.seconds))
	}

	switch p.kind {
	case fileImage:
		return &waProto.Message{ImageMessage: &waProto.ImageMessage{
			URL: url, DirectPath: directPath, MediaKey: mediaKey, MediaKeyTimestamp: keyTimestamp,
			FileEncSHA256: fileEncSHA, FileSHA256: fileSHA, FileLength: length, Mimetype: mimetype,
			Width: width, Height: height, JPEGThumbnail: p.thumb,
			Caption: captionField, ContextInfo: ci,
		}}
	case fileVideo:
		return &waProto.Message{VideoMessage: &waProto.VideoMessage{
			URL: url, DirectPath: directPath, MediaKey: mediaKey, MediaKeyTimestamp: keyTimestamp,
			FileEncSHA256: fileEncSHA, FileSHA256: fileSHA, FileLength: length, Mimetype: mimetype,
			Caption: captionField, ContextInfo: ci,
		}}
	case fileAudio:
		return &waProto.Message{AudioMessage: &waProto.AudioMessage{
			URL: url, DirectPath: directPath, MediaKey: mediaKey, MediaKeyTimestamp: keyTimestamp,
			FileEncSHA256: fileEncSHA, FileSHA256: fileSHA, FileLength: length, Mimetype: mimetype,
			Seconds: seconds, PTT: proto.Bool(p.voice), ContextInfo: ci,
		}}
	}
	doc := &waProto.DocumentMessage{
		URL: url, DirectPath: directPath, MediaKey: mediaKey, MediaKeyTimestamp: keyTimestamp,
		FileEncSHA256: fileEncSHA, FileSHA256: fileSHA, FileLength: length, Mimetype: mimetype,
		FileName: proto.String(p.name), Title: proto.String(p.name),
		Caption: captionField, ContextInfo: ci,
	}
	return documentMessage(doc)
}

// documentMessage wraps a document the way WhatsApp's apps do: one with a
// caption travels inside a "document with caption" envelope, without
// which the caption isn't shown.
func documentMessage(doc *waProto.DocumentMessage) *waProto.Message {
	msg := &waProto.Message{DocumentMessage: doc}
	if doc.GetCaption() == "" {
		return msg
	}
	return &waProto.Message{DocumentWithCaptionMessage: &waProto.FutureProofMessage{Message: msg}}
}

// upload sends the prepared file to WhatsApp's media servers: streamed
// from disk (through an encrypted temporary file whatsmeow manages), or
// from memory for a re-encoded image.
func (c *Client) upload(ctx context.Context, p *preparedFile) (whatsmeow.UploadResponse, error) {
	if !c.connected() {
		// Don't encrypt a large file to disk just to find this out.
		return whatsmeow.UploadResponse{}, whatsmeow.ErrNotConnected
	}
	if p.data != nil {
		return c.WA.Upload(ctx, p.data, p.kind.mediaType())
	}
	f, err := os.Open(p.path)
	if err != nil {
		return whatsmeow.UploadResponse{}, err
	}
	defer f.Close()
	return c.WA.UploadReader(ctx, f, nil, p.kind.mediaType())
}

// sendFile sends the file an Outgoing names under message ID id: the
// placeholder row is stored and announced first, then the file is
// uploaded and the message sent.
func (c *Client) sendFile(ctx context.Context, chat types.JID, id string, out Outgoing) error {
	p, err := prepareFile(out)
	if err != nil {
		return err
	}
	ci := c.outgoingContext(chat.String(), out.ReplyTo, out.Mentions)
	c.recordOutgoing(outgoingRow(chat.String(), id, p.message(out.Text, ci, nil)))

	c.sendMu.Lock()
	if c.pendingFiles == nil {
		c.pendingFiles = map[string]Outgoing{}
	}
	c.pendingFiles[id] = out
	c.sendMu.Unlock()

	return c.finishFile(ctx, chat, id, out, p, ci)
}

// uploadAndSend redoes a file send whose upload never finished (Retry).
func (c *Client) uploadAndSend(ctx context.Context, chat types.JID, id string, out Outgoing) error {
	p, err := prepareFile(out)
	if err != nil {
		c.finishSend(chat.String(), id, err)
		return err
	}
	return c.finishFile(ctx, chat, id, out, p, c.outgoingContext(chat.String(), out.ReplyTo, out.Mentions))
}

// finishFile uploads a prepared file, replaces the stored placeholder
// with the real message (so it can be downloaded or forwarded later) and
// sends it.
func (c *Client) finishFile(ctx context.Context, chat types.JID, id string, out Outgoing, p *preparedFile, ci *waProto.ContextInfo) error {
	var up whatsmeow.UploadResponse
	err := c.withRetry(ctx, func() (err error) {
		up, err = c.upload(ctx, p)
		return err
	})
	if err != nil {
		c.finishSend(chat.String(), id, err)
		return waerrors.Wrap(err, "uploading "+p.kind.String())
	}

	msg := p.message(out.Text, ci, &up)
	if c.msgs != nil {
		if cur, ok, err := c.msgs.Get(chat.String(), id); err == nil && ok {
			cur.RawProto = encodeRawProto(msg)
			if err := c.msgs.Put(cur); err != nil {
				c.log.Warnf("storing uploaded %s %s failed: %v", p.kind, id, err)
			}
		}
	}
	if err := c.transmit(ctx, chat, id, msg); err != nil {
		return waerrors.Wrap(err, "sending "+p.kind.String())
	}
	return nil
}

// --- the CLI's media calls: content already in memory ---

// uploadBytes uploads data as the given media type and returns the
// references to put into a message.
func (c *Client) uploadBytes(ctx context.Context, data []byte, kind fileKind) (up whatsmeow.UploadResponse, err error) {
	err = c.withRetry(ctx, func() (err error) {
		up, err = c.WA.Upload(ctx, data, kind.mediaType())
		return err
	})
	if err != nil {
		return up, waerrors.Wrap(err, "uploading "+kind.String())
	}
	return up, nil
}

// sendUploaded sends an already-uploaded media message built by
// prepared.message, applying the chat's disappearing timer.
func (c *Client) sendUploaded(ctx context.Context, jid types.JID, p *preparedFile, caption string, up whatsmeow.UploadResponse, what string) (string, error) {
	chat := c.canonicalChat(jid)
	msg := p.message(caption, c.outgoingContext(chat.String(), nil, nil), &up)
	id, err := c.deliver(ctx, chat, msg)
	if err != nil {
		return "", waerrors.Wrap(err, "sending "+what)
	}
	return id, nil
}

// SendImage uploads data and sends it as an image message with an
// optional caption.
func (c *Client) SendImage(ctx context.Context, jid types.JID, data []byte, mimetype, caption string) (string, error) {
	p := &preparedFile{kind: fileImage, mime: mimetype, size: int64(len(data))}
	if img, info, err := termimg.Decode(data, int64(termimg.DefaultBudget)); err == nil {
		p.width, p.height = info.Width, info.Height
		p.thumb, _ = termimg.Thumbnail(img, 100, 50)
	}
	up, err := c.uploadBytes(ctx, data, fileImage)
	if err != nil {
		return "", err
	}
	return c.sendUploaded(ctx, jid, p, caption, up, "image")
}

// SendVideo uploads data and sends it as a video message with an
// optional caption.
func (c *Client) SendVideo(ctx context.Context, jid types.JID, data []byte, mimetype, caption string) (string, error) {
	up, err := c.uploadBytes(ctx, data, fileVideo)
	if err != nil {
		return "", err
	}
	p := &preparedFile{kind: fileVideo, mime: mimetype, size: int64(len(data))}
	return c.sendUploaded(ctx, jid, p, caption, up, "video")
}

// SendAudio uploads data and sends it as an audio message. Set voice=true
// to send as a voice note (PTT) rather than a regular audio file.
func (c *Client) SendAudio(ctx context.Context, jid types.JID, data []byte, mimetype string, voice bool) (string, error) {
	up, err := c.uploadBytes(ctx, data, fileAudio)
	if err != nil {
		return "", err
	}
	p := &preparedFile{kind: fileAudio, mime: mimetype, size: int64(len(data)), voice: voice}
	if strings.HasPrefix(mimetype, "audio/ogg") {
		p.seconds = oggDuration(bytes.NewReader(data), int64(len(data)))
	}
	return c.sendUploaded(ctx, jid, p, "", up, "audio")
}

// SendDocument uploads data and sends it as a document message.
func (c *Client) SendDocument(ctx context.Context, jid types.JID, data []byte, mimetype, filename, caption string) (string, error) {
	up, err := c.uploadBytes(ctx, data, fileDocument)
	if err != nil {
		return "", err
	}
	p := &preparedFile{kind: fileDocument, mime: mimetype, name: filename, size: int64(len(data))}
	return c.sendUploaded(ctx, jid, p, caption, up, "document")
}

// SendSticker uploads data and sends it as a sticker message. data
// should already be in WebP format — WhatsApp doesn't accept other
// formats for stickers, and this doesn't convert for you.
func (c *Client) SendSticker(ctx context.Context, jid types.JID, data []byte, mimetype string) (string, error) {
	up, err := c.uploadBytes(ctx, data, fileImage)
	if err != nil {
		return "", err
	}
	chat := c.canonicalChat(jid)
	msg := &waProto.Message{StickerMessage: &waProto.StickerMessage{
		URL:               proto.String(up.URL),
		DirectPath:        proto.String(up.DirectPath),
		MediaKey:          up.MediaKey,
		MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		Mimetype:          proto.String(mimetype),
		FileEncSHA256:     up.FileEncSHA256,
		FileSHA256:        up.FileSHA256,
		FileLength:        proto.Uint64(uint64(len(data))),
		ContextInfo:       c.outgoingContext(chat.String(), nil, nil),
	}}
	id, err := c.deliver(ctx, chat, msg)
	if err != nil {
		return "", waerrors.Wrap(err, "sending sticker")
	}
	return id, nil
}
