package tui

import (
	"context"
	"errors"
	"image"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/termimg"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

func init() {
	// Tests don't run in a terminal; force colours on so the rendering
	// paths that add them are exercised.
	lipgloss.SetColorProfile(termenv.ANSI256)
}

// fakeBackend is an in-memory Backend that records what the TUI asks for.
type fakeBackend struct {
	mu      sync.Mutex
	chats   []whatsapp.ChatView
	msgs    map[string][]whatsapp.MessageView
	members map[string][]whatsapp.Member

	sent     []sentMsg
	read     []string
	reacted  []string
	edited   []string
	revoked  []string
	deleted  []string
	forwards []string
	typing   []bool
	drafts   map[string]string
	pinned   map[string]bool
	older    int
}

type sentMsg struct {
	chat string
	out  whatsapp.Outgoing
}

func newFake() *fakeBackend {
	return &fakeBackend{
		msgs:    map[string][]whatsapp.MessageView{},
		members: map[string][]whatsapp.Member{},
		drafts:  map[string]string{},
		pinned:  map[string]bool{},
	}
}

func chat(jid, title string, at int64) whatsapp.ChatView {
	return whatsapp.ChatView{
		Chat:  chatstore.Chat{JID: jid, Name: title, IsGroup: strings.HasSuffix(jid, "@g.us"), LastMessageAt: at},
		Title: title,
	}
}

var seq int64

func text(chatJID, id, sender, body string, fromMe bool) whatsapp.MessageView {
	seq++
	return whatsapp.MessageView{
		Message: msgstore.Message{
			ID: id, ChatJID: chatJID, SenderJID: sender, Text: body, FromMe: fromMe,
			Timestamp: time.Date(2026, 11, 17, 10, 0, 0, 0, time.Local).UnixMilli() + seq*1000, Seq: seq,
		},
		Kind: whatsapp.KindText, Sender: sender, Body: body,
	}
}

func (f *fakeBackend) Chats(context.Context) ([]whatsapp.ChatView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]whatsapp.ChatView(nil), f.chats...), nil
}

func (f *fakeBackend) Chat(_ context.Context, jid string) (whatsapp.ChatView, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.chats {
		if c.JID == jid {
			return c, true, nil
		}
	}
	return whatsapp.ChatView{}, false, nil
}

func (f *fakeBackend) Messages(_ context.Context, chatJID string, before *msgstore.Message, limit int) ([]whatsapp.MessageView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.msgs[chatJID]
	end := len(all)
	if before != nil {
		for end > 0 && !isOlder(&all[end-1].Message, before) {
			end--
		}
	}
	start := max(0, end-limit)
	return append([]whatsapp.MessageView(nil), all[start:end]...), nil
}

func (f *fakeBackend) Message(_ context.Context, chatJID, id string) (whatsapp.MessageView, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.msgs[chatJID] {
		if v.ID == id {
			return v, true, nil
		}
	}
	return whatsapp.MessageView{}, false, nil
}

func (f *fakeBackend) Search(_ context.Context, chatJID, query string, limit int) ([]whatsapp.MessageView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []whatsapp.MessageView
	for jid, msgs := range f.msgs {
		if chatJID != "" && jid != chatJID {
			continue
		}
		for _, v := range msgs {
			if strings.Contains(strings.ToLower(v.Body), strings.ToLower(query)) && len(out) < limit {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func (f *fakeBackend) Contacts(context.Context) ([]whatsapp.Contact, error) {
	return []whatsapp.Contact{{JID: "999@s.whatsapp.net", Name: "Zed Newperson"}}, nil
}

func (f *fakeBackend) Members(_ context.Context, groupJID string) ([]whatsapp.Member, error) {
	return f.members[groupJID], nil
}

func (f *fakeBackend) Send(_ context.Context, chatJID string, out whatsapp.Outgoing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatJID, out})
	return nil
}

func (f *fakeBackend) Retry(context.Context, msgstore.Message) error { return nil }

func (f *fakeBackend) React(_ context.Context, msg msgstore.Message, emoji string) error {
	f.reacted = append(f.reacted, msg.ID+"="+emoji)
	return nil
}

func (f *fakeBackend) Edit(_ context.Context, msg msgstore.Message, text string) error {
	f.edited = append(f.edited, msg.ID+"="+text)
	return nil
}

func (f *fakeBackend) Revoke(_ context.Context, msg msgstore.Message) error {
	f.revoked = append(f.revoked, msg.ID)
	return nil
}

func (f *fakeBackend) DeleteLocal(msg msgstore.Message) error {
	f.deleted = append(f.deleted, msg.ID)
	return nil
}

func (f *fakeBackend) Forward(_ context.Context, to string, msg msgstore.Message) error {
	f.forwards = append(f.forwards, msg.ID+"->"+to)
	return nil
}

func (f *fakeBackend) MarkChatRead(_ context.Context, chatJID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = append(f.read, chatJID)
	for i := range f.chats {
		if f.chats[i].JID == chatJID {
			f.chats[i].UnreadCount = 0
		}
	}
	return nil
}

func (f *fakeBackend) MarkChatUnread(context.Context, string) error { return nil }

func (f *fakeBackend) SetPinned(_ context.Context, jid string, on bool) error {
	f.pinned[jid] = on
	return nil
}
func (f *fakeBackend) SetArchived(context.Context, string, bool) error { return nil }
func (f *fakeBackend) SetMuted(context.Context, string, bool) error    { return nil }

func (f *fakeBackend) SetDraft(jid, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drafts[jid] = text
	return nil
}

func (f *fakeBackend) LoadOlder(context.Context, string) (bool, error) {
	f.older++
	return true, nil
}

func (f *fakeBackend) SetOnline(context.Context, bool) error { return nil }

func (f *fakeBackend) SetTyping(_ context.Context, _ string, typing bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typing = append(f.typing, typing)
	return nil
}

func (f *fakeBackend) WatchPresence(context.Context, string) error { return nil }

func (f *fakeBackend) Thumbnail(context.Context, msgstore.Message) (image.Image, error) {
	return nil, errors.New("no thumbnail in tests")
}

func (f *fakeBackend) SaveMedia(context.Context, msgstore.Message, string) (string, error) {
	return "", errors.New("no media in tests")
}

// harness drives a model the way Bubble Tea would: every command a
// message produces is run and its result fed back in.
type harness struct {
	t    *testing.T
	m    *model
	be   *fakeBackend
	wait time.Duration // how long a command may take; 0 = 40ms
}

func newHarness(t *testing.T, be *fakeBackend, w, h int) *harness {
	t.Helper()
	m := newModel(context.Background(), be, Options{ImageProtocol: termimg.Blocks, MediaDir: t.TempDir(), DownloadDir: t.TempDir()}, nil)
	hn := &harness{t: t, m: m, be: be}
	hn.exec(m.Init())
	hn.send(tea.WindowSizeMsg{Width: w, Height: h})
	return hn
}

func (h *harness) send(msg tea.Msg) {
	_, cmd := h.m.Update(msg)
	h.exec(cmd)
}

var cmdType = reflect.TypeOf(tea.Cmd(nil))

func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		if msg == nil {
			return
		}
		// tea.Batch and tea.Sequence produce a slice of commands.
		if rv := reflect.ValueOf(msg); rv.Kind() == reflect.Slice && rv.Type().Elem() == cmdType {
			for i := 0; i < rv.Len(); i++ {
				if c, ok := rv.Index(i).Interface().(tea.Cmd); ok {
					h.exec(c)
				}
			}
			return
		}
		h.send(msg)
	case <-time.After(max(h.wait, 40*time.Millisecond)):
		// A timer (status line, typing indicator): not needed in tests.
	}
}

func (h *harness) key(t tea.KeyType) { h.send(tea.KeyMsg{Type: t}) }
func (h *harness) typeText(s string) {
	for _, r := range s {
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}
func (h *harness) paste(s string) {
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true})
}

// frame returns the drawn frame and fails if it doesn't fit the terminal.
func (h *harness) frame() string {
	h.t.Helper()
	view := h.m.View()
	lines := strings.Split(view, "\n")
	if len(lines) != h.m.height {
		h.t.Errorf("frame is %d lines, terminal is %d", len(lines), h.m.height)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > h.m.width {
			h.t.Errorf("line %d is %d columns wide, terminal is %d: %q", i, w, h.m.width, ansi.Strip(l))
		}
	}
	return ansi.Strip(view)
}
