package tui

import (
	"strings"
	"testing"
)

func TestRAMCounterInHeader(t *testing.T) {
	h := newHarness(t, threeChats(), 100, 30)
	if h.m.mem == 0 {
		t.Skip("memory use can't be measured here")
	}
	if frame := h.frame(); !strings.Contains(strings.SplitN(frame, "\n", 2)[0], "RAM ") {
		t.Fatalf("no RAM counter on the top line:\n%s", frame)
	}

	// The peak is kept and shown once the current figure drops below it;
	// above the budget the counter is drawn as a warning.
	h.send(memSampleMsg{bytes: 120 << 20, ok: true})
	if !strings.Contains(h.m.memText(), "RAM 120 MB") || h.m.memText() == dimStyle.Render("RAM 120 MB") {
		t.Errorf("over budget: %q", h.m.memText())
	}
	h.send(memSampleMsg{bytes: 40 << 20, ok: true})
	if got := h.m.memText(); got != dimStyle.Render("RAM 40 MB · peak 120 MB") {
		t.Errorf("after the peak: %q", got)
	}
	if frame := h.frame(); !strings.Contains(frame, "RAM 40 MB · peak 120 MB") {
		t.Errorf("counter not drawn:\n%s", frame)
	}

	// It never pushes the frame past a narrow terminal's width.
	h.send(memSampleMsg{bytes: 40 << 20, ok: true})
	narrow := newHarness(t, threeChats(), 40, 12)
	narrow.m.mem, narrow.m.memPeak = 99<<20, 999<<20
	narrow.frame()
}
