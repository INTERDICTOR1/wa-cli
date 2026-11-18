package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/termimg"
)

func TestOpenable(t *testing.T) {
	shown := []string{
		"photo.jpg", "IMG_0001.JPEG", "clip.MP4", "voice.ogg", "song.mp3",
		"report.pdf", "notes.txt", "sheet.xlsx", "letter.docx", "slides.pptx",
	}
	for _, name := range shown {
		if !openable(filepath.Join("media", name)) {
			t.Errorf("%s should be opened with the default app", name)
		}
	}
	// Anything that Windows, a browser or Office would run rather than
	// show - and anything unknown.
	run := []string{
		"setup.exe", "invoice.pdf.exe", "run.bat", "x.cmd", "a.com", "saver.scr", "app.hta",
		"s.js", "s.vbs", "s.wsf", "s.ps1", "shortcut.lnk", "installer.msi", "tool.jar",
		"keys.reg", "link.url", "page.html", "drawing.svg", "macros.docm", "macros.xlsm",
		"archive.zip", "disk.iso", "noextension", "trailing.", "data.bin",
	}
	for _, name := range run {
		if openable(filepath.Join("media", name)) {
			t.Errorf("%s must not be handed to the system to open", name)
		}
	}
}

// recordLaunches stands in for starting the system's default app, so a
// test never really opens anything.
func recordLaunches(t *testing.T) *[]string {
	t.Helper()
	var launched []string
	real := launch
	launch = func(target string) error {
		launched = append(launched, target)
		return nil
	}
	t.Cleanup(func() { launch = real })
	return &launched
}

func TestOpeningAnAttachmentNeverRunsIt(t *testing.T) {
	launched := recordLaunches(t)
	h := newHarness(t, threeChats(), 100, 30)
	dir := t.TempDir()

	// A "document" that is really a program is downloaded, not started.
	h.exec(h.m.mediaSaved(mediaSavedMsg{path: filepath.Join(dir, "invoice.pdf.exe"), purpose: purposeOpen}))
	if len(*launched) != 0 {
		t.Fatalf("launched %q", *launched)
	}
	if !h.m.statusErr || !strings.Contains(h.m.status, "Not opened") || !strings.Contains(h.m.status, ".exe") {
		t.Errorf("status = %q", h.m.status)
	}
	if frame := h.frame(); !strings.Contains(frame, "Not opened") {
		t.Errorf("the refusal isn't on screen:\n%s", frame)
	}

	// A real document is opened.
	pdf := filepath.Join(dir, "report.pdf")
	h.exec(h.m.mediaSaved(mediaSavedMsg{path: pdf, purpose: purposeOpen}))
	if len(*launched) != 1 || (*launched)[0] != pdf {
		t.Fatalf("launched %q, want the pdf", *launched)
	}

	// The photo viewer's "open" obeys the same rule: something sent as a
	// photo but saved under another type stays closed.
	*launched = nil
	h.m.viewer = &viewer{path: filepath.Join(dir, "image-1.bin"), proto: termimg.Blocks}
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if len(*launched) != 0 {
		t.Fatalf("the viewer launched %q", *launched)
	}
	if h.m.viewer != nil || !strings.Contains(h.m.status, "Not opened") {
		t.Errorf("viewer open=%v status=%q", h.m.viewer != nil, h.m.status)
	}
	h.m.viewer = &viewer{path: filepath.Join(dir, "image-2.jpg"), proto: termimg.Blocks}
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if len(*launched) != 1 {
		t.Fatalf("the viewer launched %q, want the photo", *launched)
	}
}
