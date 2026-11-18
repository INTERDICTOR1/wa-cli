package conin

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// How long the paste detector waits. These are the only timing numbers in
// the package.
const (
	// pasteQuiet: a paste is over when no more text has arrived for this
	// long. Big pastes reach us in several pieces a fraction of a
	// millisecond apart, so this only has to be longer than those gaps and
	// short enough that nobody notices the wait.
	pasteQuiet = 30 * time.Millisecond

	// markerWait: how long a trailing ESC is held back to see whether it is
	// the first character of a paste marker. This is the only delay a typed
	// key can ever get, and only the Esc key gets it.
	markerWait = 10 * time.Millisecond

	// bracketQuiet: a safety net. After a start marker everything is paste
	// text until the end marker; should that end marker never arrive, give
	// up after this much silence instead of swallowing typing forever.
	bracketQuiet = 500 * time.Millisecond
)

// esc is the escape character, which begins both paste markers.
const esc = 0x1b

// translator is the paste detector. It turns batches of console records into
// Bubble Tea messages.
//
// A "batch" is everything that was waiting in the console's input queue when
// the read loop looked. That is the key observation: the console hands a
// paste over as a run of ordinary key presses, all at once, while a person
// typing produces one key press per batch — even a very fast typist is
// thousands of times slower than the read loop.
//
// The rules, in order:
//
//  1. Bracketed paste. Some terminals wrap a paste in the marker sequences
//     ESC[200~ and ESC[201~. They reach us as key presses for those
//     characters. Everything between the markers is paste text, and the
//     markers themselves are dropped.
//  2. Burst. Two or more text keys (characters, Enter, Tab) in one batch,
//     with no Ctrl/Alt combination among them, are a paste.
//  3. Continuation. While a paste is being collected, any text that arrives
//     within pasteQuiet is added to it — even a single Enter, which is what
//     the seam between two pieces of a big paste can look like. The paste is
//     sent as one message when the input goes quiet or a non-text key
//     arrives (then the paste goes first, the key after it).
//  4. Otherwise a batch with one key press is a typed key and is sent
//     immediately.
//
// translator makes no system calls and never looks at the real clock: the
// caller passes the time in, and asks deadline when to come back. That is
// what lets the tests replay any sequence of events exactly.
//
// It is not safe for use from several goroutines; the read loop owns it.
type translator struct {
	mouse bool // report mouse events (Options.Mouse)

	// window, if set, returns the position and size of the visible console
	// window. ok is false if it could not be found out.
	window func() (left, top, width, height int, ok bool)

	highSurrogate rune // first half of a UTF-16 pair waiting for its second half

	paste     []rune // paste text collected so far (line breaks not yet tidied)
	pasting   bool   // collecting a burst paste (rules 2 and 3)
	bracketed bool   // between a start marker and its end marker (rule 1)

	// held are keys read but not yet acted on, because they might be the
	// beginning of a paste marker: an ESC, or ESC [ 2 …, at the very end of
	// a batch.
	held []key

	// wake is when expire should be called if no input arrives first. The
	// zero time means there is nothing to wait for.
	wake time.Time

	lastWidth, lastHeight int    // last size reported, to skip duplicates
	buttons               uint32 // mouse buttons held at the last mouse event
}

// feed translates one batch of records, read at time now, into the messages
// to send right away. Text belonging to an unfinished paste stays inside the
// translator until a later feed or expire releases it.
func (t *translator) feed(batch []record, now time.Time) []tea.Msg {
	var out []tea.Msg

	// Keys that were held back join the front of this batch.
	keys := t.held
	t.held = nil

	for _, rec := range batch {
		switch rec.kind {
		case recKey:
			if k, ok := t.decodeKey(rec.key); ok {
				keys = append(keys, k)
			}

		// Mouse, focus and resize events have nothing to do with pastes.
		// They are reported at once and never interrupt a paste that is
		// being collected.
		case recMouse:
			if !t.mouse {
				continue
			}
			if msg, ok := t.mouseMsg(rec.mouse); ok {
				out = append(out, msg)
			}
		case recFocus:
			if rec.focused {
				out = append(out, tea.FocusMsg{})
			} else {
				out = append(out, tea.BlurMsg{})
			}
		case recResize:
			if msg, ok := t.resizeMsg(rec); ok {
				out = append(out, msg)
			}
		}
	}

	out = t.process(out, keys, false)
	t.arm(now)
	return out
}

// expire is called when the time returned by deadline has passed without any
// new input. It releases whatever was being held back.
func (t *translator) expire(now time.Time) []tea.Msg {
	var out []tea.Msg
	if len(t.held) > 0 {
		// The ESC (or partial marker) was not followed by the rest of a
		// marker in time: these were ordinary keys after all.
		keys := t.held
		t.held = nil
		out = t.process(out, keys, true)
	} else {
		// The input went quiet: the paste is complete.
		out = t.flush(out)
		t.bracketed = false
	}
	t.arm(now)
	return out
}

// deadline reports when expire should be called if no input arrives before
// then. ok is false when the translator is not waiting for anything, which
// is the normal state between key presses.
func (t *translator) deadline() (when time.Time, ok bool) {
	return t.wake, !t.wake.IsZero()
}

// arm sets the wake-up time according to what the translator is waiting for.
func (t *translator) arm(now time.Time) {
	switch {
	case len(t.held) > 0:
		t.wake = now.Add(markerWait)
	case t.bracketed:
		t.wake = now.Add(bracketQuiet)
	case t.pasting:
		t.wake = now.Add(pasteQuiet)
	default:
		t.wake = time.Time{}
	}
}

// process applies the paste rules to the keys of one batch and appends the
// resulting messages to out. final is true when no more input is coming for
// these keys (expire), so a trailing ESC must not be held back again.
func (t *translator) process(out []tea.Msg, keys []key, final bool) []tea.Msg {
	// Rule 2: is this batch a burst? If a paste is already being collected
	// (rule 3) the question is settled.
	texts, chord := 0, false
	for _, k := range keys {
		if k.text != 0 {
			texts++
		}
		if k.chord {
			chord = true
		}
	}
	burst := t.pasting || (texts >= 2 && !chord)

	for i := 0; i < len(keys); i++ {
		k := keys[i]

		// Rule 1: paste markers.
		if k.char == esc {
			switch matchMarker(keys[i:]) {
			case markerStart:
				out = t.flush(out)
				t.bracketed = true
				i += markerLen - 1 // skip the marker's remaining keys
				continue
			case markerEnd:
				// An end marker with no start before it is simply dropped.
				if t.bracketed {
					out = t.flush(out)
					t.bracketed = false
				}
				i += markerLen - 1
				continue
			case markerPartial:
				if !final {
					// The batch ended in the middle of what could be a
					// marker. Keep these keys and decide once more input
					// arrives, or markerWait runs out.
					t.held = append(t.held, keys[i:]...)
					return out
				}
			}
		}

		switch {
		case t.bracketed:
			// Inside the markers everything is text. Keys that are not
			// text (a stray control character in the clipboard) are
			// dropped: pasted data must never act as a command.
			if k.text != 0 {
				t.addText(k)
			}
		case k.text != 0 && burst:
			t.addText(k)
			t.pasting = true
		case k.text != 0:
			out = appendKey(out, k) // rule 4: a typed character
		default:
			// A non-text key ends any paste: the paste goes first.
			out = t.flush(out)
			out = appendKey(out, k)
		}
	}
	return out
}

// addText adds a key's text to the paste being collected.
func (t *translator) addText(k key) {
	for i := 0; i < k.repeat; i++ {
		t.paste = append(t.paste, k.text)
	}
}

// flush sends the collected paste, if any, as one message.
func (t *translator) flush(out []tea.Msg) []tea.Msg {
	if len(t.paste) > 0 {
		out = append(out, tea.KeyMsg{
			Type:  tea.KeyRunes,
			Runes: normalizeNewlines(t.paste),
			Paste: true,
		})
	}
	t.paste = nil
	t.pasting = false
	return out
}

// appendKey adds the message for a normal key press, once per repeat.
func appendKey(out []tea.Msg, k key) []tea.Msg {
	for i := 0; i < k.repeat; i++ {
		out = append(out, k.msg)
	}
	return out
}

// normalizeNewlines rewrites every kind of line break (CR, LF, or the
// Windows pair CR LF) as a single '\n'.
func normalizeNewlines(in []rune) []rune {
	out := make([]rune, 0, len(in))
	afterCR := false
	for _, r := range in {
		switch {
		case r == '\r':
			out = append(out, '\n')
		case r == '\n' && afterCR:
			// The LF of a CR LF pair: its line break was already written.
		default:
			out = append(out, r)
		}
		afterCR = r == '\r'
	}
	return out
}

// markerKind is the result of looking for a paste marker.
type markerKind int

const (
	markerNone    markerKind = iota // these keys are not a marker
	markerStart                     // ESC [ 2 0 0 ~ : a paste begins
	markerEnd                       // ESC [ 2 0 1 ~ : the paste ends
	markerPartial                   // a marker so far, but the keys ran out
)

// markerLen is the number of characters in a paste marker.
const markerLen = 6

// matchMarker checks whether keys begins with a bracketed-paste marker.
func matchMarker(keys []key) markerKind {
	const common = "\x1b[20" // both markers start like this
	for i := 0; i < markerLen; i++ {
		if i == len(keys) {
			return markerPartial
		}
		c := keys[i].char
		var match bool
		switch {
		case i < len(common):
			match = c == rune(common[i])
		case i == len(common):
			match = c == '0' || c == '1'
		default:
			match = c == '~'
		}
		if !match {
			return markerNone
		}
	}
	if keys[len(common)].char == '0' {
		return markerStart
	}
	return markerEnd
}

// resizeMsg builds the window-size message for a resize record. ok is false
// when the size is the same as last time.
func (t *translator) resizeMsg(rec record) (msg tea.WindowSizeMsg, ok bool) {
	// The record carries the size of the screen buffer, which in a classic
	// console window can be much taller than what is visible. Ask for the
	// window itself when we can.
	width, height := rec.width, rec.height
	if t.window != nil {
		if _, _, w, h, ok := t.window(); ok {
			width, height = w, h
		}
	}
	if width == t.lastWidth && height == t.lastHeight {
		return msg, false
	}
	t.lastWidth, t.lastHeight = width, height
	return tea.WindowSizeMsg{Width: width, Height: height}, true
}
