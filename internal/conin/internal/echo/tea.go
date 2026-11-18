package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/conin"
)

// model is a minimal Bubble Tea program: it remembers the window size and
// the last key, and quits on q. Every message it receives is logged, which
// is how the smoke test proves that messages sent through conin reach
// Update.
type model struct {
	log           *logger
	running       *sync.Once // logs the console modes once the program runs
	width, height int
	last          string
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.running.Do(func() {
		in, out := consoleModes()
		m.log.printf("RUNNING in=%#x out=%#x", in, out)
	})
	m.log.printf("TEA at=%d %s", time.Now().UnixMicro(), format(msg))

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		m.last = msg.String()
		if m.last == "q" || msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() string {
	return fmt.Sprintf("conin smoke test\nsize=%dx%d\nlast=%q\npress q to quit", m.width, m.height, m.last)
}

// runTea wires conin into Bubble Tea exactly the way the real TUI does.
func runTea(log *logger, mouse bool) int {
	inBefore, outBefore := consoleModes()
	log.printf("BEFORE in=%#x out=%#x", inBefore, outBefore)

	// tea.WithInput(nil) tells Bubble Tea not to read the keyboard itself.
	p := tea.NewProgram(model{log: log, running: new(sync.Once)},
		tea.WithInput(nil),
		tea.WithAltScreen(),
		tea.WithOutput(os.Stdout),
	)

	// Start before Run: messages that arrive early simply wait in conin's
	// queue until the program is ready for them.
	reader, err := conin.Start(p.Send, conin.Options{Mouse: mouse})
	if err != nil {
		log.printf("ERROR %v", err)
		return 1
	}
	if reader == nil {
		log.printf("UNSUPPORTED stdin is not a Windows console")
		return 2
	}
	log.printf("STARTED")

	_, err = p.Run()
	reader.Stop()

	inAfter, outAfter := consoleModes()
	log.printf("EXIT in=%#x out=%#x err=%v", inAfter, outAfter, err)
	return 0
}
