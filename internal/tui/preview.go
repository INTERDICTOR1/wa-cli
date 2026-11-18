package tui

import (
	"context"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/termimg"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// previewLimit bounds how many drawn pictures are remembered. A picture is
// a few kilobytes of text, so this is a couple of megabytes at most.
const previewLimit = 300

type previewState int

const (
	previewNone   previewState = iota // not tried yet
	previewReady                      // drawn
	previewFailed                     // tried, no picture available
)

// previewCache holds the small in-chat pictures, already drawn as text,
// by message ID. It is used from the UI and from background loads, hence
// the lock.
type previewCache struct {
	mu       sync.Mutex
	rendered map[string]preview
	order    []string // oldest first, for eviction
	openChat string
	loading  bool // a load is running: only one at a time, to bound memory
}

// preview is one drawn picture and the size it was drawn for. text ""
// means there is no picture to show (don't try again).
type preview struct {
	text       string
	cols, rows int
}

func newPreviewCache() *previewCache {
	return &previewCache{rendered: map[string]preview{}}
}

// get returns the picture drawn for a cols x rows box. One drawn for a
// different size (the window was resized) counts as not there yet.
func (p *previewCache) get(id string, cols, rows int) (string, previewState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pv, ok := p.rendered[id]
	switch {
	case !ok:
		return "", previewNone
	case pv.text == "":
		return "", previewFailed
	case pv.cols != cols || pv.rows != rows:
		return "", previewNone
	}
	return pv.text, previewReady
}

func (p *previewCache) set(id, text string, cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.rendered[id]; !ok {
		p.order = append(p.order, id)
	}
	p.rendered[id] = preview{text, cols, rows}
	if len(p.order) > previewLimit {
		drop := len(p.order) - previewLimit*2/3
		for _, old := range p.order[:drop] {
			delete(p.rendered, old)
		}
		p.order = append([]string(nil), p.order[drop:]...)
	}
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

// begin claims the single loading slot; false if a load is already running.
func (p *previewCache) begin() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loading {
		return false
	}
	p.loading = true
	return true
}

func (p *previewCache) end() {
	p.mu.Lock()
	p.loading = false
	p.mu.Unlock()
}

// previewBox returns the size in terminal cells of v's in-chat picture, or
// 0, 0 if it gets none. Each cell shows two pixels stacked, so a picture
// is 2*rows pixels tall. Known in advance from the message's own
// dimensions, so space can be reserved before the picture loads.
func previewBox(v *whatsapp.MessageView, bw int) (cols, rows int) {
	mi := v.Media
	if mi == nil || v.Deleted || mi.ViewOnce {
		return 0, 0
	}
	maxCols, maxRows := 0, 0
	w, h := mi.Width, mi.Height
	switch v.Kind {
	case whatsapp.KindImage, whatsapp.KindVideo, whatsapp.KindGIF:
		if !mi.HasThumb && v.Kind != whatsapp.KindImage {
			return 0, 0
		}
		maxCols, maxRows = 40, 16
		if w <= 0 || h <= 0 {
			w, h = 4, 3
		}
	case whatsapp.KindSticker:
		maxCols, maxRows = 18, 9
		if w <= 0 || h <= 0 {
			w, h = 1, 1
		}
	case whatsapp.KindDocument:
		if !mi.HasThumb {
			return 0, 0
		}
		maxCols, maxRows = 22, 12
		if w <= 0 || h <= 0 {
			w, h = 3, 4
		}
	default:
		return 0, 0
	}
	cols = min(maxCols, bw)
	pxRows := max(2, cols*h/w)
	if pxRows > 2*maxRows {
		pxRows = 2 * maxRows
		cols = max(1, pxRows*w/h)
	}
	return cols, (pxRows + 1) / 2
}

type previewsReadyMsg struct{ chatJID string }

type previewJob struct {
	view       whatsapp.MessageView
	cols, rows int
}

// wantPreviews starts loading the pictures of the messages on screen that
// don't have one yet. Only one load runs at a time and it stops when you
// switch chats; when it finishes, previewsReadyMsg calls this again to
// pick up whatever has scrolled into view meanwhile.
func (m *model) wantPreviews() tea.Cmd {
	if !m.ready {
		return nil // sizes aren't known before the first window size
	}
	from, to := m.visibleMsgs()
	bw := m.bubbleWidth()
	var jobs []previewJob
	for i := to - 1; i >= from; i-- { // newest first: that's where you're looking
		v := &m.msgs[i].view
		cols, rows := previewBox(v, bw)
		if cols == 0 {
			continue
		}
		if _, state := m.previews.get(v.ID, cols, rows); state == previewNone {
			jobs = append(jobs, previewJob{*v, cols, rows})
			if len(jobs) == 12 {
				break
			}
		}
	}
	if len(jobs) == 0 || !m.previews.begin() {
		return nil
	}
	return loadPreviewsCmd(m.ctx, m.be, m.previews, m.openJID, jobs)
}

func loadPreviewsCmd(ctx context.Context, be Backend, cache *previewCache, chatJID string, jobs []previewJob) tea.Cmd {
	return func() tea.Msg {
		defer cache.end()
		for _, job := range jobs {
			if !cache.isOpen(chatJID) {
				break
			}
			img, err := be.Thumbnail(ctx, job.view.Message)
			if err != nil {
				dlog("preview %s: %v", job.view.ID, err)
				cache.set(job.view.ID, "", 0, 0)
				continue
			}
			pic := termimg.HalfBlocks(img, job.cols, job.rows)
			// Exactly the reserved height, so the chat doesn't shift.
			lines := strings.Split(pic, "\n")
			for len(lines) < job.rows {
				lines = append(lines, "")
			}
			cache.set(job.view.ID, strings.Join(lines[:job.rows], "\n"), job.cols, job.rows)
		}
		return previewsReadyMsg{chatJID: chatJID}
	}
}
