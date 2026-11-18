package conin

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// This file describes console input the way Windows reports it, but as plain
// Go values. conin_windows.go fills these in from the raw INPUT_RECORD
// structures; the tests build them by hand.

// recordKind says which kind of console event a record holds.
type recordKind uint8

const (
	recKey    recordKind = iota + 1 // a key went down or up
	recMouse                        // the mouse moved, clicked or scrolled
	recResize                       // the console's screen buffer changed size
	recFocus                        // the console window gained or lost focus
)

// record is one console input event. Only the fields that belong to its kind
// are meaningful.
type record struct {
	kind  recordKind
	key   keyRecord
	mouse mouseRecord

	// For recResize: the new screen-buffer size as Windows reported it. The
	// translator prefers to ask for the visible window size instead (the
	// buffer can be larger than the window), and only falls back to these.
	width, height int

	// For recFocus: true when the window gained focus.
	focused bool
}

// keyRecord mirrors the Win32 KEY_EVENT_RECORD.
type keyRecord struct {
	down   bool   // true = pressed, false = released
	repeat uint16 // how many presses this record stands for (a held key)
	vk     uint16 // virtual-key code: which key, independent of the layout
	scan   uint16 // hardware scan code (only used for tracing)
	char   uint16 // the character produced, as one UTF-16 code unit; 0 if none
	mods   uint32 // which modifier keys were held (the *Pressed bits below)
}

// mouseRecord mirrors the Win32 MOUSE_EVENT_RECORD.
type mouseRecord struct {
	x, y    int    // cell position, in screen-buffer coordinates
	buttons uint32 // low bits: buttons held; high 16 bits: wheel movement
	mods    uint32 // modifier keys, same bits as keyRecord.mods
	flags   uint32 // what happened (the mouse* flags below); 0 = a click
}

// Modifier bits of dwControlKeyState.
const (
	rightAltPressed  = 0x0001
	leftAltPressed   = 0x0002
	rightCtrlPressed = 0x0004
	leftCtrlPressed  = 0x0008
	shiftPressed     = 0x0010

	anyAlt  = leftAltPressed | rightAltPressed
	anyCtrl = leftCtrlPressed | rightCtrlPressed
)

// Virtual-key codes we give a meaning to. Values are from WinUser.h.
const (
	vkBack   = 0x08
	vkTab    = 0x09
	vkReturn = 0x0D
	vkMenu   = 0x12 // the Alt key ("menu" is its historical Windows name)
	vkEscape = 0x1B
	vkSpace  = 0x20
	vkPrior  = 0x21 // Page Up
	vkNext   = 0x22 // Page Down
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkInsert = 0x2D
	vkDelete = 0x2E
	vkF1     = 0x70 // F1..F20 are consecutive: vkF1+1 is F2, and so on
	vkF20    = 0x83
	vkOem4   = 0xDB // the "[{" key on a US keyboard
	vkOem6   = 0xDD // the "]}" key on a US keyboard
)

// Mouse button bits (low word of dwButtonState).
const (
	buttonLeft     = 0x0001 // FROM_LEFT_1ST_BUTTON_PRESSED
	buttonRight    = 0x0002 // RIGHTMOST_BUTTON_PRESSED
	buttonMiddle   = 0x0004 // FROM_LEFT_2ND_BUTTON_PRESSED
	buttonBackward = 0x0008 // FROM_LEFT_3RD_BUTTON_PRESSED
	buttonForward  = 0x0010 // FROM_LEFT_4TH_BUTTON_PRESSED
	buttonMask     = 0x001F
)

// Mouse event flags (dwEventFlags).
const (
	mouseMoved    = 0x0001
	mouseWheeled  = 0x0004 // vertical wheel
	mouseHWheeled = 0x0008 // horizontal wheel
)

// String renders a record compactly for the trace log and for test failures,
// for example `down vk=0x0D ch='\r' mods=0x10`.
func (r record) String() string {
	switch r.kind {
	case recKey:
		k := r.key
		var b strings.Builder
		if k.down {
			b.WriteString("down")
		} else {
			b.WriteString("up")
		}
		fmt.Fprintf(&b, " vk=0x%02X sc=0x%02X ch=%q", k.vk, k.scan, rune(k.char))
		if k.mods != 0 {
			fmt.Fprintf(&b, " mods=0x%X", k.mods)
		}
		if k.repeat != 1 {
			fmt.Fprintf(&b, " x%d", k.repeat)
		}
		return b.String()
	case recMouse:
		m := r.mouse
		return fmt.Sprintf("mouse x=%d y=%d buttons=0x%X flags=0x%X mods=0x%X", m.x, m.y, m.buttons, m.flags, m.mods)
	case recResize:
		return fmt.Sprintf("resize %dx%d", r.width, r.height)
	case recFocus:
		return fmt.Sprintf("focus %v", r.focused)
	}
	return "unknown"
}

// describe renders a message compactly for the trace log and for test
// failures, for example `key "enter"` or `paste "two\nlines"`.
func describe(msg tea.Msg) string {
	switch m := msg.(type) {
	case tea.KeyMsg:
		var b strings.Builder
		switch {
		case m.Paste:
			fmt.Fprintf(&b, "paste %q", string(m.Runes))
		case m.Type == tea.KeyRunes:
			fmt.Fprintf(&b, "key runes %q", string(m.Runes))
		default:
			fmt.Fprintf(&b, "key %q", m.Type.String())
		}
		if m.Alt {
			b.WriteString(" alt")
		}
		return b.String()
	case tea.MouseMsg:
		return fmt.Sprintf("mouse %q at %d,%d", m.String(), m.X, m.Y)
	case tea.WindowSizeMsg:
		return fmt.Sprintf("size %dx%d", m.Width, m.Height)
	case tea.FocusMsg:
		return "focus"
	case tea.BlurMsg:
		return "blur"
	}
	return fmt.Sprintf("%T", msg)
}
