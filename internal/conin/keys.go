package conin

import (
	"unicode/utf16"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// The key and mouse tables in this file are adapted from Bubble Tea v1's
// Windows reader (keyType and mouseEvent in key_windows.go),
// Copyright (c) 2020-2025 Charmbracelet, Inc, MIT License:
// https://github.com/charmbracelet/bubbletea

// key is one key press after decoding: the message it would send as a
// normal key, plus what the paste detector needs to know about it.
type key struct {
	msg    tea.KeyMsg // the message for a normal (typed) press
	repeat int        // how many times to send it (a held key repeats)

	// char is the character the key carries (0 if none). The paste detector
	// looks at it to spot the ESC [ 2 0 0 ~ paste markers.
	char rune

	// text is what this key contributes if it turns out to be part of a
	// paste: the character itself, '\r' for Enter, '\n' for a line feed,
	// '\t' for Tab. It is 0 for keys that are never pasted text (arrows,
	// Backspace, Esc, Ctrl+C, …).
	text rune

	// chord is true for a Ctrl or Alt combination (Ctrl+C, Alt+x,
	// Ctrl+Left, …). A paste never contains one, so a batch with a chord in
	// it is treated as typing.
	chord bool
}

// decodeKey turns one key record into a key. ok is false for records that
// mean nothing on their own: key releases, a bare Shift/Ctrl/Alt press, and
// the first half of a surrogate pair (which is remembered until its second
// half arrives).
func (t *translator) decodeKey(r keyRecord) (k key, ok bool) {
	c := rune(r.char)
	vk, mods := r.vk, r.mods

	if !r.down {
		// Releases are ignored, with one exception. A character that is not
		// on the keyboard layout is entered — by a person with Alt+numpad
		// digits, or by the console itself when such a character is pasted —
		// as: Alt down, digits, Alt UP. The character rides on that final
		// release of the Alt key.
		if vk != vkMenu || c == 0 {
			return key{}, false
		}
		vk, mods = 0, 0 // from here on it is a plain character, not Alt
	}

	// Windows speaks UTF-16: a character outside the Basic Multilingual
	// Plane (most emoji) arrives as two records, one per surrogate half.
	// Join them into the single rune they stand for.
	switch {
	case utf16.IsSurrogate(c) && c < 0xDC00: // first (high) half
		t.highSurrogate = c
		return key{}, false
	case utf16.IsSurrogate(c): // second (low) half
		c = utf16.DecodeRune(t.highSurrogate, c)
		t.highSurrogate = 0
		if c == utf8.RuneError {
			return key{}, false // a second half without its first: drop it
		}
	case c != 0:
		t.highSurrogate = 0 // a first half that was never completed
	}

	ctrl := mods&anyCtrl != 0
	alt := mods&anyAlt != 0
	shift := mods&shiftPressed != 0

	// AltGr (the right Alt key on many non-US keyboards, used to type
	// characters such as @ or €) is reported by Windows as Ctrl+Alt. When
	// Ctrl+Alt produced a printable character, the modifiers were "used up"
	// making that character: report the character, not a chord.
	if ctrl && alt && isPrintable(c) {
		ctrl, alt = false, false
	}

	k = key{char: c, repeat: int(r.repeat)}
	if k.repeat < 1 {
		k.repeat = 1
	}

	if vk == vkReturn {
		switch {
		case alt:
			// Same as Unix terminals, which send ESC followed by CR.
			k.msg = tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
		case ctrl || shift:
			// Shift+Enter and Ctrl+Enter mean "new line" in the message
			// box. Bubble Tea v1 has no key type for them, so we borrow
			// Ctrl+J (the line-feed key), which the TUI treats the same.
			k.msg = tea.KeyMsg{Type: tea.KeyCtrlJ}
			if c == '\n' {
				// Ctrl+Enter carries a line feed. That is also exactly how
				// the console delivers a pasted '\n', so it counts as
				// paste text.
				k.text = '\n'
			}
		default:
			k.msg = tea.KeyMsg{Type: tea.KeyEnter}
			k.text = '\r'
		}
	} else {
		typ := keyType(vk, c, shift, ctrl)
		switch {
		case typ != tea.KeyRunes:
			k.msg = tea.KeyMsg{Type: typ, Alt: alt}
			if !ctrl && !alt && typ == tea.KeyTab {
				k.text = '\t'
			}
			if !alt && typ == tea.KeyCtrlJ {
				k.text = '\n' // Ctrl+J is a line feed too
			}
		case c == 0:
			// A key that produces no character and has no name: a bare
			// Shift/Ctrl/Alt press, Caps Lock, the Windows key, …
			return key{}, false
		default:
			k.msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{c}, Alt: alt}
			if !ctrl && !alt {
				k.text = c
			}
		}
	}

	k.chord = (ctrl || alt) && k.text == 0
	return k, true
}

// isPrintable reports whether c is a character you can see or a space, as
// opposed to "no character" (0) or a control code.
func isPrintable(c rune) bool {
	return c >= 0x20 && c != 0x7f
}

// keyType maps a key to Bubble Tea's key type. Keys that simply produce a
// character are tea.KeyRunes. Enter is handled by the caller.
func keyType(vk uint16, c rune, shift, ctrl bool) tea.KeyType {
	// pick chooses among the four variants Bubble Tea has for the keys that
	// move the cursor: plain, Shift+, Ctrl+ and Ctrl+Shift+.
	pick := func(plain, withShift, withCtrl, withBoth tea.KeyType) tea.KeyType {
		switch {
		case shift && ctrl:
			return withBoth
		case shift:
			return withShift
		case ctrl:
			return withCtrl
		}
		return plain
	}

	switch vk {
	case vkBack:
		return tea.KeyBackspace
	case vkTab:
		if shift {
			return tea.KeyShiftTab
		}
		return tea.KeyTab
	case vkEscape:
		return tea.KeyEscape
	case vkUp:
		return pick(tea.KeyUp, tea.KeyShiftUp, tea.KeyCtrlUp, tea.KeyCtrlShiftUp)
	case vkDown:
		return pick(tea.KeyDown, tea.KeyShiftDown, tea.KeyCtrlDown, tea.KeyCtrlShiftDown)
	case vkRight:
		return pick(tea.KeyRight, tea.KeyShiftRight, tea.KeyCtrlRight, tea.KeyCtrlShiftRight)
	case vkLeft:
		return pick(tea.KeyLeft, tea.KeyShiftLeft, tea.KeyCtrlLeft, tea.KeyCtrlShiftLeft)
	case vkHome:
		return pick(tea.KeyHome, tea.KeyShiftHome, tea.KeyCtrlHome, tea.KeyCtrlShiftHome)
	case vkEnd:
		return pick(tea.KeyEnd, tea.KeyShiftEnd, tea.KeyCtrlEnd, tea.KeyCtrlShiftEnd)
	case vkPrior:
		if ctrl {
			return tea.KeyCtrlPgUp
		}
		return tea.KeyPgUp
	case vkNext:
		if ctrl {
			return tea.KeyCtrlPgDown
		}
		return tea.KeyPgDown
	case vkDelete:
		return tea.KeyDelete
	case vkInsert:
		return tea.KeyInsert
	}
	if vk >= vkF1 && vk <= vkF20 {
		// Bubble Tea counts its key types downwards (KeyF2 == KeyF1 - 1).
		return tea.KeyF1 - tea.KeyType(vk-vkF1)
	}

	// Not a named key, so it is a character — possibly a control character.
	switch {
	case c >= 0x01 && c <= 0x1f:
		// Ctrl+A..Ctrl+Z produce the control codes 1..26, and Bubble Tea
		// gave its key types those same numbers (tea.KeyCtrlC == 3), so the
		// conversion is direct. It also covers Ctrl+[ \ ] ^ _ (27..31).
		return tea.KeyType(c)
	case c == 0x7f:
		return tea.KeyBackspace
	case ctrl && c == '@':
		return tea.KeyCtrlAt
	case ctrl && vk == vkOem4:
		return tea.KeyCtrlOpenBracket
	case ctrl && vk == vkOem6:
		return tea.KeyCtrlCloseBracket
	case ctrl && c == 0 && vk >= 'A' && vk <= 'Z':
		// Ctrl+Alt+letter produces no character; the virtual-key code still
		// says which letter it was.
		return tea.KeyCtrlA + tea.KeyType(vk-'A')
	}
	return tea.KeyRunes
}

// mouseMsg turns a mouse record into a Bubble Tea mouse message. ok is false
// for events not worth reporting: the pointer merely moving with no button
// held (there would be hundreds of them), or nothing recognisable.
func (t *translator) mouseMsg(m mouseRecord) (msg tea.MouseMsg, ok bool) {
	// The console reports positions inside its whole screen buffer, which
	// can be taller than the window (scrollback). Bubble Tea wants positions
	// inside the visible window.
	x, y := m.x, m.y
	if t.window != nil {
		if left, top, _, _, ok := t.window(); ok {
			x, y = x-left, y-top
		}
	}

	ev := tea.MouseMsg{
		X:     x,
		Y:     y,
		Alt:   m.mods&anyAlt != 0,
		Ctrl:  m.mods&anyCtrl != 0,
		Shift: m.mods&shiftPressed != 0,
	}

	// For wheel events the high half of "buttons" is the scroll distance: a
	// signed number, positive when the wheel turns away from the user.
	wheel := int16(m.buttons >> 16)
	held := m.buttons & buttonMask

	switch {
	case m.flags&mouseWheeled != 0:
		ev.Button, ev.Type = tea.MouseButtonWheelDown, tea.MouseWheelDown
		if wheel > 0 {
			ev.Button, ev.Type = tea.MouseButtonWheelUp, tea.MouseWheelUp
		}

	case m.flags&mouseHWheeled != 0:
		ev.Button, ev.Type = tea.MouseButtonWheelLeft, tea.MouseWheelLeft
		if wheel > 0 {
			ev.Button, ev.Type = tea.MouseButtonWheelRight, tea.MouseWheelRight
		}

	case m.flags&mouseMoved != 0:
		t.buttons = held
		if held == 0 {
			return ev, false // plain pointer movement
		}
		ev.Button = firstButton(held) // a drag
		ev.Action = tea.MouseActionMotion
		ev.Type = tea.MouseMotion

	default:
		// A click: one button changed. Comparing with the buttons that were
		// held before tells us which one, and whether it went down or up.
		changed := t.buttons ^ held
		t.buttons = held
		if changed == 0 {
			changed = held // no change seen (e.g. a double click): report what is down
		}
		ev.Button = firstButton(changed)
		if ev.Button == tea.MouseButtonNone {
			return ev, false
		}
		if changed&held == 0 {
			ev.Action = tea.MouseActionRelease
			ev.Type = tea.MouseRelease
		} else {
			ev.Action = tea.MouseActionPress
			ev.Type = buttonType(ev.Button)
		}
	}
	return ev, true
}

// firstButton names the lowest-numbered button set in bits.
func firstButton(bits uint32) tea.MouseButton {
	switch {
	case bits&buttonLeft != 0:
		return tea.MouseButtonLeft
	case bits&buttonMiddle != 0:
		return tea.MouseButtonMiddle
	case bits&buttonRight != 0:
		return tea.MouseButtonRight
	case bits&buttonBackward != 0:
		return tea.MouseButtonBackward
	case bits&buttonForward != 0:
		return tea.MouseButtonForward
	}
	return tea.MouseButtonNone
}

// buttonType fills Bubble Tea's older, deprecated MouseMsg.Type field for a
// button press, for code that still switches on it.
func buttonType(b tea.MouseButton) tea.MouseEventType {
	switch b {
	case tea.MouseButtonLeft:
		return tea.MouseLeft
	case tea.MouseButtonMiddle:
		return tea.MouseMiddle
	case tea.MouseButtonRight:
		return tea.MouseRight
	case tea.MouseButtonBackward:
		return tea.MouseBackward
	case tea.MouseButtonForward:
		return tea.MouseForward
	}
	return tea.MouseUnknown
}
