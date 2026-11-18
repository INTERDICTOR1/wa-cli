package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/termimg"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// The photo viewer shows a picture as large and as sharp as the terminal
// can: with real pixels where the terminal has a graphics protocol (Sixel
// in Windows Terminal, Kitty, iTerm2), otherwise with half-block
// characters.
//
// Pixels can't go through Bubble Tea, which only knows text. So while the
// viewer is open, View() returns an empty frame with a caption and the
// picture is written straight to the terminal on top of it. Bubble Tea
// only repaints lines that change, and the empty frame never does, so the
// picture stays until the viewer closes and the screen is cleared.
//
// Memory: the photo is downloaded to disk (never held in RAM), decoded
// only if it fits termimg.DefaultBudget, scaled down to the screen
// straight away, and the decoded original is released before drawing.

type mediaPurpose int

const (
	purposeView mediaPurpose = iota // show in the viewer
	purposeOpen                     // open with the default app
	purposeSave                     // just keep the file
)

type mediaSavedMsg struct {
	view    whatsapp.MessageView
	path    string
	purpose mediaPurpose
	err     error
}

func saveMediaCmd(ctx context.Context, be Backend, v whatsapp.MessageView, dir string, purpose mediaPurpose) tea.Cmd {
	return func() tea.Msg {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return mediaSavedMsg{view: v, purpose: purpose, err: err}
		}
		path, err := be.SaveMedia(ctx, v.Message, dir)
		return mediaSavedMsg{view: v, path: path, purpose: purpose, err: err}
	}
}

func (m *model) mediaSaved(msg mediaSavedMsg) tea.Cmd {
	if msg.err != nil {
		return m.fail(msg.err)
	}
	switch msg.purpose {
	case purposeOpen:
		if !openable(msg.path) {
			return m.setStatus(notOpenedText(msg.path), true)
		}
		if err := launch(msg.path); err != nil {
			return m.fail(err)
		}
		return m.info("Opened " + filepath.Base(msg.path))
	case purposeSave:
		return m.info("Saved to " + msg.path)
	}
	m.status = ""
	m.viewer = &viewer{msg: msg.view, path: msg.path, proto: m.opts.ImageProtocol}
	return m.viewer.redraw(m)
}

type viewer struct {
	msg   whatsapp.MessageView
	path  string
	proto termimg.Protocol

	seq     int    // bumps on every redraw, to drop a stale result
	text    string // the picture as text (half-block mode)
	loading bool
	note    string // why there's no picture, if so
	info    termimg.Info
}

type viewerDrawnMsg struct {
	seq  int
	text string
	info termimg.Info
	err  error
}

// redraw (re)draws the picture for the current terminal size and mode.
func (v *viewer) redraw(m *model) tea.Cmd {
	v.seq++
	v.loading, v.text, v.note = true, "", ""
	return tea.Sequence(tea.ClearScreen, drawPhotoCmd(m.out, v.path, v.proto, m.width, m.height-1, v.seq))
}

func (v *viewer) drawn(msg viewerDrawnMsg) {
	v.loading, v.text, v.info = false, msg.text, msg.info
	switch {
	case errors.Is(msg.err, termimg.ErrTooLarge):
		v.note = "This photo is too large to draw here within the memory limit. Press o to open it in your image viewer."
	case msg.err != nil:
		v.note = "Couldn't show this photo: " + msg.err.Error() + " (press o to open it in your image viewer)"
	}
}

// drawPhotoCmd decodes the photo, fits it to cols x rows cells and puts
// it on screen: as text returned to View (half blocks) or written directly
// to the terminal (graphics protocols).
func drawPhotoCmd(out io.Writer, path string, proto termimg.Protocol, cols, rows, seq int) tea.Cmd {
	return func() tea.Msg {
		started := time.Now()
		// Decoded pixels are garbage as soon as this returns; hand the
		// memory back to the OS rather than sitting on it.
		defer debug.FreeOSMemory()

		if proto == termimg.ITerm {
			// iTerm2 takes the file itself: no decoding, full quality.
			info, err := termimg.Probe(path)
			if err != nil {
				return viewerDrawnMsg{seq: seq, err: err}
			}
			f, err := os.Open(path)
			if err != nil {
				return viewerDrawnMsg{seq: seq, err: err}
			}
			defer f.Close()
			waitForBlankFrame(started)
			w := bufio.NewWriterSize(out, 64<<10)
			fmt.Fprint(w, "\x1b7\x1b[H")
			err = termimg.WriteITerm(w, f, info.Size, filepath.Base(path), cols, rows)
			fmt.Fprint(w, "\x1b8")
			if ferr := w.Flush(); err == nil {
				err = ferr
			}
			return viewerDrawnMsg{seq: seq, info: info, err: err}
		}

		img, info, err := termimg.Load(path, termimg.DefaultBudget)
		if err != nil {
			return viewerDrawnMsg{seq: seq, info: info, err: err}
		}
		img = termimg.Orient(img, info.Orientation)
		b := img.Bounds()

		if proto == termimg.Blocks {
			return viewerDrawnMsg{seq: seq, info: info, text: termimg.HalfBlocks(img, cols, rows)}
		}

		// Scale to the screen's pixel size (never up), then draw centred.
		cw, ch := termimg.CellPixels(proto)
		pw, ph := termimg.Fit(b.Dx(), b.Dy(), cols*cw, rows*ch)
		var scaled image.Image = termimg.Scale(img, pw, ph)
		img = nil // let the full-size pixels go before encoding

		col := max(1, (cols-(pw+cw-1)/cw)/2+1)
		row := max(1, (rows-(ph+ch-1)/ch)/2+1)
		waitForBlankFrame(started)
		w := bufio.NewWriterSize(out, 64<<10)
		fmt.Fprintf(w, "\x1b7\x1b[%d;%dH", row, col)
		if proto == termimg.Kitty {
			err = termimg.WriteKitty(w, scaled)
		} else {
			err = termimg.WriteSixel(w, scaled, 256)
		}
		fmt.Fprint(w, "\x1b8")
		if ferr := w.Flush(); err == nil {
			err = ferr
		}
		return viewerDrawnMsg{seq: seq, info: info, err: err}
	}
}

// waitForBlankFrame gives Bubble Tea time to paint the viewer's empty
// frame (it paints at 60 frames a second) before the picture is drawn
// over it; otherwise that paint would wipe the picture.
func waitForBlankFrame(started time.Time) {
	if d := 120*time.Millisecond - time.Since(started); d > 0 {
		time.Sleep(d)
	}
}

// view is the frame under the picture: empty lines and a caption. In
// half-block mode the picture itself is part of the frame.
func (v *viewer) view(m *model) string {
	rows := max(1, m.height-1)
	lines := make([]string, 0, m.height)
	if v.text != "" {
		pic := strings.Split(v.text, "\n")
		for i := 0; i < (rows-len(pic))/2; i++ {
			lines = append(lines, "")
		}
		for _, l := range pic {
			lines = append(lines, center(l, m.width))
		}
	} else if v.note != "" {
		for i := 0; i < rows/2; i++ {
			lines = append(lines, "")
		}
		lines = append(lines, center(statusStyle.Render(truncate(oneLine(v.note), m.width-2)), m.width))
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}

	who := oneLine(v.msg.Sender)
	if v.msg.FromMe {
		who = "You"
	}
	parts := []string{who}
	if v.info.Width > 0 {
		parts = append(parts, itoa(v.info.Width)+"×"+itoa(v.info.Height))
	}
	if v.msg.HD {
		parts = append(parts, "HD")
	}
	if size := humanSize(uint64(v.info.Size)); size != "" {
		parts = append(parts, size)
	}
	if v.loading {
		parts = append(parts, "loading…")
	}
	caption := strings.Join(parts, " · ") + "    " + v.proto.String() + " · m mode · o open · s save · any key closes"
	return strings.Join(lines[:rows], "\n") + "\n" + helpStyle.Render(truncate(caption, m.width))
}

func (v *viewer) key(m *model, msg tea.KeyMsg) tea.Cmd {
	switch keyText(msg) {
	case "m":
		// Next drawing mode. A mode the terminal doesn't understand shows
		// nothing (terminals ignore graphics they can't draw): press m again.
		v.clear(m)
		v.proto = (v.proto + 1) % 4
		return v.redraw(m)
	case "o":
		if openable(v.path) {
			if err := launch(v.path); err != nil {
				return m.fail(err)
			}
			return nil
		}
		// Sent as a photo, but not a type that is safe to hand over.
		v.clear(m)
		m.viewer = nil
		return tea.Batch(tea.ClearScreen, m.setStatus(notOpenedText(v.path), true))
	case "s":
		dst, err := copyFile(v.path, m.opts.DownloadDir)
		v.clear(m)
		m.viewer = nil
		if err != nil {
			return tea.Batch(tea.ClearScreen, m.fail(err))
		}
		return tea.Batch(tea.ClearScreen, m.info("Saved to "+dst))
	}
	v.clear(m)
	m.viewer = nil
	return tea.ClearScreen
}

// clear removes pictures that outlive a screen clear (Kitty keeps images
// until told to delete them).
func (v *viewer) clear(m *model) {
	if v.proto == termimg.Kitty {
		fmt.Fprint(m.out, "\x1b_Ga=d\x1b\\")
	}
}

// copyFile copies src into dir, returning the new path.
func copyFile(src, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if dst == src {
		return dst, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	return dst, out.Close()
}

// --- clipboard pictures ---

type clipboardImageMsg struct {
	path string
	err  error
}

// pasteImageCmd saves the picture on the clipboard (a screenshot, a
// copied image) to a file so it can be attached. Terminals only paste
// text, so this asks the system directly; Windows only for now.
func pasteImageCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		if runtime.GOOS != "windows" {
			return clipboardImageMsg{err: errors.New("pasting pictures from the clipboard is only supported on Windows")}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return clipboardImageMsg{err: err}
		}
		path := filepath.Join(dir, "clipboard-"+time.Now().Format("20060102-150405")+".png")
		// The path goes in through the environment, not the script text,
		// so nothing in it can be read as PowerShell.
		script := `Add-Type -AssemblyName System.Windows.Forms,System.Drawing; ` +
			`$i=[System.Windows.Forms.Clipboard]::GetImage(); ` +
			`if($i){$i.Save($env:WA_CLIP_PATH,[System.Drawing.Imaging.ImageFormat]::Png); 'ok'}`
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
		cmd.Env = append(os.Environ(), "WA_CLIP_PATH="+path)
		out, err := cmd.Output()
		if err != nil {
			return clipboardImageMsg{err: fmt.Errorf("reading the clipboard: %w", err)}
		}
		if !strings.Contains(string(out), "ok") {
			return clipboardImageMsg{err: errors.New("there's no picture on the clipboard")}
		}
		return clipboardImageMsg{path: path}
	}
}
