package conin

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	tea "github.com/charmbracelet/bubbletea"
)

// These tests drive the paste detector with hand-made console records and a
// made-up clock. The record shapes used here are the ones the console really
// produces; they were copied from traces of the end-to-end tests (see
// e2e_windows_test.go), where each shape is noted.

// --- building records -------------------------------------------------------

func down(vk uint16, ch rune, mods uint32) record {
	return record{kind: recKey, key: keyRecord{down: true, repeat: 1, vk: vk, char: uint16(ch), mods: mods}}
}

func up(vk uint16, ch rune, mods uint32) record {
	return record{kind: recKey, key: keyRecord{down: false, repeat: 1, vk: vk, char: uint16(ch), mods: mods}}
}

// repeated is a key-down record that stands for n presses (a held key).
func repeated(vk uint16, ch rune, n int) record {
	r := down(vk, ch, 0)
	r.key.repeat = uint16(n)
	return r
}

// pasted returns the records the console generates when the terminal sends
// text s: for every character a key-down followed at once by its key-up.
// Line breaks: '\r' becomes the Enter key, and '\n' becomes Ctrl+Enter
// carrying a line feed, wrapped in a press and release of Ctrl. Capital
// letters are wrapped in Shift. Characters outside the BMP become two
// records, one per UTF-16 half.
func pasted(s string) []record {
	const (
		vkShift   = 0x10
		vkControl = 0x11
	)
	var recs []record
	for _, r := range s {
		switch {
		case r == '\r':
			recs = append(recs, down(vkReturn, '\r', 0), up(vkReturn, '\r', 0))
		case r == '\n':
			recs = append(recs,
				down(vkControl, 0, leftCtrlPressed),
				down(vkReturn, '\n', leftCtrlPressed), up(vkReturn, '\n', leftCtrlPressed),
				up(vkControl, 0, 0))
		case r == '\t':
			recs = append(recs, down(vkTab, '\t', 0), up(vkTab, '\t', 0))
		case r == esc:
			recs = append(recs, down(vkEscape, esc, 0), up(vkEscape, esc, 0))
		case r >= 'A' && r <= 'Z':
			recs = append(recs,
				down(vkShift, 0, shiftPressed),
				down(uint16(r), r, shiftPressed), up(uint16(r), r, shiftPressed),
				up(vkShift, 0, 0))
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			recs = append(recs, down(0, hi, 0), up(0, hi, 0), down(0, lo, 0), up(0, lo, 0))
		default:
			recs = append(recs, down(0, r, 0), up(0, r, 0))
		}
	}
	return recs
}

// --- replaying a timeline ---------------------------------------------------

// step is one batch of records arriving at a given time after the start.
type step struct {
	at    time.Duration
	batch []record
}

// at is shorthand for a step.
func at(ms int, batch ...record) step {
	return step{at: time.Duration(ms) * time.Millisecond, batch: batch}
}

// play feeds the steps to tr the way the read loop does — a timeout fires
// whenever the translator's deadline passes before the next input — and
// returns every message with the time it was produced, as "<ms> <message>".
func play(tr *translator, steps ...step) []string {
	start := time.Unix(1_000_000, 0)
	var got []string
	emit := func(when time.Time, msgs []tea.Msg) {
		for _, msg := range msgs {
			got = append(got, fmt.Sprintf("%d %s", when.Sub(start).Milliseconds(), describe(msg)))
		}
	}
	// fireTimeouts runs the timeouts due before limit (all of them if limit
	// is the zero time).
	fireTimeouts := func(limit time.Time) {
		for {
			when, ok := tr.deadline()
			if !ok || (!limit.IsZero() && !when.Before(limit)) {
				return
			}
			emit(when, tr.expire(when))
		}
	}
	for _, s := range steps {
		now := start.Add(s.at)
		fireTimeouts(now)
		emit(now, tr.feed(s.batch, now))
	}
	fireTimeouts(time.Time{})
	return got
}

func check(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("messages differ\n got: %s\nwant: %s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
}

// --- the paste rules ---------------------------------------------------------

// Case a: several lines arriving in one batch are one paste, and no Enter
// escapes. The paste is released pasteQuiet (30 ms) after the last input.
func TestBurstIsOnePaste(t *testing.T) {
	got := play(&translator{}, at(0, pasted("hello\rworld\rthird line")...))
	check(t, got, `30 paste "hello\nworld\nthird line"`)
}

// Case b: a lone Enter is a typed key and is not delayed at all.
func TestLoneEnterIsImmediate(t *testing.T) {
	got := play(&translator{}, at(0, pasted("\r")...))
	check(t, got, `0 key "enter"`)
}

// Case c: one key per batch is typing, however fast.
func TestTypingIsNotAPaste(t *testing.T) {
	got := play(&translator{},
		at(0, pasted("h")...),
		at(5, pasted("i")...),
		at(10, pasted("!")...),
		at(15, pasted("\r")...),
	)
	check(t, got, `0 key runes "h"`, `5 key runes "i"`, `10 key runes "!"`, `15 key "enter"`)
}

// Fast typists overlap keys: the next key goes down before the previous one
// comes up, so a batch can hold a down and an unrelated up. Still typing.
func TestOverlappingKeysAreTyping(t *testing.T) {
	got := play(&translator{},
		at(0, down('H', 'h', 0)),
		at(5, down('I', 'i', 0), up('H', 'h', 0)),
		at(10, up('I', 'i', 0)),
		at(15, down(vkReturn, '\r', 0)),
		at(20, up(vkReturn, '\r', 0)),
	)
	check(t, got, `0 key runes "h"`, `5 key runes "i"`, `15 key "enter"`)
}

// Rule 3: a big paste arrives in pieces. A piece can be a single Enter; it
// still belongs to the paste as long as it comes within pasteQuiet.
func TestContinuationJoinsPieces(t *testing.T) {
	got := play(&translator{},
		at(0, pasted("first")...),
		at(10, pasted("\r")...), // a lone Enter: the seam between two pieces
		at(25, pasted("second")...),
		at(40, pasted("\r")...),
	)
	check(t, got, `70 paste "first\nsecond\n"`)
}

// After the quiet period the paste is over; an Enter that comes later is a
// typed Enter again.
func TestEnterAfterPasteIsTyped(t *testing.T) {
	got := play(&translator{},
		at(0, pasted("some text")...),
		at(100, pasted("\r")...),
	)
	check(t, got, `30 paste "some text"`, `100 key "enter"`)
}

// A non-text key ends the paste: the paste is sent first, then the key.
func TestNonTextKeyFlushesPaste(t *testing.T) {
	got := play(&translator{},
		at(0, pasted("ab")...),
		at(5, down(vkUp, 0, 0)),
	)
	check(t, got, `5 paste "ab"`, `5 key "up"`)
}

// A Ctrl or Alt combination in the batch means a person, not a paste.
func TestChordInBatchMeansTyping(t *testing.T) {
	got := play(&translator{},
		at(0, down('A', 'a', 0), down('C', 0x03, leftCtrlPressed), down('B', 'b', 0)),
	)
	check(t, got, `0 key runes "a"`, `0 key "ctrl+c"`, `0 key runes "b"`)
}

// A held key reaches us as ONE record with a repeat count (measured: the
// console merges repeats of the same key). That is typing, not a paste —
// holding Enter must not turn into pasted line breaks.
func TestRepeatCountIsNotAPaste(t *testing.T) {
	got := play(&translator{},
		at(0, repeated('A', 'a', 3)),
		at(100, repeated(vkReturn, '\r', 2)),
	)
	check(t, got, `0 key runes "a"`, `0 key runes "a"`, `0 key runes "a"`, `100 key "enter"`, `100 key "enter"`)
}

// Line breaks in every flavour become '\n'. A pasted '\n' reaches us as
// Ctrl+Enter carrying a line feed (measured on both console hosts).
func TestLineBreaksAreNormalised(t *testing.T) {
	got := play(&translator{}, at(0, pasted("a\rb\nc\r\nd\te")...))
	check(t, got, `30 paste "a\nb\nc\nd\te"`)
}

func TestNormalizeNewlines(t *testing.T) {
	for in, want := range map[string]string{
		"":             "",
		"plain":        "plain",
		"a\rb":         "a\nb",
		"a\nb":         "a\nb",
		"a\r\nb":       "a\nb",
		"a\r\rb":       "a\n\nb",
		"a\r\n\r\nb":   "a\n\nb",
		"a\n\rb":       "a\n\nb",
		"\r\n":         "\n",
		"tab\tstays\r": "tab\tstays\n",
	} {
		if got := string(normalizeNewlines([]rune(in))); got != want {
			t.Errorf("normalizeNewlines(%q) = %q, want %q", in, got, want)
		}
	}
}

// Characters that are not on the keyboard layout are delivered by the
// classic console host as Alt+numpad: Alt down, digits, and the character on
// the RELEASE of Alt (measured with the inbox conhost for ’, → and €).
func TestAltNumpadCharacters(t *testing.T) {
	const vkNumpad3, vkNumpad9 = 0x63, 0x69
	apostrophe := []record{
		down(vkMenu, 0, leftAltPressed),
		down(vkNumpad3, 0, leftAltPressed), up(vkNumpad3, 0, leftAltPressed),
		down(vkNumpad9, 0, leftAltPressed), up(vkNumpad9, 0, leftAltPressed),
		up(vkMenu, '’', 0),
	}

	// On its own it is one typed character, with no Alt flag.
	check(t, play(&translator{}, at(0, apostrophe...)), `0 key runes "’"`)

	// Inside a paste it is part of the text.
	var batch []record
	batch = append(batch, pasted("don")...)
	batch = append(batch, apostrophe...)
	batch = append(batch, pasted("t")...)
	check(t, play(&translator{}, at(0, batch...)), `30 paste "don’t"`)
}

// --- bracketed paste markers -------------------------------------------------

// Case d as specified: a terminal's ESC[200~ … ESC[201~ markers arriving as
// characters. (Neither console host on this machine passes them on — they
// swallow them, see TestE2EBracketedPaste — so this is covered here.)
func TestBracketedPaste(t *testing.T) {
	got := play(&translator{}, at(0, pasted("\x1b[200~a\rb\x1b[201~")...))
	check(t, got, `0 paste "a\nb"`) // the end marker ends it: no waiting
}

// The markers and the text may be cut into batches anywhere.
func TestBracketedPasteSplitAnywhere(t *testing.T) {
	all := pasted("\x1b[200~a\rb\x1b[201~")
	for cut := 1; cut < len(all); cut++ {
		got := play(&translator{}, at(0, all[:cut]...), at(3, all[cut:]...))
		// The paste completes with the end marker's last key-down, which is
		// in the second batch for every cut but the last one.
		want := `3 paste "a\nb"`
		if cut == len(all)-1 {
			want = `0 paste "a\nb"`
		}
		check(t, got, want)
		if t.Failed() {
			t.Fatalf("failed with the batch cut after record %d", cut)
		}
	}
}

// The markers make even a one-key paste recognisable: a pasted line break is
// text, not the Enter key.
func TestBracketedSingleEnter(t *testing.T) {
	got := play(&translator{}, at(0, pasted("\x1b[200~\r\x1b[201~")...))
	check(t, got, `0 paste "\n"`)
}

// Inside the markers nothing acts as a key, and keys that are not text are
// dropped.
func TestBracketedPasteDropsNonText(t *testing.T) {
	var batch []record
	batch = append(batch, pasted("\x1b[200~a")...)
	batch = append(batch, down(vkUp, 0, 0), down('C', 0x03, leftCtrlPressed), down(vkBack, '\b', 0))
	batch = append(batch, pasted("b\x1b[201~")...)
	check(t, play(&translator{}, at(0, batch...)), `0 paste "ab"`)
}

// If the end marker never comes, the text is released after bracketQuiet
// (500 ms) rather than held forever.
func TestBracketedPasteWithoutEnd(t *testing.T) {
	got := play(&translator{},
		at(0, pasted("\x1b[200~abc")...),
		at(2000, pasted("x")...),
	)
	check(t, got, `500 paste "abc"`, `2000 key runes "x"`)
}

// An end marker with no start is dropped without a trace.
func TestStrayEndMarkerIsDropped(t *testing.T) {
	check(t, play(&translator{}, at(0, pasted("\x1b[201~")...)))
}

// Text right after the end marker, in the same batch, is still handled.
func TestTextAfterEndMarker(t *testing.T) {
	got := play(&translator{}, at(0, pasted("\x1b[200~one\x1b[201~")...), at(200, pasted("x")...))
	check(t, got, `0 paste "one"`, `200 key runes "x"`)
}

// A lone Esc is held only markerWait (10 ms) to see whether a marker
// follows, then delivered.
func TestLoneEscape(t *testing.T) {
	// As Windows Terminal sends it: just the key-down; the key-up follows
	// when the finger lifts.
	got := play(&translator{}, at(0, down(vkEscape, esc, 0)), at(80, up(vkEscape, esc, 0)))
	check(t, got, `10 key "esc"`)

	// As a plain terminal sends it: down and up together.
	got = play(&translator{}, at(0, pasted("\x1b")...))
	check(t, got, `10 key "esc"`)
}

// Esc followed quickly by another key: both are delivered, in order, as soon
// as it is clear that no marker is coming.
func TestEscapeThenKey(t *testing.T) {
	got := play(&translator{}, at(0, down(vkEscape, esc, 0)), at(4, pasted("x")...))
	check(t, got, `4 key "esc"`, `4 key runes "x"`)

	// Even "ESC [" — the first two characters of a marker — typed by hand.
	got = play(&translator{}, at(0, pasted("\x1b")...), at(4, pasted("[")...))
	check(t, got, `14 key "esc"`, `14 key runes "["`)
}

// An Esc at the end of a paste burst ends the paste, in order.
func TestEscapeEndsBurst(t *testing.T) {
	got := play(&translator{}, at(0, pasted("ab\x1b")...))
	check(t, got, `10 paste "ab"`, `10 key "esc"`)
}

// --- characters --------------------------------------------------------------

// Case e: an emoji is two UTF-16 halves, joined into one rune.
func TestSurrogatePairs(t *testing.T) {
	hi, lo := utf16.EncodeRune('😀')

	// Both halves in one batch (Windows Terminal's host: plain key-downs).
	check(t, play(&translator{}, at(0, pasted("😀")...)), `0 key runes "😀"`)

	// The halves in separate batches.
	got := play(&translator{}, at(0, down(0, hi, 0), up(0, hi, 0)), at(3, down(0, lo, 0), up(0, lo, 0)))
	check(t, got, `3 key runes "😀"`)

	// The inbox host's form: each half on the release of an Alt+numpad
	// sequence (measured).
	const vkNumpad6, vkNumpad3 = 0x66, 0x63
	half := func(ch rune) []record {
		return []record{
			down(vkMenu, 0, leftAltPressed),
			down(vkNumpad6, 0, leftAltPressed), up(vkNumpad6, 0, leftAltPressed),
			down(vkNumpad3, 0, leftAltPressed), up(vkNumpad3, 0, leftAltPressed),
			up(vkMenu, ch, 0),
		}
	}
	check(t, play(&translator{}, at(0, append(half(hi), half(lo)...)...)), `0 key runes "😀"`)

	// A second half with no first half is dropped.
	check(t, play(&translator{}, at(0, down(0, lo, 0))))

	// An emoji inside pasted text.
	check(t, play(&translator{}, at(0, pasted("ok 😀!")...)), `30 paste "ok 😀!"`)

	// Two emoji in one batch are two characters at once: a (harmless) paste.
	check(t, play(&translator{}, at(0, pasted("😀🎉")...)), `30 paste "😀🎉"`)
}

// Case f and the rest of the key table.
func TestKeyTable(t *testing.T) {
	const enhanced = 0x0100 // ENHANCED_KEY, set for the arrow cluster; ignored
	cases := []struct {
		name string
		rec  record
		want string // "" = no message
	}{
		{"enter", down(vkReturn, '\r', 0), `key "enter"`},
		{"shift+enter", down(vkReturn, '\r', shiftPressed), `key "ctrl+j"`},
		{"ctrl+enter", down(vkReturn, '\n', leftCtrlPressed), `key "ctrl+j"`},
		{"ctrl+shift+enter", down(vkReturn, '\n', leftCtrlPressed|shiftPressed), `key "ctrl+j"`},
		{"alt+enter", down(vkReturn, '\r', leftAltPressed), `key "enter" alt`},
		{"ctrl+c", down('C', 0x03, leftCtrlPressed), `key "ctrl+c"`},
		{"ctrl+c right ctrl", down('C', 0x03, rightCtrlPressed), `key "ctrl+c"`},
		{"ctrl+a", down('A', 0x01, leftCtrlPressed), `key "ctrl+a"`},
		{"ctrl+z", down('Z', 0x1a, leftCtrlPressed), `key "ctrl+z"`},
		{"ctrl+j", down('J', '\n', leftCtrlPressed), `key "ctrl+j"`},
		{"ctrl+m is enter", down('M', '\r', leftCtrlPressed), `key "enter"`},
		{"ctrl+i is tab", down('I', '\t', leftCtrlPressed), `key "tab"`},
		{"ctrl+h", down('H', '\b', leftCtrlPressed), `key "ctrl+h"`},
		{"ctrl+[", down(vkOem4, 0x1b, leftCtrlPressed), `key "esc"`},
		{"ctrl+]", down(vkOem6, 0x1d, leftCtrlPressed), `key "ctrl+]"`},
		{"ctrl+\\", down(0xDC, 0x1c, leftCtrlPressed), `key "ctrl+\\"`},
		{"ctrl+alt+a", down('A', 0, leftCtrlPressed|leftAltPressed), `key "ctrl+a" alt`},
		{"esc", down(vkEscape, esc, 0), `key "esc"`},
		{"backspace", down(vkBack, '\b', 0), `key "backspace"`},
		{"ctrl+backspace", down(vkBack, 0x7f, leftCtrlPressed), `key "backspace"`},
		{"tab", down(vkTab, '\t', 0), `key "tab"`},
		{"shift+tab", down(vkTab, '\t', shiftPressed), `key "shift+tab"`},
		{"space", down(vkSpace, ' ', 0), `key runes " "`},
		{"letter", down('A', 'a', 0), `key runes "a"`},
		{"capital", down('A', 'A', shiftPressed), `key runes "A"`},
		{"alt+x", down('X', 'x', leftAltPressed), `key runes "x" alt`},
		{"right alt+x", down('X', 'x', rightAltPressed), `key runes "x" alt`},
		{"altgr @ (ctrl+alt makes a character)", down('Q', '@', leftCtrlPressed|rightAltPressed), `key runes "@"`},
		{"altgr €", down('E', '€', leftCtrlPressed|rightAltPressed), `key runes "€"`},
		{"up", down(vkUp, 0, enhanced), `key "up"`},
		{"shift+up", down(vkUp, 0, shiftPressed), `key "shift+up"`},
		{"ctrl+up", down(vkUp, 0, leftCtrlPressed), `key "ctrl+up"`},
		{"ctrl+shift+up", down(vkUp, 0, leftCtrlPressed|shiftPressed), `key "ctrl+shift+up"`},
		{"alt+up", down(vkUp, 0, leftAltPressed), `key "up" alt`},
		{"down", down(vkDown, 0, 0), `key "down"`},
		{"ctrl+left", down(vkLeft, 0, rightCtrlPressed), `key "ctrl+left"`},
		{"shift+right", down(vkRight, 0, shiftPressed), `key "shift+right"`},
		{"home", down(vkHome, 0, 0), `key "home"`},
		{"ctrl+shift+end", down(vkEnd, 0, leftCtrlPressed|shiftPressed), `key "ctrl+shift+end"`},
		{"pgup", down(vkPrior, 0, 0), `key "pgup"`},
		{"ctrl+pgdown", down(vkNext, 0, leftCtrlPressed), `key "ctrl+pgdown"`},
		{"delete", down(vkDelete, 0, 0), `key "delete"`},
		{"insert", down(vkInsert, 0, 0), `key "insert"`},
		{"f1", down(vkF1, 0, 0), `key "f1"`},
		{"f5", down(vkF1+4, 0, 0), `key "f5"`},
		{"f12", down(vkF1+11, 0, 0), `key "f12"`},
		{"f20", down(vkF20, 0, 0), `key "f20"`},

		// Records that must produce nothing.
		{"shift alone", down(0x10, 0, shiftPressed), ""},
		{"ctrl alone", down(0x11, 0, leftCtrlPressed), ""},
		{"alt alone", down(vkMenu, 0, leftAltPressed), ""},
		{"caps lock", down(0x14, 0, 0), ""},
		{"windows key", down(0x5B, 0, 0), ""},
		{"key up", up('A', 'a', 0), ""},
		{"enter up", up(vkReturn, '\r', 0), ""},
		{"alt up without a character", up(vkMenu, 0, 0), ""},
	}
	for _, c := range cases {
		tr := &translator{}
		msgs := tr.feed([]record{c.rec}, time.Unix(0, 0))
		// A lone Esc is held back for markerWait; let it out.
		msgs = append(msgs, tr.expire(time.Unix(1, 0))...)
		var got []string
		for _, m := range msgs {
			got = append(got, describe(m))
		}
		var want []string
		if c.want != "" {
			want = []string{c.want}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %q, want %q", c.name, got, want)
		}
	}
}

// --- mouse, focus, resize ----------------------------------------------------

func mouse(x, y int, buttons, flags uint32) record {
	return record{kind: recMouse, mouse: mouseRecord{x: x, y: y, buttons: buttons, flags: flags}}
}

func TestMouse(t *testing.T) {
	const wheelUp, wheelDown = 0x00800000, 0xFF800000 // +120 and -120 in the high word
	tr := &translator{mouse: true}
	got := play(tr,
		at(0, mouse(9, 4, wheelUp, mouseWheeled)),
		at(1, mouse(9, 4, wheelDown, mouseWheeled)),
		at(2, mouse(3, 2, buttonLeft, 0)),                       // left button down
		at(3, mouse(4, 2, buttonLeft, mouseMoved)),              // dragged
		at(4, mouse(4, 2, 0, 0)),                                // released
		at(5, mouse(7, 7, 0, mouseMoved)),                       // plain movement: dropped
		at(6, mouse(1, 1, buttonRight, 0)),                      // right button down
		at(7, mouse(1, 1, 0, 0)),                                // released
		at(8, mouse(2, 2, wheelUp, mouseHWheeled)),              // horizontal wheel
		at(9, mouse(2, 2, buttonMiddle, 0)),                     // middle button down
		at(10, mouse(2, 2, buttonMiddle|wheelUp, mouseWheeled)), // wheel while a button is held
		at(11, mouse(2, 2, 0, 0)),                               // middle released (the wheel must not confuse this)
	)
	check(t, got,
		`0 mouse "wheel up" at 9,4`,
		`1 mouse "wheel down" at 9,4`,
		`2 mouse "left press" at 3,2`,
		`3 mouse "left motion" at 4,2`,
		`4 mouse "left release" at 4,2`,
		`6 mouse "right press" at 1,1`,
		`7 mouse "right release" at 1,1`,
		`8 mouse "wheel right" at 2,2`,
		`9 mouse "middle press" at 2,2`,
		`10 mouse "wheel up" at 2,2`,
		`11 mouse "middle release" at 2,2`,
	)

	// Without Options.Mouse nothing is reported.
	check(t, play(&translator{}, at(0, mouse(9, 4, wheelUp, mouseWheeled))))
}

// Mouse positions are made relative to the visible window (a classic console
// window can be scrolled within a taller buffer).
func TestMousePositionIsWindowRelative(t *testing.T) {
	tr := &translator{mouse: true, window: func() (int, int, int, int, bool) { return 0, 100, 80, 25, true }}
	check(t, play(tr, at(0, mouse(5, 103, buttonLeft, 0))), `0 mouse "left press" at 5,3`)
}

func TestFocus(t *testing.T) {
	got := play(&translator{},
		at(0, record{kind: recFocus, focused: false}),
		at(5, record{kind: recFocus, focused: true}),
	)
	check(t, got, `0 blur`, `5 focus`)
}

func TestResize(t *testing.T) {
	// The window is asked for its size; the record's own numbers (the
	// screen buffer, here much taller) are ignored. Repeats are dropped.
	width, height := 120, 40
	tr := &translator{window: func() (int, int, int, int, bool) { return 0, 0, width, height, true }}
	resize := record{kind: recResize, width: 120, height: 9001}
	step1 := play(tr, at(0, resize), at(1, resize))
	check(t, step1, `0 size 120x40`)
	width = 100
	check(t, play(tr, at(2, resize)), `2 size 100x40`)

	// With no way to ask, the record's size is used.
	check(t, play(&translator{}, at(0, record{kind: recResize, width: 80, height: 25})), `0 size 80x25`)
}

// Mouse, focus and resize events arriving in the middle of a paste are
// reported at once and do not cut the paste in two.
func TestOtherEventsDoNotSplitPaste(t *testing.T) {
	got := play(&translator{mouse: true},
		at(0, pasted("one\r")...),
		at(5, record{kind: recFocus, focused: true}, mouse(1, 1, buttonLeft, 0)),
		at(10, pasted("two")...),
	)
	check(t, got, `5 focus`, `5 mouse "left press" at 1,1`, `40 paste "one\ntwo"`)
}

// --- a large paste -----------------------------------------------------------

// Case g: a large paste cut into pieces at random places, with random small
// gaps, always comes out as exactly one paste with every character in order
// and never as a bare Enter.
func TestLargePasteInRandomPieces(t *testing.T) {
	var text strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&text, "line %03d: the quick brown fox — jumps over the lazy dog 😀\r", i)
	}
	want := fmt.Sprintf("paste %q", strings.ReplaceAll(text.String(), "\r", "\n"))
	all := pasted(text.String())

	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		var steps []step
		now := 0
		for rest := all; len(rest) > 0; {
			n := 1 + rng.Intn(400)
			if round%2 == 1 {
				n = 1 + rng.Intn(6) // tiny pieces: many are a single key
			}
			if n > len(rest) {
				n = len(rest)
			}
			// The very first piece must be recognisable as a burst (two
			// text keys); after that, anything goes.
			if len(steps) == 0 && n < 4 {
				n = 4
			}
			steps = append(steps, at(now, rest[:n]...))
			rest = rest[n:]
			now += rng.Intn(int(pasteQuiet/time.Millisecond)) + 1 // 1..30 ms: never longer than the quiet period
		}
		got := play(&translator{}, steps...)
		if len(got) != 1 || !strings.HasSuffix(got[0], want) {
			t.Fatalf("round %d: got %d messages, first %.80q", round, len(got), got)
		}
	}
}
