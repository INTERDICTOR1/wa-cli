package tui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// editorCharLimit is WhatsApp's own limit on a text message's length.
const editorCharLimit = 65536

// editor is the message box: plain multi-line text with a cursor. It
// soft-wraps to its width and grows from one row up to maxRows before it
// scrolls. It exists (rather than using a stock text input) so the cursor
// position is a simple index into the text - which the @mention and
// :emoji: dropdowns need - and so a pasted line break is just a character.
type editor struct {
	text   []rune
	cursor int // index into text: 0..len(text)

	width   int // columns available for text
	maxRows int // rows shown before scrolling
	top     int // first visible row
	goalCol int // column to aim for when moving up/down; -1 = unset
}

func newEditor() editor {
	return editor{width: 40, maxRows: 6, goalCol: -1}
}

// row is one screen row of the wrapped text: text[start:end]. A row never
// includes the line break that ends it.
type row struct{ start, end int }

func (e *editor) Value() string { return string(e.text) }
func (e *editor) Empty() bool   { return len(e.text) == 0 }
func (e *editor) Cursor() int   { return e.cursor }

// SetValue replaces the text and puts the cursor at the end.
func (e *editor) SetValue(s string) {
	e.text = []rune(cleanInput(s))
	if len(e.text) > editorCharLimit {
		e.text = e.text[:editorCharLimit]
	}
	e.cursor = len(e.text)
	e.goalCol = -1
	e.scrollToCursor()
}

func (e *editor) Reset() {
	e.text, e.cursor, e.top, e.goalCol = nil, 0, 0, -1
}

// SetCursor moves the cursor to rune index i.
func (e *editor) SetCursor(i int) {
	e.cursor = max(0, min(i, len(e.text)))
	e.goalCol = -1
	e.scrollToCursor()
}

// SetWidth sets the text columns available.
func (e *editor) SetWidth(w int) {
	e.width = max(4, w)
	e.scrollToCursor()
}

// Insert adds s at the cursor (typed or pasted text; line breaks allowed).
func (e *editor) Insert(s string) {
	ins := []rune(cleanInput(s))
	if room := editorCharLimit - len(e.text); len(ins) > room {
		ins = ins[:max(0, room)]
	}
	if len(ins) == 0 {
		return
	}
	text := make([]rune, 0, len(e.text)+len(ins))
	text = append(text, e.text[:e.cursor]...)
	text = append(text, ins...)
	text = append(text, e.text[e.cursor:]...)
	e.text = text
	e.cursor += len(ins)
	e.goalCol = -1
	e.scrollToCursor()
}

// Replace swaps text[start:end] for s and leaves the cursor after it.
func (e *editor) Replace(start, end int, s string) {
	start = max(0, min(start, len(e.text)))
	end = max(start, min(end, len(e.text)))
	e.text = append(e.text[:start:start], e.text[end:]...)
	e.cursor = start
	e.Insert(s)
}

// cleanInput normalises line breaks to '\n' and drops control characters,
// which would otherwise be sent as invisible junk.
func cleanInput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// --- deleting ---

func (e *editor) deleteRange(start, end int) {
	if start >= end {
		return
	}
	e.text = append(e.text[:start:start], e.text[end:]...)
	e.cursor = start
	e.goalCol = -1
	e.scrollToCursor()
}

// Backspace deletes the character before the cursor. A "character" is a
// whole grapheme cluster, so an emoji made of several code points goes in
// one press.
func (e *editor) Backspace()      { e.deleteRange(e.prevBoundary(e.cursor), e.cursor) }
func (e *editor) Delete()         { e.deleteRange(e.cursor, e.nextBoundary(e.cursor)) }
func (e *editor) DeleteWordBack() { e.deleteRange(e.wordLeft(e.cursor), e.cursor) }
func (e *editor) DeleteWordForward() {
	e.deleteRange(e.cursor, e.wordRight(e.cursor))
}
func (e *editor) KillToLineStart() { e.deleteRange(e.lineStart(e.cursor), e.cursor) }
func (e *editor) KillToLineEnd()   { e.deleteRange(e.cursor, e.lineEnd(e.cursor)) }

// --- moving ---

func (e *editor) Left()      { e.SetCursor(e.prevBoundary(e.cursor)) }
func (e *editor) Right()     { e.SetCursor(e.nextBoundary(e.cursor)) }
func (e *editor) WordLeft()  { e.SetCursor(e.wordLeft(e.cursor)) }
func (e *editor) WordRight() { e.SetCursor(e.wordRight(e.cursor)) }
func (e *editor) Start()     { e.SetCursor(0) }
func (e *editor) End()       { e.SetCursor(len(e.text)) }

// Home and LineEnd move within the current screen row.
func (e *editor) Home() {
	rows := e.rows()
	e.SetCursor(rows[e.cursorRow(rows)].start)
}

func (e *editor) LineEnd() {
	rows := e.rows()
	e.SetCursor(rows[e.cursorRow(rows)].end)
}

// Up moves one screen row up; false if already on the first row.
func (e *editor) Up() bool { return e.vertical(-1) }

// Down moves one screen row down; false if already on the last row.
func (e *editor) Down() bool { return e.vertical(1) }

func (e *editor) vertical(delta int) bool {
	rows := e.rows()
	r := e.cursorRow(rows)
	if r+delta < 0 || r+delta >= len(rows) {
		return false
	}
	if e.goalCol < 0 {
		e.goalCol = ansi.StringWidth(string(e.text[rows[r].start:e.cursor]))
	}
	goal := e.goalCol
	target := rows[r+delta]
	// Walk the target row cluster by cluster until the goal column.
	pos, col := target.start, 0
	g := uniseg.NewGraphemes(string(e.text[target.start:target.end]))
	for g.Next() {
		w := ansi.StringWidth(g.Str())
		if col+w > goal {
			break
		}
		col += w
		pos += len(g.Runes())
	}
	e.cursor = pos
	e.goalCol = goal
	e.scrollToCursor()
	return true
}

// lineStart/lineEnd are the bounds of the logical line (between '\n's).
func (e *editor) lineStart(i int) int {
	for i > 0 && e.text[i-1] != '\n' {
		i--
	}
	return i
}

func (e *editor) lineEnd(i int) int {
	for i < len(e.text) && e.text[i] != '\n' {
		i++
	}
	return i
}

// prevBoundary/nextBoundary step over one grapheme cluster.
func (e *editor) prevBoundary(i int) int {
	if i <= 0 {
		return 0
	}
	if e.text[i-1] == '\n' {
		return i - 1
	}
	start := e.lineStart(i)
	pos, last := start, start
	g := uniseg.NewGraphemes(string(e.text[start:i]))
	for g.Next() {
		last = pos
		pos += len(g.Runes())
	}
	return last
}

func (e *editor) nextBoundary(i int) int {
	if i >= len(e.text) {
		return len(e.text)
	}
	if e.text[i] == '\n' {
		return i + 1
	}
	g := uniseg.NewGraphemes(string(e.text[i:e.lineEnd(i)]))
	if g.Next() {
		return i + len(g.Runes())
	}
	return i + 1
}

func (e *editor) wordLeft(i int) int {
	for i > 0 && unicode.IsSpace(e.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.text[i-1]) {
		i--
	}
	return i
}

func (e *editor) wordRight(i int) int {
	for i < len(e.text) && unicode.IsSpace(e.text[i]) {
		i++
	}
	for i < len(e.text) && !unicode.IsSpace(e.text[i]) {
		i++
	}
	return i
}

// --- layout ---

// rows wraps the text to the editor's width: at spaces where it can, in
// the middle of a word when a single word is longer than a row.
func (e *editor) rows() []row {
	var rows []row
	lineStart := 0
	for {
		lineEnd := e.lineEnd(lineStart)
		rows = e.wrapLine(rows, lineStart, lineEnd)
		if lineEnd >= len(e.text) {
			break
		}
		lineStart = lineEnd + 1
	}
	return rows
}

func (e *editor) wrapLine(rows []row, start, end int) []row {
	if start == end {
		return append(rows, row{start, end})
	}
	rowStart, col := start, 0
	lastSpace := -1 // index just after the last space in this row
	pos := start
	g := uniseg.NewGraphemes(string(e.text[start:end]))
	for g.Next() {
		n := len(g.Runes())
		w := ansi.StringWidth(g.Str())
		if col+w > e.width && pos > rowStart {
			breakAt := pos
			if lastSpace > rowStart {
				breakAt = lastSpace
			}
			rows = append(rows, row{rowStart, breakAt})
			col = ansi.StringWidth(string(e.text[breakAt:pos]))
			rowStart, lastSpace = breakAt, -1
		}
		col += w
		pos += n
		if n == 1 && e.text[pos-1] == ' ' {
			lastSpace = pos
		}
	}
	return append(rows, row{rowStart, end})
}

// cursorRow returns the index of the row holding the cursor. A cursor
// sitting exactly where a row wraps belongs to the start of the next row.
func (e *editor) cursorRow(rows []row) int {
	for i, r := range rows {
		if e.cursor < r.end {
			return i
		}
		if e.cursor == r.end && (i == len(rows)-1 || rows[i+1].start != r.end) {
			return i
		}
	}
	return len(rows) - 1
}

// OnFirstRow / OnLastRow report where the cursor is, so the arrow keys can
// leave the box at its edges.
func (e *editor) OnFirstRow() bool { return e.cursorRow(e.rows()) == 0 }
func (e *editor) OnLastRow() bool {
	rows := e.rows()
	return e.cursorRow(rows) == len(rows)-1
}

// Height is how many rows the editor takes on screen.
func (e *editor) Height() int {
	return max(1, min(len(e.rows()), e.maxRows))
}

func (e *editor) scrollToCursor() {
	rows := e.rows()
	r := e.cursorRow(rows)
	h := max(1, min(len(rows), e.maxRows))
	if r < e.top {
		e.top = r
	}
	if r >= e.top+h {
		e.top = r - h + 1
	}
	e.top = max(0, min(e.top, len(rows)-h))
}

// View draws the editor: Height() lines. The cursor is a reversed cell,
// shown only when focused.
func (e *editor) View(focused bool, placeholder string) string {
	if len(e.text) == 0 {
		if focused {
			rest := ""
			if r := []rune(placeholder); len(r) > 0 {
				rest = string(r[1:])
				return cursorStyle.Render(string(r[0])) + dimStyle.Render(ansi.Truncate(rest, e.width-1, ""))
			}
			return cursorStyle.Render(" ")
		}
		return dimStyle.Render(ansi.Truncate(placeholder, e.width, ""))
	}
	rows := e.rows()
	cur := e.cursorRow(rows)
	h := max(1, min(len(rows), e.maxRows))
	top := max(0, min(e.top, len(rows)-h))

	lines := make([]string, 0, h)
	for i := top; i < top+h; i++ {
		r := rows[i]
		if !focused || i != cur {
			lines = append(lines, ansi.Truncate(string(e.text[r.start:r.end]), e.width+1, ""))
			continue
		}
		before := string(e.text[r.start:e.cursor])
		at, after := " ", ""
		if e.cursor < r.end {
			next := e.nextBoundary(e.cursor)
			at = string(e.text[e.cursor:next])
			after = string(e.text[next:r.end])
		}
		lines = append(lines, before+cursorStyle.Render(at)+ansi.Truncate(after, max(0, e.width-ansi.StringWidth(before)-ansi.StringWidth(at)+1), ""))
	}
	return strings.Join(lines, "\n")
}
