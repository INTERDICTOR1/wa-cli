// Command echo shows what package conin makes of the console's input: it
// prints one line per message received.
//
// It has two uses. The ConPTY tests in package conin run it inside a pseudo
// console, type and paste at it, and read back what it saw. And a person can
// run it in any terminal to see how that terminal delivers keys and pastes:
//
//	go run ./internal/conin/internal/echo          (press F12 to quit)
//	go run ./internal/conin/internal/echo -tea     (the same through Bubble Tea; q quits)
//
// Line format, one message per line:
//
//	MSG at=<unix microseconds> key type="enter" runes="" paste=false alt=false
//	MSG at=<unix microseconds> size w=120 h=30
package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/conin"
)

func main() {
	outPath := flag.String("out", "", "append the lines to this file instead of printing them")
	mouse := flag.Bool("mouse", false, "also report mouse events")
	bracketed := flag.Bool("bp", false, "ask the terminal for bracketed paste first, as Bubble Tea does")
	teaMode := flag.Bool("tea", false, "run a small Bubble Tea program fed by conin")
	flag.Parse()

	log := newLogger(*outPath)
	if *teaMode {
		os.Exit(runTea(log, *mouse))
	}
	os.Exit(runPlain(log, *mouse, *bracketed))
}

// runPlain prints every message conin delivers until F12 is pressed.
func runPlain(log *logger, mouse, bracketed bool) int {
	inBefore, _ := consoleModes()
	if bracketed {
		fmt.Print("\x1b[?2004h")
	}

	quit := make(chan struct{})
	var once sync.Once
	reader, err := conin.Start(func(msg tea.Msg) {
		log.printf("MSG at=%d %s", time.Now().UnixMicro(), format(msg))
		if k, ok := msg.(tea.KeyMsg); ok && k.Type == tea.KeyF12 {
			once.Do(func() { close(quit) })
		}
	}, conin.Options{Mouse: mouse})
	if err != nil {
		log.printf("ERROR %v", err)
		return 1
	}
	if reader == nil {
		log.printf("UNSUPPORTED stdin is not a Windows console")
		return 2
	}

	inNow, _ := consoleModes()
	log.printf("READY in-before=%#x in-now=%#x", inBefore, inNow)
	<-quit

	reader.Stop()
	reader.Stop() // a second Stop must be harmless
	if bracketed {
		fmt.Print("\x1b[?2004l")
	}
	inAfter, _ := consoleModes()
	log.printf("STOPPED in-after=%#x", inAfter)
	return 0
}

// format renders a message as the part of a line after "MSG at=…".
func format(msg tea.Msg) string {
	switch m := msg.(type) {
	case tea.KeyMsg:
		return fmt.Sprintf("key type=%q runes=%q paste=%v alt=%v", m.Type.String(), string(m.Runes), m.Paste, m.Alt)
	case tea.MouseMsg:
		return fmt.Sprintf("mouse event=%q x=%d y=%d", m.String(), m.X, m.Y)
	case tea.WindowSizeMsg:
		return fmt.Sprintf("size w=%d h=%d", m.Width, m.Height)
	case tea.FocusMsg:
		return "focus"
	case tea.BlurMsg:
		return "blur"
	}
	return fmt.Sprintf("other %T", msg)
}

// logger writes lines either to a file (for the tests) or to the terminal.
type logger struct {
	mu   sync.Mutex
	file *os.File // nil: print to the terminal
}

func newLogger(path string) *logger {
	if path == "" {
		return &logger{}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return &logger{file: f}
}

func (l *logger) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := fmt.Sprintf(format, args...)
	if l.file != nil {
		fmt.Fprintln(l.file, line)
		return
	}
	// The console is in raw mode: a line feed alone does not return the
	// cursor to the left edge, so send CR LF.
	fmt.Print(line + "\r\n")
}
