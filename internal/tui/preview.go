package tui

import (
	"context"
	"fmt"
	"image"
	"runtime/debug"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// Maximum preview size in terminal cells. Each cell shows two pixels
// stacked vertically, so this is up to 32x24 pixels.
const previewCols, previewRows = 32, 12

// previewCache holds rendered image previews by message ID ("" means the
// preview failed and shouldn't be retried). It's shared by pointer across
// model copies and used from tea.Cmd goroutines, hence the locks.
type previewCache struct {
	mu       sync.Mutex
	rendered map[string]string
	openChat string

	// job is held while fetching, so only one image is ever downloaded
	// and decoded at a time — that's what bounds the memory cost.
	job sync.Mutex
}

func newPreviewCache() *previewCache {
	return &previewCache{rendered: map[string]string{}}
}

func (p *previewCache) get(id string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.rendered[id]
	return s, ok
}

func (p *previewCache) set(id, s string) {
	p.mu.Lock()
	p.rendered[id] = s
	p.mu.Unlock()
}

func (p *previewCache) setOpen(chatJID string) {
	p.mu.Lock()
	p.openChat = chatJID
	p.mu.Unlock()
}

func (p *previewCache) isOpen(chatJID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openChat == chatJID
}

type previewsReadyMsg struct{ chatJID string }

// needsPreview reports whether msg is an image/video without a cached
// preview attempt yet.
func (p *previewCache) needsPreview(msg msgstore.Message) bool {
	if msg.MediaType != "image" && msg.MediaType != "video" {
		return false
	}
	_, done := p.get(msg.ID)
	return !done
}

// loadPreviewsCmd renders previews for chatJID's image/video messages,
// one at a time, stopping early if another chat is opened meanwhile.
func loadPreviewsCmd(ctx context.Context, client *whatsapp.Client, cache *previewCache, chatJID string, msgs []displayMsg) tea.Cmd {
	return func() tea.Msg {
		cache.job.Lock()
		defer cache.job.Unlock()
		for _, dm := range msgs {
			if !cache.isOpen(chatJID) {
				break
			}
			if !cache.needsPreview(dm.Message) {
				continue
			}
			img, err := client.ImagePreview(ctx, dm.Message)
			if err != nil {
				dlog("preview %s: %v", dm.ID, err)
				cache.set(dm.ID, "")
				continue
			}
			cache.set(dm.ID, halfBlocks(img, previewCols, previewRows))
		}
		// Decoded images are garbage now; give the memory back to Windows
		// rather than letting Task Manager show it as still in use.
		debug.FreeOSMemory()
		return previewsReadyMsg{chatJID: chatJID}
	}
}

// halfBlocks draws img in at most maxCols x maxRows terminal cells using
// "▀": the top pixel is the text colour, the bottom one the background.
func halfBlocks(img image.Image, maxCols, maxRows int) string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return ""
	}
	// Pixels are square: cols wide, 2*rows tall.
	cols := min(maxCols, w)
	pxRows := max(2, cols*h/w)
	if pxRows > 2*maxRows {
		pxRows = 2 * maxRows
		cols = max(1, pxRows*w/h)
	}
	pxRows += pxRows % 2

	at := func(x, y int) lipgloss.Color {
		r, g, bl, _ := img.At(b.Min.X+x*w/cols, b.Min.Y+min(y, pxRows-1)*h/pxRows).RGBA()
		return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, bl>>8))
	}
	var sb strings.Builder
	for y := 0; y < pxRows; y += 2 {
		if y > 0 {
			sb.WriteByte('\n')
		}
		for x := 0; x < cols; x++ {
			sb.WriteString(lipgloss.NewStyle().Foreground(at(x, y)).Background(at(x, y+1)).Render("▀"))
		}
	}
	return sb.String()
}
