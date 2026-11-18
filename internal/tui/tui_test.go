package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

func threeChats() *fakeBackend {
	be := newFake()
	be.chats = []whatsapp.ChatView{
		chat("1@s.whatsapp.net", "Alice", 3),
		chat("2@g.us", "Jobs Group", 2),
		chat("3@s.whatsapp.net", "Bob", 1),
	}
	be.msgs["1@s.whatsapp.net"] = []whatsapp.MessageView{
		text("1@s.whatsapp.net", "a1", "Alice", "hello there", false),
		text("1@s.whatsapp.net", "a2", "", "hi Alice", true),
	}
	return be
}

// The reported bug: pasting text with line breaks sent the first line at
// once and every other line as its own message.
func TestPastedLineBreaksStayInOneMessage(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter) // open Alice

	h.paste("first line\r\nsecond line\nthird line")
	if len(be.sent) != 0 {
		t.Fatalf("pasting sent %d messages; it must only fill the box", len(be.sent))
	}
	if got := h.m.ed.Value(); got != "first line\nsecond line\nthird line" {
		t.Fatalf("box holds %q", got)
	}
	if rows := h.m.ed.Height(); rows != 3 {
		t.Fatalf("box shows %d rows, want 3", rows)
	}
	h.frame()

	h.key(tea.KeyEnter)
	if len(be.sent) != 1 {
		t.Fatalf("Enter sent %d messages, want exactly 1", len(be.sent))
	}
	if got := be.sent[0].out.Text; got != "first line\nsecond line\nthird line" {
		t.Fatalf("sent %q", got)
	}
	if !h.m.ed.Empty() {
		t.Fatal("box not cleared after sending")
	}
}

func TestNewlineKeysDoNotSend(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)

	h.typeText("one")
	h.key(tea.KeyCtrlJ) // shift+enter / ctrl+enter on Windows, ctrl+j anywhere
	h.typeText("two")
	h.send(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	h.typeText("three")
	if len(be.sent) != 0 {
		t.Fatalf("a newline key sent a message")
	}
	h.key(tea.KeyEnter)
	if len(be.sent) != 1 || be.sent[0].out.Text != "one\ntwo\nthree" {
		t.Fatalf("sent = %+v", be.sent)
	}
}

func TestFilterPreviewOpenAndFocus(t *testing.T) {
	be := threeChats()
	be.chats[1].UnreadCount = 4
	h := newHarness(t, be, 100, 30)

	// Moving the highlight previews the chat, keeps focus on the list and
	// doesn't mark it read.
	h.key(tea.KeyDown)
	if h.m.selectedJID != "2@g.us" || h.m.openJID != "2@g.us" || h.m.focus != focusChatList {
		t.Fatalf("after down: selected=%q open=%q focus=%d", h.m.selectedJID, h.m.openJID, h.m.focus)
	}
	if len(be.read) != 0 {
		t.Fatalf("previewing marked %v read", be.read)
	}

	// Tab switches focus to the message box (which reads the chat) and back.
	h.key(tea.KeyTab)
	if h.m.focus != focusInput || len(be.read) != 1 || be.read[0] != "2@g.us" {
		t.Fatalf("tab from list: focus=%d read=%v", h.m.focus, be.read)
	}
	h.key(tea.KeyTab)
	if h.m.focus != focusChatList {
		t.Fatalf("tab from input: focus=%d", h.m.focus)
	}

	// Typing filters; arrows only move.
	h.typeText("jo")
	if h.m.filter != "jo" || len(h.m.visible) != 1 || h.m.selectedJID != "2@g.us" {
		t.Fatalf("filter %q: visible=%d selected=%q", h.m.filter, len(h.m.visible), h.m.selectedJID)
	}
	if view := h.frame(); !strings.Contains(view, "Search: jo") || strings.Contains(view, "Alice") {
		t.Fatalf("view doesn't show only the filtered list")
	}
	h.key(tea.KeyBackspace)
	if h.m.filter != "j" {
		t.Fatalf("backspace: filter=%q", h.m.filter)
	}
	h.key(tea.KeyEsc)
	if h.m.filter != "" || len(h.m.visible) != 3 {
		t.Fatalf("esc should clear the filter, got %q", h.m.filter)
	}

	// Enter opens the highlighted chat, clears the filter, focuses the box.
	h.typeText("bob")
	h.key(tea.KeyEnter)
	if h.m.openJID != "3@s.whatsapp.net" || h.m.filter != "" || h.m.focus != focusInput {
		t.Fatalf("enter: open=%q filter=%q focus=%d", h.m.openJID, h.m.filter, h.m.focus)
	}
	// Esc returns to the list with that chat highlighted.
	h.key(tea.KeyEsc)
	if h.m.focus != focusChatList || h.m.selectedJID != "3@s.whatsapp.net" {
		t.Fatalf("esc: focus=%d selected=%q", h.m.focus, h.m.selectedJID)
	}
}

// Typing a contact's name offers to start a chat with them.
func TestFilterOffersContacts(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.typeText("zed")
	if len(h.m.visible) != 1 || !h.m.visible[0].contact {
		t.Fatalf("visible = %+v", h.m.visible)
	}
	h.key(tea.KeyEnter)
	h.typeText("hey")
	h.key(tea.KeyEnter)
	if len(be.sent) != 1 || be.sent[0].chat != "999@s.whatsapp.net" {
		t.Fatalf("sent = %+v", be.sent)
	}
}

func TestFrameAlwaysFitsTerminal(t *testing.T) {
	be := newFake()
	for i := 0; i < 300; i++ {
		c := chat(fmt.Sprintf("%d@s.whatsapp.net", i), fmt.Sprintf("Contact number %d with a long name 😀", i), int64(1000-i))
		c.UnreadCount = i % 3
		c.LastMessagePreview = "a preview that is rather long and would not fit on one line of the list"
		c.Pinned = i == 7
		be.chats = append(be.chats, c)
	}
	jid := "7@s.whatsapp.net" // the pinned chat: first in the list
	for i := 0; i < 200; i++ {
		v := text(jid, fmt.Sprintf("m%d", i), "Contact", strings.Repeat("word ", i%40+1)+"😀 end", i%3 == 0)
		if i%7 == 0 {
			v.Quote = &whatsapp.Quote{ID: "x", Sender: "Someone", Text: strings.Repeat("quoted ", 30)}
			v.Reactions = []whatsapp.ReactionView{{Emoji: "👍", Count: 3, Mine: true}, {Emoji: "❤️", Count: 1}}
		}
		if i%11 == 0 {
			v.Kind, v.Media = whatsapp.KindImage, &whatsapp.MediaInfo{Width: 1600, Height: 900, Size: 2 << 20, HasThumb: true}
			v.HD = true
		}
		if i%13 == 0 {
			v.Kind, v.Body = whatsapp.KindPoll, ""
			v.Poll = &whatsapp.PollView{Question: "Where?", Options: []whatsapp.PollOption{{Name: "Here", Votes: 3, Mine: true}, {Name: "There", Votes: 1}}}
		}
		if i%17 == 0 {
			v.Kind, v.Body = whatsapp.KindSystem, "Someone changed the group description"
		}
		if i%19 == 0 {
			v.Deleted, v.Kind = true, whatsapp.KindDeleted
		}
		be.msgs[jid] = append(be.msgs[jid], v)
	}

	for _, size := range [][2]int{{120, 40}, {80, 24}, {200, 60}, {60, 20}, {40, 12}} {
		h := newHarness(t, be, size[0], size[1])
		h.frame()
		h.key(tea.KeyEnter)
		h.frame()
		h.paste("a\nb\nc\nd\ne\nf\ng\nh\ni") // taller than the box's limit
		h.frame()
		h.key(tea.KeyEsc)
		h.key(tea.KeyEnter)
		for i := 0; i < 12; i++ {
			h.key(tea.KeyPgUp)
		}
		h.frame()
		h.key(tea.KeyF1)
		h.frame()
		h.key(tea.KeyEsc)
		h.key(tea.KeyCtrlF)
		h.typeText("word")
		h.frame()
		h.key(tea.KeyEsc)
		h.m.ed.Reset() // with text in the box, Up would move inside it first
		for i := 0; i < 5; i++ {
			h.key(tea.KeyUp)
		}
		if h.m.focus != focusMessages {
			t.Fatalf("%v: up from the box should select a message, focus=%d", size, h.m.focus)
		}
		h.frame()
		h.typeText("e")
		h.frame()
	}
}

func TestIncomingWhileScrolledUpKeepsPlace(t *testing.T) {
	be := newFake()
	jid := "1@s.whatsapp.net"
	be.chats = []whatsapp.ChatView{chat(jid, "Alice", 1)}
	for i := 0; i < 60; i++ {
		be.msgs[jid] = append(be.msgs[jid], text(jid, fmt.Sprintf("m%d", i), "Alice", fmt.Sprintf("message %d", i), false))
	}
	h := newHarness(t, be, 100, 24)
	h.key(tea.KeyEnter)
	h.key(tea.KeyPgUp)
	if h.m.scrollBack == 0 {
		t.Fatal("page up didn't scroll")
	}
	before := h.frame()

	incoming := text(jid, "new1", "Alice", "something new", false)
	be.msgs[jid] = append(be.msgs[jid], incoming)
	h.send(backendEventMsg{whatsapp.MessageEvent{ChatJID: jid, ID: "new1", New: true}})
	after := h.frame()
	if strings.Contains(after, "something new") {
		t.Fatal("the view jumped to the new message while reading older ones")
	}
	if h.m.newBelow != 1 || !strings.Contains(after, "1 new") {
		t.Fatalf("newBelow=%d, header should announce it", h.m.newBelow)
	}
	// Same messages still on screen.
	if firstMessageLine(before) != firstMessageLine(after) {
		t.Fatalf("scroll position moved:\n%q\n%q", firstMessageLine(before), firstMessageLine(after))
	}

	// Your own message always jumps to the bottom.
	mine := text(jid, "mine1", "", "my reply", true)
	be.msgs[jid] = append(be.msgs[jid], mine)
	h.send(backendEventMsg{whatsapp.MessageEvent{ChatJID: jid, ID: "mine1", New: true}})
	if h.m.scrollBack != 0 || !strings.Contains(h.frame(), "my reply") {
		t.Fatal("own message didn't scroll to the bottom")
	}
}

func firstMessageLine(frame string) string {
	for _, l := range strings.Split(frame, "\n") {
		if i := strings.Index(l, "message "); i >= 0 {
			return strings.TrimSpace(l[i:])
		}
	}
	return ""
}

func TestMessageChangesInPlace(t *testing.T) {
	be := threeChats()
	jid := "1@s.whatsapp.net"
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)

	// A status change redraws the ticks.
	be.msgs[jid][1].Status = msgstore.StatusRead
	h.send(backendEventMsg{whatsapp.MessageEvent{ChatJID: jid, ID: "a2"}})
	if !strings.Contains(h.frame(), "✓✓") {
		t.Fatal("read ticks not shown")
	}
	// An edit replaces the text.
	be.msgs[jid][0].Body, be.msgs[jid][0].Edited = "hello again", true
	h.send(backendEventMsg{whatsapp.MessageEvent{ChatJID: jid, ID: "a1"}})
	if view := h.frame(); !strings.Contains(view, "hello again") || strings.Contains(view, "hello there") || !strings.Contains(view, "edited") {
		t.Fatalf("edit not applied:\n%s", view)
	}
	if len(h.m.msgs) != 2 {
		t.Fatalf("an update added a message: %d", len(h.m.msgs))
	}
	// A local delete removes it.
	be.msgs[jid] = be.msgs[jid][1:]
	h.send(backendEventMsg{whatsapp.MessageEvent{ChatJID: jid, ID: "a1"}})
	if len(h.m.msgs) != 1 || strings.Contains(h.frame(), "hello again") {
		t.Fatalf("deleted message still shown")
	}
}

func TestSelectedMessageActions(t *testing.T) {
	be := threeChats()
	jid := "1@s.whatsapp.net"
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)

	// Up from the box selects the newest message (mine).
	h.key(tea.KeyUp)
	if h.m.focus != focusMessages || h.m.selID != "a2" {
		t.Fatalf("focus=%d sel=%q", h.m.focus, h.m.selID)
	}

	// e + 2 reacts with the second quick emoji.
	h.typeText("e")
	if h.m.react == nil {
		t.Fatal("reaction picker didn't open")
	}
	h.frame()
	h.typeText("2")
	if len(be.reacted) != 1 || be.reacted[0] != "a2="+quickReactions[1] {
		t.Fatalf("reacted = %v", be.reacted)
	}

	// E edits my message: the box is loaded with its text, Enter saves.
	h.m.msgs[1].view.Timestamp = nowMillis()
	h.typeText("E")
	if h.m.editing == nil || h.m.ed.Value() != "hi Alice" || h.m.focus != focusInput {
		t.Fatalf("edit: editing=%v value=%q", h.m.editing != nil, h.m.ed.Value())
	}
	h.typeText("!")
	h.key(tea.KeyEnter)
	if len(be.edited) != 1 || be.edited[0] != "a2=hi Alice!" || len(be.sent) != 0 {
		t.Fatalf("edited=%v sent=%v", be.edited, be.sent)
	}

	// r on Alice's message replies to it.
	h.key(tea.KeyUp)
	h.key(tea.KeyUp)
	if h.m.selID != "a1" {
		t.Fatalf("sel=%q", h.m.selID)
	}
	h.typeText("r")
	if h.m.replyTo == nil || h.m.focus != focusInput {
		t.Fatal("reply didn't start")
	}
	if !strings.Contains(h.frame(), "Reply to Alice") {
		t.Fatal("reply bar not shown")
	}
	h.typeText("sure")
	h.key(tea.KeyEnter)
	if len(be.sent) != 1 || be.sent[0].out.ReplyTo == nil || be.sent[0].out.ReplyTo.ID != "a1" || be.sent[0].out.Text != "sure" {
		t.Fatalf("sent = %+v", be.sent)
	}
	if h.m.replyTo != nil {
		t.Fatal("reply not cleared after sending")
	}

	// d on my message asks; e deletes for everyone.
	h.key(tea.KeyUp)
	h.typeText("d")
	if h.m.confirm == nil {
		t.Fatal("no delete confirmation")
	}
	// An answer that arrives at once (a key already on its way) cancels.
	h.typeText("e")
	if len(be.revoked) != 0 || h.m.confirm != nil {
		t.Fatalf("an instant key answered the question: revoked=%v", be.revoked)
	}
	h.typeText("d")
	h.m.confirm.opened = time.Now().Add(-time.Second)
	h.typeText("e")
	if len(be.revoked) != 1 || be.revoked[0] != "a2" || h.m.confirm != nil {
		t.Fatalf("revoked = %v", be.revoked)
	}

	_ = jid
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func TestForwardPicker(t *testing.T) {
	be := threeChats()
	be.msgs["1@s.whatsapp.net"][0].RawProto = "AAAA"
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)
	h.key(tea.KeyUp)
	h.key(tea.KeyUp)
	h.typeText("f")
	if h.m.overlay == nil {
		t.Fatal("forward picker didn't open")
	}
	h.typeText("bob")
	h.key(tea.KeyEnter)
	if len(be.forwards) != 1 || be.forwards[0] != "a1->3@s.whatsapp.net" {
		t.Fatalf("forwards = %v", be.forwards)
	}
}

func TestOlderPagesLoadOnScroll(t *testing.T) {
	be := newFake()
	jid := "1@s.whatsapp.net"
	be.chats = []whatsapp.ChatView{chat(jid, "Alice", 1)}
	for i := 0; i < pageSize*2+10; i++ {
		be.msgs[jid] = append(be.msgs[jid], text(jid, fmt.Sprintf("m%d", i), "Alice", fmt.Sprintf("message %d", i), false))
	}
	h := newHarness(t, be, 100, 24)
	h.key(tea.KeyEnter)
	if len(h.m.msgs) != pageSize || !h.m.haveOlder {
		t.Fatalf("first page: %d messages, haveOlder=%v", len(h.m.msgs), h.m.haveOlder)
	}
	for i := 0; i < 200 && len(h.m.msgs) < pageSize*2+10; i++ {
		h.key(tea.KeyPgUp)
	}
	if len(h.m.msgs) != pageSize*2+10 {
		t.Fatalf("loaded %d of %d messages", len(h.m.msgs), pageSize*2+10)
	}
	for i := range h.m.msgs {
		if want := fmt.Sprintf("m%d", i); h.m.msgs[i].view.ID != want {
			t.Fatalf("message %d is %q, want %q", i, h.m.msgs[i].view.ID, want)
		}
	}
	// At the very top with nothing older stored, the phone is asked once.
	h.key(tea.KeyPgUp)
	h.key(tea.KeyPgUp)
	if be.older != 1 {
		t.Fatalf("asked the phone %d times, want 1", be.older)
	}
}

func TestDraftsArePerChat(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)
	h.typeText("unfinished")
	h.key(tea.KeyEsc) // back to the list
	h.key(tea.KeyDown)
	if !h.m.ed.Empty() {
		t.Fatalf("draft leaked into another chat: %q", h.m.ed.Value())
	}
	if be.drafts["1@s.whatsapp.net"] != "unfinished" {
		t.Fatalf("draft not saved: %v", be.drafts)
	}
	if !strings.Contains(h.frame(), "Draft: unfinished") {
		t.Fatal("chat list doesn't show the draft")
	}
	h.key(tea.KeyUp)
	if h.m.ed.Value() != "unfinished" {
		t.Fatalf("draft not restored: %q", h.m.ed.Value())
	}
}

func TestPastedFilePathBecomesAttachment(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)

	path := filepath.Join(t.TempDir(), "holiday photo.jpg")
	if err := os.WriteFile(path, []byte("not really a jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.paste(`"` + path + `"`) // as Windows pastes a dropped file
	if h.m.attach == nil || h.m.attach.path != path || !h.m.ed.Empty() {
		t.Fatalf("attach=%+v box=%q", h.m.attach, h.m.ed.Value())
	}
	if !strings.Contains(h.frame(), "holiday photo.jpg") {
		t.Fatal("attachment bar not shown")
	}
	h.typeText("look")
	h.key(tea.KeyEnter)
	if len(be.sent) != 1 || be.sent[0].out.FilePath != path || be.sent[0].out.Text != "look" || be.sent[0].out.AsDocument {
		t.Fatalf("sent = %+v", be.sent)
	}
	if h.m.attach != nil {
		t.Fatal("attachment not cleared")
	}
}

func TestEmojiCompletion(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)
	h.typeText("nice :thumbsu")
	if !h.m.sug.active() || h.m.sug.kind != sugEmoji {
		t.Fatal("emoji dropdown didn't open")
	}
	h.frame()
	h.key(tea.KeyTab)
	if got := h.m.ed.Value(); got != "nice 👍" {
		t.Fatalf("box = %q", got)
	}
	// A time is not an emoji name.
	h.typeText(" at 10:30")
	if h.m.sug.active() {
		t.Fatal("dropdown opened for a time")
	}
}

func TestTypingIndicatorSentOnce(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.key(tea.KeyEnter)
	h.typeText("hello")
	if len(be.typing) != 1 || !be.typing[0] {
		t.Fatalf("typing calls = %v, want one 'true'", be.typing)
	}
	h.key(tea.KeyEsc) // leaving the box clears it
	if len(be.typing) != 2 || be.typing[1] {
		t.Fatalf("typing calls = %v, want [true false]", be.typing)
	}
}

func TestHostileTextCannotReachTheTerminal(t *testing.T) {
	const evil = "\x1b]0;pwned\x07\x1b[2J\x1b[H\x9b31m\r\x08"
	be := newFake()
	jid := "1@g.us"
	be.chats = []whatsapp.ChatView{chat(jid, "Group"+evil, 1)}
	be.chats[0].LastMessagePreview = "preview" + evil
	be.chats[0].Draft = "draft" + evil
	be.members[jid] = []whatsapp.Member{{JID: "9@lid", Name: "Member" + evil}}

	v := text(jid, "x", "Sender"+evil, "body"+evil, false)
	v.SenderJID = "9@lid"
	v.Quote = &whatsapp.Quote{ID: "q", Sender: "Quoted" + evil, Text: "quote" + evil}
	v.Reactions = []whatsapp.ReactionView{{Emoji: "👍" + evil, Count: 2, Names: []string{"R" + evil}}}
	doc := text(jid, "d", "Sender"+evil, "caption"+evil, false)
	doc.Kind, doc.Media = whatsapp.KindDocument, &whatsapp.MediaInfo{FileName: "file" + evil + ".pdf", Size: 10}
	poll := text(jid, "p", "Sender"+evil, "", false)
	poll.Kind = whatsapp.KindPoll
	poll.Poll = &whatsapp.PollView{Question: "Q" + evil, Options: []whatsapp.PollOption{{Name: "opt" + evil, Votes: 1}}}
	sys := text(jid, "s", "", "system"+evil, false)
	sys.Kind = whatsapp.KindSystem
	be.msgs[jid] = []whatsapp.MessageView{v, doc, poll, sys}

	h := newHarness(t, be, 110, 40)
	check := func(where string) {
		t.Helper()
		view := h.m.View()
		// The frame's own colour codes are fine; anything from evil is not.
		for _, bad := range []string{"\x1b]0;", "\x07", "\x1b[2J", "\x1b[H", "\x9b", "\r", "\x08"} {
			if strings.Contains(view, bad) {
				t.Fatalf("%s: the frame contains %q, taken from remote text", where, bad)
			}
		}
	}
	check("chat list")
	h.key(tea.KeyEnter)
	check("open chat")
	h.send(backendEventMsg{whatsapp.TypingEvent{ChatJID: jid, SenderJID: "9@lid", SenderName: "Typer" + evil, State: whatsapp.TypingText}})
	check("typing indicator")
	h.typeText("@Mem")
	check("mention dropdown")
	h.key(tea.KeyEsc)
	h.m.ed.Reset()
	h.key(tea.KeyUp)
	h.key(tea.KeyUp)
	h.key(tea.KeyUp)
	h.key(tea.KeyUp)
	h.typeText("r")
	check("reply bar")
	h.key(tea.KeyEsc)
	h.key(tea.KeyCtrlF)
	h.typeText("body")
	check("search results")
	h.key(tea.KeyEsc)
	h.key(tea.KeyCtrlG)
	check("group info")
	h.key(tea.KeyEsc)
	h.send(actionDoneMsg{err: errors.New("failed" + evil)})
	check("status line")
}

// Where a paste arrives as a burst of separate key presses (no bracketed
// paste, no console reader), its line breaks must not send and its
// letters must not run commands.
func TestBurstOfKeysIsTreatedAsPaste(t *testing.T) {
	be := threeChats()
	h := newHarness(t, be, 100, 30)
	h.m.burstGuard = true
	h.m.lastKeyAt = time.Now()
	h.key(tea.KeyEnter) // right after another key: part of a burst, ignored
	if h.m.focus != focusChatList {
		t.Fatal("an Enter inside a burst opened a chat")
	}
	h.m.lastKeyAt = time.Time{}
	h.key(tea.KeyEnter)
	if h.m.focus != focusInput {
		t.Fatal("a deliberate Enter should open the chat")
	}

	burst := func(msgs ...tea.KeyMsg) {
		for _, k := range msgs {
			h.m.lastKeyAt = time.Now() // as if the previous key had just arrived
			h.send(k)
		}
	}
	runes := func(s string) (out []tea.KeyMsg) {
		for _, r := range s {
			out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		return out
	}
	burst(runes("line one")...)
	burst(tea.KeyMsg{Type: tea.KeyEnter})
	burst(runes("line two")...)
	if len(be.sent) != 0 || h.m.ed.Value() != "line one\nline two" {
		t.Fatalf("burst: sent=%d box=%q", len(be.sent), h.m.ed.Value())
	}
	h.m.lastKeyAt = time.Time{} // a pause, then a deliberate Enter
	h.key(tea.KeyEnter)
	if len(be.sent) != 1 || be.sent[0].out.Text != "line one\nline two" {
		t.Fatalf("sent = %+v", be.sent)
	}

	// With a message selected, a burst of letters does nothing.
	h.m.lastKeyAt = time.Time{}
	h.key(tea.KeyUp)
	if h.m.focus != focusMessages {
		t.Fatal("no message selected")
	}
	burst(runes("delete everything for ever")...)
	if h.m.confirm != nil || h.m.react != nil || h.m.overlay != nil || len(be.revoked)+len(be.deleted)+len(be.reacted) != 0 {
		t.Fatal("pasted letters ran commands on the selected message")
	}
}
