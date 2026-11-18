package conin

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Under "go test" stdin is not a console (and on other systems there is no
// Windows console at all), which is exactly the case where Start must step
// aside by returning (nil, nil) — and where the caller will go on to call
// Stop and Divert on that nil reader.
func TestStartWithoutConsole(t *testing.T) {
	reader, err := Start(func(tea.Msg) {}, Options{Mouse: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if reader != nil {
		// Someone ran the test binary by hand in a console window.
		reader.Stop()
		t.Skip("stdin is a console here")
	}

	// A nil *Reader must be safe to use.
	reader.Divert(func(tea.Msg) bool { return true })
	reader.Divert(nil)
	reader.Stop()
	reader.Stop()
}
