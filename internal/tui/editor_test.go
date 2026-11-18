package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestEditorInsertAndNewlines(t *testing.T) {
	e := newEditor()
	e.SetWidth(20)
	e.Insert("hello")
	e.Insert("\r\nworld\rthird\x1b[2J\tx")
	if got := e.Value(); got != "hello\nworld\nthird[2J x" {
		t.Fatalf("value = %q", got)
	}
	if e.Height() != 3 {
		t.Fatalf("height = %d", e.Height())
	}
}

func TestEditorWrapsAndGrowsToLimit(t *testing.T) {
	e := newEditor()
	e.SetWidth(10)
	e.Insert("aaa bbb ccc ddd eee fff")
	rows := e.rows()
	var got []string
	for _, r := range rows {
		got = append(got, string(e.text[r.start:r.end]))
	}
	if strings.Join(got, "|") != "aaa bbb |ccc ddd |eee fff" {
		t.Fatalf("rows = %q", got)
	}
	e.SetValue(strings.Repeat("x\n", 20))
	if e.Height() != e.maxRows {
		t.Fatalf("height = %d, want the limit %d", e.Height(), e.maxRows)
	}
	for _, line := range strings.Split(e.View(true, ""), "\n") {
		if w := ansi.StringWidth(line); w > 11 {
			t.Fatalf("line %q is %d wide", ansi.Strip(line), w)
		}
	}
	// A word longer than the row is broken, not lost.
	e.SetValue(strings.Repeat("z", 25))
	if n := len(e.rows()); n != 3 {
		t.Fatalf("long word wrapped into %d rows, want 3", n)
	}
}

func TestEditorCursorMovesByWholeCharacters(t *testing.T) {
	e := newEditor()
	e.SetWidth(40)
	family := "👨‍👩‍👧" // one visible character made of five code points
	e.Insert("a" + family + "b")
	e.Left()
	e.Left()
	if e.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1 (before the family emoji)", e.Cursor())
	}
	e.Right()
	e.Backspace()
	if got := e.Value(); got != "ab" {
		t.Fatalf("backspace left %q", got)
	}
}

func TestEditorUpDownAcrossLines(t *testing.T) {
	e := newEditor()
	e.SetWidth(40)
	e.Insert("first line\nx\nthird line")
	if e.Down() {
		t.Fatal("down on the last row should report false")
	}
	if !e.Up() || !e.Up() {
		t.Fatal("up should move twice")
	}
	if !e.OnFirstRow() || e.Cursor() != len("first line") {
		t.Fatalf("cursor = %d, want end of the first line (goal column kept)", e.Cursor())
	}
	if e.Up() {
		t.Fatal("up on the first row should report false")
	}
}

func TestEditorReplaceAndKill(t *testing.T) {
	e := newEditor()
	e.SetWidth(40)
	e.Insert("hi @sha and more")
	e.Replace(3, 7, "@Shashwat ")
	if got := e.Value(); got != "hi @Shashwat  and more" || e.Cursor() != 13 {
		t.Fatalf("value=%q cursor=%d", got, e.Cursor())
	}
	e.KillToLineEnd()
	e.DeleteWordBack()
	if got := e.Value(); got != "hi " {
		t.Fatalf("value=%q", got)
	}
	e.KillToLineStart()
	if !e.Empty() {
		t.Fatalf("value=%q", e.Value())
	}
}

func TestSanitizeAndHelpers(t *testing.T) {
	if got := sanitize("a\x1b[31mb\x07c\td\ne"); got != "a[31mbc d\ne" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := oneLine("a\n\n  b\tc"); got != "a b c" {
		t.Fatalf("oneLine = %q", got)
	}
	if got := firstURL("see https://example.com/x?y=1). ok"); got != "https://example.com/x?y=1" {
		t.Fatalf("firstURL = %q", got)
	}
	if _, ok := asFilePath("just some words"); ok {
		t.Fatal("plain text taken for a file")
	}
	if got := padRight("ab", 5); got != "ab   " {
		t.Fatalf("padRight = %q", got)
	}
	if w := ansi.StringWidth(spread("left side", "right", 12)); w != 12 {
		t.Fatalf("spread width = %d", w)
	}
}

func TestEmojiAndMentionQueries(t *testing.T) {
	for _, c := range []struct {
		value string
		q     string
		ok    bool
	}{
		{":smi", "smi", true},
		{"ok :thumbsup", "thumbsup", true},
		{"10:30", "", false},
		{":)", "", false},
		{":a", "", false},
		{"line\n:fire", "fire", true},
	} {
		q, _, ok := emojiQuery(c.value, len([]rune(c.value)))
		if q != c.q || ok != c.ok {
			t.Errorf("emojiQuery(%q) = (%q, %v), want (%q, %v)", c.value, q, ok, c.q, c.ok)
		}
	}
	if q, start, ok := mentionQuery("hi\n@sha", 7); !ok || q != "sha" || start != 3 {
		t.Errorf("mention after a line break: %q %d %v", q, start, ok)
	}
	if _, _, ok := mentionQuery("@sha\nmore", 9); ok {
		t.Error("a mention must not span lines")
	}
}
