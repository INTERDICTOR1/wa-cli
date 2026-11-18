package tui

import (
	"context"
	"os"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// This file holds the small temporary UIs that take over the keyboard:
// yes/no questions, one-line prompts, the reaction picker, and the
// overlays drawn in place of the messages (help, search, forward, info).

// --- a question answered with one key ---

type confirmPrompt struct {
	text   string
	keys   map[string]func() tea.Cmd
	opened time.Time
}

// confirmDelay is how long a question must have been on screen before an
// answer counts: long enough to rule out a key that was already on its
// way (a paste, a held key), far shorter than reading the question takes.
const confirmDelay = 350 * time.Millisecond

func (c *confirmPrompt) key(m *model, msg tea.KeyMsg) tea.Cmd {
	m.confirm = nil
	if msg.Paste || time.Since(c.opened) < confirmDelay {
		return nil
	}
	if act, ok := c.keys[strings.ToLower(keyText(msg))]; ok {
		return act()
	}
	return nil
}

// --- a one-line text prompt ---

type linePrompt struct {
	label  string
	ed     editor
	submit func(m *model, text string) tea.Cmd
}

func (p *linePrompt) key(m *model, msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.prompt = nil
	case tea.KeyEnter:
		m.prompt = nil
		return p.submit(m, strings.TrimSpace(p.ed.Value()))
	case tea.KeyBackspace:
		p.ed.Backspace()
	case tea.KeyDelete:
		p.ed.Delete()
	case tea.KeyLeft:
		p.ed.Left()
	case tea.KeyRight:
		p.ed.Right()
	case tea.KeyHome:
		p.ed.Start()
	case tea.KeyEnd:
		p.ed.End()
	case tea.KeyCtrlU:
		p.ed.Reset()
	default:
		if text := keyText(msg); text != "" {
			p.ed.Insert(oneLine(text))
		}
	}
	return nil
}

func (m *model) openAttachPrompt() tea.Cmd {
	if m.openJID == "" {
		return nil
	}
	ed := newEditor()
	ed.maxRows = 1
	ed.SetWidth(max(10, m.calc().paneW-22))
	m.prompt = &linePrompt{
		label: "File to send:",
		ed:    ed,
		submit: func(m *model, text string) tea.Cmd {
			if text == "" {
				return nil
			}
			path, ok := asFilePath(text)
			if !ok {
				return m.setStatus("No such file: "+text, true)
			}
			return m.setAttachment(path)
		},
	}
	return m.info("Type or paste a file's full path (or just drop the file on the terminal).")
}

// setAttachment queues a file to go out with the next Enter.
func (m *model) setAttachment(path string) tea.Cmd {
	info, err := os.Stat(path)
	if err != nil {
		return m.fail(err)
	}
	m.attach = &attachment{path: path, size: info.Size()}
	m.focus = focusInput
	return nil
}

// fileKind says how a file will be sent, from its extension.
func fileKind(path string) string {
	ext := ""
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		ext = strings.ToLower(path[i+1:])
	}
	switch ext {
	case "jpg", "jpeg", "png", "webp", "bmp", "gif":
		return "a photo (full quality)"
	case "mp4", "mov", "m4v", "webm", "mkv", "3gp", "avi":
		return "a video"
	case "ogg", "opus":
		return "a voice message"
	case "mp3", "m4a", "aac", "wav", "flac", "amr":
		return "audio"
	}
	return "a document"
}

// --- reaction picker ---

type reactPicker struct {
	target whatsapp.MessageView
	idx    int
}

func newReactPicker(v whatsapp.MessageView) *reactPicker { return &reactPicker{target: v} }

func (r *reactPicker) send(m *model, emoji string) tea.Cmd {
	m.react = nil
	ctx, be, target := m.ctx, m.be, r.target.Message
	return do(func() error { return be.React(ctx, target, emoji) })
}

func (r *reactPicker) key(m *model, msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.react = nil
	case tea.KeyLeft:
		r.idx = (r.idx + len(quickReactions) - 1) % len(quickReactions)
	case tea.KeyRight, tea.KeyTab:
		r.idx = (r.idx + 1) % len(quickReactions)
	case tea.KeyEnter:
		return r.send(m, quickReactions[r.idx])
	case tea.KeyBackspace, tea.KeyDelete:
		return r.send(m, "")
	default:
		text := strings.TrimSpace(keyText(msg))
		if text == "" {
			return nil
		}
		if len(text) == 1 && text[0] >= '1' && int(text[0]-'1') < len(quickReactions) {
			return r.send(m, quickReactions[text[0]-'1'])
		}
		// Anything that isn't plain letters is taken as the emoji itself
		// (typed with the emoji picker, or pasted).
		for _, c := range text {
			if c < unicode.MaxASCII {
				return nil
			}
		}
		return r.send(m, text)
	}
	return nil
}

func (r *reactPicker) view(w int) string {
	var b strings.Builder
	b.WriteString(" " + headerStyle.Render("React") + " ")
	for i, e := range quickReactions {
		if i == r.idx {
			b.WriteString(selectedStyle.Render(" " + e + " "))
		} else {
			b.WriteString(" " + e + " ")
		}
	}
	return ansi.Truncate(b.String(), w, "")
}

// --- overlays ---

type overlayKind int

const (
	overlayHelp overlayKind = iota
	overlaySearch
	overlayForward
	overlayInfo
)

type overlayItem struct {
	label  string // main text
	detail string // dim text after it
	jid    string // chat this row is about
	id     string // message ID (search results)
	ts     int64
}

// overlay is a list drawn in place of the messages. With a query line it
// filters as you type (search, forward); without it just scrolls (help,
// info).
type overlay struct {
	kind    overlayKind
	title   string
	query   string
	typing  bool // has a query line
	items   []overlayItem
	sel     int
	top     int
	scope   string // search: the chat searched, "" = everywhere
	seq     int    // search: drops results for an older query
	note    string // shown when there are no items
	forward whatsapp.MessageView
}

func (o *overlay) help() string {
	switch o.kind {
	case overlaySearch:
		return "type to search · ↑/↓ move · enter go to message · esc close"
	case overlayForward:
		return "type to filter · ↑/↓ move · enter forward · esc cancel"
	}
	return "↑/↓ scroll · esc close"
}

func (o *overlay) key(m *model, msg tea.KeyMsg) tea.Cmd {
	page := max(1, m.calc().msgH-4)
	switch msg.Type {
	case tea.KeyEsc:
		m.overlay = nil
		return nil
	case tea.KeyUp:
		o.sel--
	case tea.KeyDown:
		o.sel++
	case tea.KeyPgUp:
		o.sel -= page
	case tea.KeyPgDown:
		o.sel += page
	case tea.KeyHome:
		o.sel = 0
	case tea.KeyEnd:
		o.sel = len(o.items) - 1
	case tea.KeyEnter:
		return o.choose(m)
	case tea.KeyBackspace:
		if r := []rune(o.query); o.typing && len(r) > 0 {
			return o.setQuery(m, string(r[:len(r)-1]))
		}
	default:
		text := keyText(msg)
		switch {
		case text == "":
		case o.typing:
			return o.setQuery(m, o.query+oneLine(text))
		case text == "q" || text == "?":
			m.overlay = nil
		}
	}
	o.sel = max(0, min(o.sel, len(o.items)-1))
	return nil
}

func (o *overlay) setQuery(m *model, q string) tea.Cmd {
	o.query = q
	o.sel, o.top = 0, 0
	switch o.kind {
	case overlayForward:
		o.items = forwardTargets(m, q)
	case overlaySearch:
		o.seq++
		if len([]rune(strings.TrimSpace(q))) < 2 {
			o.items, o.note = nil, "Type at least two letters."
			return nil
		}
		o.note = "Searching…"
		return searchCmd(m.ctx, m.be, o.scope, strings.TrimSpace(q), o.seq)
	}
	return nil
}

func (o *overlay) choose(m *model) tea.Cmd {
	if o.sel < 0 || o.sel >= len(o.items) {
		return nil
	}
	it := o.items[o.sel]
	switch o.kind {
	case overlayForward:
		m.overlay = nil
		ctx, be, target := m.ctx, m.be, o.forward.Message
		return tea.Batch(
			m.info("Forwarding to "+it.label+"…"),
			do(func() error { return be.Forward(ctx, it.jid, target) }),
		)
	case overlaySearch:
		m.overlay = nil
		return m.jumpTo(it.jid, it.id)
	}
	return nil
}

// view returns exactly h lines.
func (o *overlay) view(m *model, w, h int) []string {
	out := make([]string, 0, h)
	head := " " + headerStyle.Render(oneLine(o.title))
	if o.typing {
		head += "  " + oneLine(o.query) + cursorStyle.Render(" ")
	}
	out = append(out, ansi.Truncate(head, w, ""), "")

	rows := max(1, h-2)
	if o.kind == overlayHelp || o.kind == overlayInfo {
		// Nothing to select: the arrows simply scroll.
		o.sel = max(0, min(o.sel, len(o.items)-rows))
		o.top = o.sel
	}
	if o.sel < o.top {
		o.top = o.sel
	}
	if o.sel >= o.top+rows {
		o.top = o.sel - rows + 1
	}
	o.top = max(0, min(o.top, len(o.items)-rows))

	if len(o.items) == 0 && o.note != "" {
		out = append(out, "   "+dimStyle.Render(o.note))
	}
	for i := o.top; i < len(o.items) && len(out) < h; i++ {
		it := o.items[i]
		selectable := o.kind == overlaySearch || o.kind == overlayForward
		// Rows hold names and message text from the network: never draw
		// them unsanitized, whatever built the row.
		label := truncate(oneLine(it.label), w-6)
		detail := ""
		if room := w - 8 - ansi.StringWidth(label); it.detail != "" && room > 4 {
			detail = "  " + truncate(oneLine(it.detail), room)
		}
		switch {
		case selectable && i == o.sel:
			out = append(out, " "+selectedStyle.Render(" › "+label+" ")+dimStyle.Render(detail))
		case it.jid == "" && it.id == "" && it.detail == "" && strings.HasSuffix(it.label, ":"):
			out = append(out, " "+headerStyle.Render(label)) // a section title
		default:
			out = append(out, "    "+chatNameStyle.Render(label)+dimStyle.Render(detail))
		}
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}

// --- help ---

func (m *model) openHelp() tea.Cmd {
	lines := []string{
		"Chat list:",
		"type            search chats and contacts (a contact starts a new chat)",
		"↑ ↓ PgUp PgDn   move; the highlighted chat is previewed, not marked read",
		"Enter / Tab     open the chat and start typing",
		"Ctrl+P          pin or unpin",
		"Ctrl+A          archive or unarchive",
		"Ctrl+T          mute or unmute",
		"Ctrl+U          mark unread / read",
		"Ctrl+R          show or hide archived chats",
		"Ctrl+F          search every chat's messages",
		"",
		"Writing:",
		"Enter           send",
		"Shift+Enter     new line (also Ctrl+Enter, Alt+Enter, Ctrl+J)",
		"paste           multi-line text stays one message",
		"@name           mention someone in a group",
		":name           pick an emoji, e.g. :thumbsup",
		"Ctrl+O          attach a file by path - or just drop a file on the terminal",
		"Alt+V           attach the picture on the clipboard (a screenshot)",
		"Ctrl+D          send the attached file as a document instead",
		"↑               select the last message (then act on it)",
		"PgUp PgDn       scroll the chat",
		"Ctrl+F          search this chat",
		"Ctrl+G          chat / group info",
		"Esc / Tab       back to the chat list",
		"",
		"A selected message:",
		"Enter           view photo full screen / open media or link / retry / reply",
		"r               reply",
		"e               react (then 1-6, arrows, or any emoji)",
		"E               edit (your own, within 15 minutes)",
		"d               delete (for everyone, or just here)",
		"f               forward",
		"c               copy the text",
		"o               open with the default app",
		"s               save to Downloads/WhatsApp",
		"Esc             back to writing",
		"",
		"Photo viewer:",
		"m               switch drawing mode (sixel / kitty / iterm / blocks)",
		"o               open in the system's image viewer (true full resolution)",
		"s               save",
		"any other key   close",
		"",
		"Ctrl+C quits. Settings: wa config (sendReadReceipts, sendPresence, mouse, imageProtocol).",
	}
	o := &overlay{kind: overlayHelp, title: "Keys"}
	for _, l := range lines {
		key, rest, ok := strings.Cut(l, "  ")
		if !ok {
			o.items = append(o.items, overlayItem{label: l})
			continue
		}
		o.items = append(o.items, overlayItem{label: padRight(key, 15), detail: strings.TrimSpace(rest)})
	}
	m.overlay = o
	return nil
}

// --- forward ---

func forwardTargets(m *model, q string) []overlayItem {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []overlayItem
	for _, c := range m.chats {
		if q == "" || strings.Contains(c.lower, q) {
			out = append(out, overlayItem{label: c.Title, jid: c.JID})
			if len(out) == 200 {
				break
			}
		}
	}
	return out
}

func (m *model) openForward(v whatsapp.MessageView) tea.Cmd {
	m.overlay = &overlay{kind: overlayForward, title: "Forward to:", typing: true, forward: v, note: "No matching chat."}
	m.overlay.items = forwardTargets(m, "")
	return nil
}

// --- search ---

type searchResultsMsg struct {
	seq   int
	items []overlayItem
	err   error
}

func searchCmd(ctx context.Context, be Backend, chatJID, query string, seq int) tea.Cmd {
	return func() tea.Msg {
		found, err := be.Search(ctx, chatJID, query, 100)
		if err != nil {
			return searchResultsMsg{seq: seq, err: err}
		}
		now := time.Now()
		items := make([]overlayItem, 0, len(found))
		for _, v := range found {
			who := oneLine(v.Sender)
			if v.FromMe {
				who = "You"
			}
			items = append(items, overlayItem{
				label:  oneLine(v.Body),
				detail: strings.TrimSpace(who + " · " + listTime(v.Timestamp, now)),
				jid:    v.ChatJID,
				id:     v.ID,
				ts:     v.Timestamp,
			})
		}
		return searchResultsMsg{seq: seq, items: items}
	}
}

func (o *overlay) searchResults(msg searchResultsMsg) {
	if o.kind != overlaySearch || msg.seq != o.seq {
		return
	}
	o.items, o.sel, o.top = msg.items, 0, 0
	o.note = "Nothing found."
	if msg.err != nil {
		o.note = msg.err.Error()
	}
}

func (m *model) openSearch(scope string) tea.Cmd {
	title := "Search all chats:"
	if scope != "" {
		title = "Search " + m.open.Title + ":"
	}
	m.overlay = &overlay{kind: overlaySearch, title: title, typing: true, scope: scope, note: "Type at least two letters."}
	return nil
}

// jumpTo shows a specific message, switching chats and loading older
// pages as needed.
func (m *model) jumpTo(chatJID, id string) tea.Cmd {
	var cmds []tea.Cmd
	if chatJID != m.openJID {
		item, ok := chatItem{}, false
		if i, has := m.chatIdx[chatJID]; has {
			item, ok = m.chats[i], true
		}
		if !ok {
			return m.info("That chat is no longer in the list.")
		}
		m.selectedJID = chatJID
		if m.filter != "" {
			m.filter = ""
			m.refilter()
		}
		m.keepSelectionVisible()
		cmds = append(cmds, m.showChat(item))
	}
	m.focus = focusMessages
	m.jump = &jumpTarget{id: id, pages: 40}
	cmds = append(cmds, m.continueJump())
	return tea.Batch(cmds...)
}

// jumpTarget is a message being looked for while older pages load.
type jumpTarget struct {
	id    string
	pages int // how many more pages to try
}

// continueJump is called whenever messages have loaded: select the target
// if it's there, otherwise load one more page.
func (m *model) continueJump() tea.Cmd {
	if m.jump == nil {
		return nil
	}
	if _, ok := m.msgIdx[m.jump.id]; ok {
		m.selID = m.jump.id
		m.jump = nil
		m.revealSelected()
		return m.wantPreviews()
	}
	if len(m.msgs) == 0 {
		return nil // the first page is still loading
	}
	if !m.haveOlder || m.jump.pages <= 0 {
		m.jump = nil
		return m.info("Couldn't find that message in local history.")
	}
	m.jump.pages--
	m.loadingOlder = true
	oldest := m.msgs[0].view.Message
	return loadPageCmd(m.ctx, m.be, m.openJID, m.loadSeq, &oldest, pageSize*2, false)
}

// --- chat info ---

type infoLoadedMsg struct {
	jid     string
	members []whatsapp.Member
	err     error
}

func (m *model) openInfo() tea.Cmd {
	if m.openJID == "" {
		return nil
	}
	o := &overlay{kind: overlayInfo, title: m.open.Title}
	user, _, _ := strings.Cut(m.openJID, "@")
	if m.open.IsGroup {
		o.items = append(o.items, overlayItem{label: "Group", detail: m.openJID})
		o.note = ""
		o.items = append(o.items, overlayItem{label: "Loading members…"})
		m.overlay = o
		ctx, be, jid := m.ctx, m.be, m.openJID
		return func() tea.Msg {
			members, err := be.Members(ctx, jid)
			return infoLoadedMsg{jid: jid, members: members, err: err}
		}
	}
	o.items = append(o.items, overlayItem{label: "Number", detail: "+" + user})
	if strings.HasSuffix(m.openJID, "@lid") {
		o.items[0] = overlayItem{label: "ID", detail: m.openJID}
	}
	if sub := m.subtitle(); sub != "" {
		o.items = append(o.items, overlayItem{label: "Status", detail: sub})
	}
	flags := []string{}
	if m.open.Pinned {
		flags = append(flags, "pinned")
	}
	if m.open.Muted || m.open.MutedUntil != 0 {
		flags = append(flags, "muted")
	}
	if m.open.Archived {
		flags = append(flags, "archived")
	}
	if m.open.Ephemeral > 0 {
		flags = append(flags, "disappearing messages on")
	}
	if len(flags) > 0 {
		o.items = append(o.items, overlayItem{label: "Chat", detail: strings.Join(flags, ", ")})
	}
	m.overlay = o
	return nil
}

func (o *overlay) infoLoaded(m *model, msg infoLoadedMsg) {
	if o.kind != overlayInfo || msg.jid != m.openJID {
		return
	}
	o.items = o.items[:1]
	if msg.err != nil {
		o.items = append(o.items, overlayItem{label: "Couldn't load members", detail: msg.err.Error()})
		return
	}
	o.items = append(o.items, overlayItem{label: "", detail: ""}, overlayItem{label: itoa(len(msg.members)+1) + " members:"})
	for _, mem := range msg.members {
		detail := ""
		if mem.IsAdmin {
			detail = "admin"
		}
		o.items = append(o.items, overlayItem{label: oneLine(mem.Name), detail: detail})
	}
}
