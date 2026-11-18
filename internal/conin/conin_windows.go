//go:build windows

package conin

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/sys/windows"
)

// Reader is a running console input reader. Create one with Start.
type Reader struct {
	in       windows.Handle // the console's input queue (our stdin)
	out      windows.Handle // the console's screen, for asking its size; 0 if unavailable
	origMode uint32         // the input mode to put back in Stop

	stop     windows.Handle // an event object; signalled by Stop to end the read loop
	stopOnce sync.Once
	loopDone chan struct{} // closed when the read loop has returned

	translate translator
	delivery  *delivery
	trace     *tracer // nil unless WA_CONIN_TRACE is set
}

// Start begins reading the Windows console's input and forwards Bubble Tea
// messages to send. It returns (nil, nil) when unsupported — not Windows, or
// stdin is not a console (piped/redirected) — in which case the caller lets
// Bubble Tea read input itself.
//
// send is called from a goroutine of its own, one message at a time and in
// order; it may block (tea.Program.Send does until the program is running).
//
// Setting the environment variable WA_CONIN_TRACE to a file name makes the
// reader append everything it reads, and what it made of it, to that file.
// That is the tool for finding out what a particular terminal really sends.
func Start(send func(tea.Msg), opts Options) (*Reader, error) {
	if send == nil {
		return nil, errors.New("conin: send must not be nil")
	}

	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return nil, nil
	}
	var origMode uint32
	if err := windows.GetConsoleMode(in, &origMode); err != nil {
		// Only a console has a console mode: stdin is a pipe or a file.
		return nil, nil
	}

	// The input mode decides what the console delivers to us:
	//
	//   ENABLE_WINDOW_INPUT    report window resizes
	//   ENABLE_MOUSE_INPUT     report mouse events (only if asked for)
	//   ENABLE_EXTENDED_FLAGS  needed to change quick-edit mode; setting it
	//                          WITHOUT ENABLE_QUICK_EDIT_MODE turns quick-edit
	//                          off, so that clicks reach us instead of
	//                          starting a text selection
	//
	// Just as important is what is left out:
	//
	//   ENABLE_LINE_INPUT, ENABLE_ECHO_INPUT   we want every key as it
	//                          happens, not whole lines, and no echo
	//   ENABLE_PROCESSED_INPUT with it, Windows would swallow Ctrl+C and
	//                          raise an interrupt; without it Ctrl+C is a key
	//   ENABLE_VIRTUAL_TERMINAL_INPUT  with it, keys would arrive as escape
	//                          sequences without the modifier information
	//                          that lets us tell Shift+Enter from Enter
	mode := uint32(windows.ENABLE_WINDOW_INPUT | windows.ENABLE_EXTENDED_FLAGS)
	if opts.Mouse {
		mode |= windows.ENABLE_MOUSE_INPUT
	}
	if err := windows.SetConsoleMode(in, mode); err != nil {
		return nil, fmt.Errorf("conin: set console input mode: %w", err)
	}

	// A manual-reset event that starts out unsignalled. The read loop waits
	// on it together with the console, so Stop can wake the loop at any
	// moment simply by signalling it.
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.SetConsoleMode(in, origMode)
		return nil, fmt.Errorf("conin: create stop event: %w", err)
	}

	r := &Reader{
		in:       in,
		out:      openConsoleOutput(),
		origMode: origMode,
		stop:     stop,
		loopDone: make(chan struct{}),
		delivery: newDelivery(send),
		trace:    openTrace(),
	}
	r.translate.mouse = opts.Mouse
	r.translate.window = r.window

	go r.readLoop()
	return r, nil
}

// Stop restores the console's original input mode and ends the read loop.
// Safe to call more than once and on a nil *Reader.
func (r *Reader) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		_ = windows.SetEvent(r.stop)

		// The loop notices the event at once. The timeout only guards
		// against a console that has stopped answering; in that case the
		// handles are left open for the loop that may still be using them.
		stopped := false
		select {
		case <-r.loopDone:
			stopped = true
		case <-time.After(500 * time.Millisecond):
		}

		r.delivery.close()
		_ = windows.SetConsoleMode(r.in, r.origMode)

		if stopped {
			_ = windows.CloseHandle(r.stop)
			if r.out != 0 {
				_ = windows.CloseHandle(r.out)
			}
			r.trace.close()
		}
	})
}

// Divert routes every message to fn first while fn is non-nil; a message is
// dropped (not passed to send) when fn returns true. Divert(nil) clears it.
// Used while a full-screen image viewer owns the terminal and Bubble Tea's
// event loop is blocked. Safe on a nil *Reader.
//
// fn runs on the reader's own goroutine, so it must not touch the UI's state
// without synchronisation; the usual fn just puts the message on a channel.
// A call to fn that is already in progress may still be finishing when
// Divert returns.
func (r *Reader) Divert(fn func(tea.Msg) bool) {
	if r == nil {
		return
	}
	r.delivery.setDivert(fn)
}

// readLoop is the goroutine that reads the console until Stop is called.
func (r *Reader) readLoop() {
	defer close(r.loopDone)

	// Each turn of the loop reads whatever is waiting, up to this many
	// records, in one call. A paste longer than that simply takes several
	// turns; the translator joins the pieces (its "continuation" rule).
	// 2048 records are 40 KB, safely below the 64 KB that old versions of
	// Windows allowed for one console call.
	buf := make([]inputRecord, 2048)
	batch := make([]record, 0, len(buf))

	// WaitForMultipleObjects reports the first handle in this list that is
	// ready, so Stop wins over pending input.
	handles := []windows.Handle{r.stop, r.in}

	for {
		// Sleep until there is input, Stop is called, or the translator
		// wants to be woken (a paste or a held-back ESC is pending).
		timeout := uint32(windows.INFINITE)
		if when, ok := r.translate.deadline(); ok {
			timeout = millisecondsUntil(when)
		}
		// If input is already waiting, this returns "input" even when the
		// timeout has run out too. A late wake-up therefore can never cut a
		// paste in two: the timeout only wins when the queue is truly empty.
		event, err := windows.WaitForMultipleObjects(handles, false, timeout)

		var msgs []tea.Msg
		switch {
		case err != nil:
			r.trace.printf("wait failed: %v", err)
			return
		case event == windows.WAIT_OBJECT_0: // r.stop
			return
		case event == windows.WAIT_OBJECT_0+1: // r.in has records
			// The handle is signalled, so at least one record is waiting
			// and this read returns immediately.
			var n uint32
			if err := readConsoleInput(r.in, buf, &n); err != nil {
				r.trace.printf("read failed: %v", err)
				return
			}
			batch = batch[:0]
			for _, raw := range buf[:n] {
				if rec, ok := raw.decode(); ok {
					batch = append(batch, rec)
				}
			}
			// The clock is read after the read, so the translator measures
			// "quiet" from the moment we last saw input.
			msgs = r.translate.feed(batch, time.Now())
			r.trace.batch(batch, msgs)
		case event == uint32(windows.WAIT_TIMEOUT):
			msgs = r.translate.expire(time.Now())
			r.trace.batch(nil, msgs)
		default:
			return
		}

		// push never blocks; see delivery for why that matters.
		r.delivery.push(msgs...)
	}
}

// millisecondsUntil converts a wake-up time into a timeout for
// WaitForMultipleObjects, rounding up so that we never wake too early.
func millisecondsUntil(when time.Time) uint32 {
	d := time.Until(when)
	if d <= 0 {
		return 0
	}
	return uint32((d + time.Millisecond - 1) / time.Millisecond)
}

// window reports the position and size of the visible console window, in
// character cells.
func (r *Reader) window() (left, top, width, height int, ok bool) {
	if r.out == 0 {
		return 0, 0, 0, 0, false
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(r.out, &info); err != nil {
		return 0, 0, 0, 0, false
	}
	w := info.Window
	return int(w.Left), int(w.Top), int(w.Right-w.Left) + 1, int(w.Bottom-w.Top) + 1, true
}

// openConsoleOutput opens the console's screen. "CONOUT$" is a special file
// name that always means "the console this process is attached to", even if
// stdout has been redirected to a file. It returns 0 if that fails.
func openConsoleOutput() windows.Handle {
	name, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return 0
	}
	h, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0
	}
	return h
}

// inputRecord has the memory layout of the Win32 INPUT_RECORD structure: a
// 16-bit event type, two bytes of padding, then 16 bytes whose meaning
// depends on the type (a C union).
type inputRecord struct {
	eventType uint16
	_         uint16
	event     [16]byte
}

// Event types of INPUT_RECORD.
const (
	keyEvent              = 0x0001
	mouseEvent            = 0x0002
	windowBufferSizeEvent = 0x0004
	focusEvent            = 0x0010
)

// decode converts a raw INPUT_RECORD into a record. ok is false for event
// types we have no use for (menu events).
func (raw inputRecord) decode() (rec record, ok bool) {
	// All fields are little-endian integers at fixed offsets; see the
	// KEY_EVENT_RECORD, MOUSE_EVENT_RECORD, … structures in the Windows
	// console documentation.
	e := raw.event[:]
	u16 := func(offset int) uint16 { return binary.LittleEndian.Uint16(e[offset:]) }
	u32 := func(offset int) uint32 { return binary.LittleEndian.Uint32(e[offset:]) }

	switch raw.eventType {
	case keyEvent:
		rec.kind = recKey
		rec.key = keyRecord{
			down:   u32(0) != 0,
			repeat: u16(4),
			vk:     u16(6),
			scan:   u16(8),
			char:   u16(10),
			mods:   u32(12),
		}
	case mouseEvent:
		rec.kind = recMouse
		rec.mouse = mouseRecord{
			x:       int(int16(u16(0))),
			y:       int(int16(u16(2))),
			buttons: u32(4),
			mods:    u32(8),
			flags:   u32(12),
		}
	case windowBufferSizeEvent:
		rec.kind = recResize
		rec.width = int(int16(u16(0)))
		rec.height = int(int16(u16(2)))
	case focusEvent:
		rec.kind = recFocus
		rec.focused = u32(0) != 0
	default:
		return rec, false
	}
	return rec, true
}

// golang.org/x/sys/windows does not wrap ReadConsoleInputW, so we call it
// from kernel32.dll ourselves.
var procReadConsoleInputW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

// readConsoleInput reads up to len(buf) records, storing how many it got in
// *n. It blocks while the queue is empty.
func readConsoleInput(console windows.Handle, buf []inputRecord, n *uint32) error {
	ok, _, err := procReadConsoleInputW.Call(
		uintptr(console),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(n)),
	)
	if ok == 0 {
		return fmt.Errorf("ReadConsoleInput: %w", err)
	}
	return nil
}

// tracer writes the WA_CONIN_TRACE log. All its methods do nothing on a nil
// *tracer, so the read loop can call them unconditionally.
type tracer struct {
	file  *os.File
	out   *bufio.Writer // a large paste is tens of thousands of lines
	start time.Time
}

// openTrace returns a tracer if WA_CONIN_TRACE names a file that can be
// opened, and nil otherwise.
func openTrace() *tracer {
	path := os.Getenv("WA_CONIN_TRACE")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	tr := &tracer{file: f, out: bufio.NewWriter(f), start: time.Now()}
	tr.printf("trace started")
	return tr
}

// printf writes one line, prefixed with the time since the trace started.
func (tr *tracer) printf(format string, args ...any) {
	if tr == nil {
		return
	}
	ms := float64(time.Since(tr.start).Microseconds()) / 1000
	fmt.Fprintf(tr.out, "%10.3fms  %s\n", ms, fmt.Sprintf(format, args...))
	_ = tr.out.Flush()
}

// batch logs one turn of the read loop: the records read (nil for a
// timeout) and the messages they produced.
func (tr *tracer) batch(batch []record, msgs []tea.Msg) {
	if tr == nil {
		return
	}
	if batch == nil {
		tr.printf("timeout")
	} else {
		tr.printf("read %d records", len(batch))
	}
	for _, rec := range batch {
		fmt.Fprintf(tr.out, "                  %s\n", rec)
	}
	for _, msg := range msgs {
		fmt.Fprintf(tr.out, "              ->  %s\n", describe(msg))
	}
	_ = tr.out.Flush()
}

func (tr *tracer) close() {
	if tr != nil {
		_ = tr.out.Flush()
		_ = tr.file.Close()
	}
}
