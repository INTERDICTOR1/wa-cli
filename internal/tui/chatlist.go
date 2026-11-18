package tui

import (
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// chatItem is one row of the chat list.
type chatItem struct {
	whatsapp.ChatView
	lower   string // lower-cased title, for filtering
	contact bool   // an address-book entry with no chat yet
}

func newChatItem(c whatsapp.ChatView) chatItem {
	c.Title = oneLine(c.Title)
	if c.Title == "" {
		c.Title = c.JID
	}
	return chatItem{ChatView: c, lower: strings.ToLower(c.Title)}
}

// chatLess is the list order: pinned chats first, then the most recent.
func chatLess(a, b *chatItem) bool {
	if a.Pinned != b.Pinned {
		return a.Pinned
	}
	if a.LastMessageAt != b.LastMessageAt {
		return a.LastMessageAt > b.LastMessageAt
	}
	return a.lower < b.lower
}

func (m *model) setChats(chats []whatsapp.ChatView) {
	m.chats = make([]chatItem, len(chats))
	for i, c := range chats {
		m.chats[i] = newChatItem(c)
	}
	m.sortChats()
}

func (m *model) sortChats() {
	sort.SliceStable(m.chats, func(i, j int) bool { return chatLess(&m.chats[i], &m.chats[j]) })
	clear(m.chatIdx)
	for i, c := range m.chats {
		m.chatIdx[c.JID] = i
	}
	if i, ok := m.chatIdx[m.openJID]; ok {
		m.open = m.chats[i]
	}
	m.refilter()
}

// updateChat applies one chat's new state (or its removal) to the list.
func (m *model) updateChat(jid string, c whatsapp.ChatView, found bool) {
	i, known := m.chatIdx[jid]
	switch {
	case !found && known:
		m.chats = append(m.chats[:i], m.chats[i+1:]...)
	case !found:
		return
	case known:
		m.chats[i] = newChatItem(c)
	default:
		m.chats = append(m.chats, newChatItem(c))
	}
	m.sortChats()
}

// refilter recomputes which rows the list shows: chats whose name contains
// the filter, then - while filtering - contacts you have no chat with yet,
// so typing a name is also how you start a new chat. Archived chats only
// appear when filtering or when shown on purpose.
func (m *model) refilter() {
	q := strings.ToLower(strings.TrimSpace(m.filter))
	m.visible = m.visible[:0]
	for _, c := range m.chats {
		if q == "" {
			if !c.Archived || m.showArchived {
				m.visible = append(m.visible, c)
			}
		} else if strings.Contains(c.lower, q) {
			m.visible = append(m.visible, c)
		}
	}
	if q != "" {
		added := 0
		for _, ct := range m.contacts {
			if _, has := m.chatIdx[ct.JID]; has || ct.Name == "" || !strings.HasSuffix(ct.JID, "@s.whatsapp.net") {
				continue
			}
			lower := strings.ToLower(ct.Name)
			if !strings.Contains(lower, q) {
				continue
			}
			item := chatItem{lower: lower, contact: true}
			item.JID, item.Title = ct.JID, oneLine(ct.Name)
			m.visible = append(m.visible, item)
			if added++; added == 50 {
				break
			}
		}
	}
	m.keepSelectionVisible()
}

func (m *model) selectedIndex() int {
	for i, c := range m.visible {
		if c.JID == m.selectedJID {
			return i
		}
	}
	return -1
}

// setFilter changes the filter and highlights its first match.
func (m *model) setFilter(f string) tea.Cmd {
	m.filter = f
	m.refilter()
	if len(m.visible) > 0 {
		m.selectedJID = m.visible[0].JID
		m.listTop = 0
	}
	var cmd tea.Cmd
	if f != "" && !m.contactsOK {
		m.contactsOK = true // don't ask twice while it loads
		cmd = loadContactsCmd(m.ctx, m.be)
	}
	return tea.Batch(cmd, m.showSelected())
}

func (m *model) moveSelection(delta int) tea.Cmd {
	if len(m.visible) == 0 {
		return nil
	}
	i := max(0, m.selectedIndex())
	i = max(0, min(i+delta, len(m.visible)-1))
	m.selectedJID = m.visible[i].JID
	m.keepSelectionVisible()
	return m.showSelected()
}

// listRows is how many chats fit in the list: each takes two lines.
func (m *model) listRows() int {
	l := m.calc()
	return max(1, (l.bodyH-2-2)/2) // minus border, header and blank line
}

func (m *model) keepSelectionVisible() {
	rows := m.listRows()
	if i := m.selectedIndex(); i >= 0 {
		if i < m.listTop {
			m.listTop = i
		}
		if i >= m.listTop+rows {
			m.listTop = i - rows + 1
		}
	}
	m.listTop = max(0, min(m.listTop, len(m.visible)-rows))
}

func (m *model) selectedItem() (chatItem, bool) {
	if i := m.selectedIndex(); i >= 0 {
		return m.visible[i], true
	}
	return chatItem{}, false
}

// showSelected previews the highlighted chat in the message pane without
// marking it read. No-op if it is already shown.
func (m *model) showSelected() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok || item.JID == m.openJID {
		return nil
	}
	return m.showChat(item)
}

// showChat switches the message pane to another chat.
func (m *model) showChat(item chatItem) tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, m.stopTyping(), m.stashDraft())

	m.openJID, m.open = item.JID, item
	m.loadSeq++
	m.msgs, m.lines, m.lineMsg, m.msgStart = nil, nil, nil, nil
	clear(m.msgIdx)
	m.haveOlder, m.loadingOlder = false, false
	m.scrollBack, m.selID, m.unreadFrom, m.newBelow = 0, "", "", 0
	m.replyTo, m.editing, m.attach, m.react, m.confirm, m.jump = nil, nil, nil, nil, nil, nil
	m.sug.reset()
	m.previews.setOpen(item.JID)

	draft := m.drafts[item.JID]
	if draft == "" {
		draft = item.Draft
	}
	m.ed.SetValue(draft)

	cmds = append(cmds, loadPageCmd(m.ctx, m.be, item.JID, m.loadSeq, nil, pageSize, false))
	return tea.Batch(cmds...)
}

// stashDraft remembers the message box's text for the chat being left.
func (m *model) stashDraft() tea.Cmd {
	if m.openJID == "" || m.editing != nil {
		return nil
	}
	text := m.ed.Value()
	old, had := m.drafts[m.openJID]
	if !had {
		old = m.open.Draft
	}
	if text == old {
		return nil
	}
	m.drafts[m.openJID] = text
	if i, ok := m.chatIdx[m.openJID]; ok {
		m.chats[i].Draft = text
		m.refilter()
	}
	return saveDraftCmd(m.be, m.openJID, text)
}

// enterChat moves into the highlighted chat to write: the point where it
// counts as opened and read.
func (m *model) enterChat() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok {
		return nil
	}
	var cmds []tea.Cmd
	if item.JID != m.openJID {
		cmds = append(cmds, m.showChat(item))
	}
	if m.filter != "" {
		m.filter = ""
		m.refilter()
	}
	m.focus = focusInput
	cmds = append(cmds, m.markReadIfWatching(), watchPresenceCmd(m.ctx, m.be, item.JID))
	return tea.Batch(cmds...)
}

// --- drawing ---

func (m *model) renderSidebar(l layout) string {
	w := l.sideW - 4 // text width inside border and padding
	rows := m.listRows()
	now := time.Now()

	lines := make([]string, 0, l.bodyH)
	if m.focus == focusChatList {
		search := "Search: " + m.filter
		if over := ansi.StringWidth(search) - (w - 1); over > 0 {
			search = ansi.TruncateLeft(search, over+1, "…") // keep the end you're typing
		}
		lines = append(lines, headerStyle.Render(search)+cursorStyle.Render(" "))
	} else {
		title := "Chats"
		if m.showArchived {
			title = "Chats + archived"
		}
		lines = append(lines, headerStyle.Render(title))
	}
	lines = append(lines, "")

	if len(m.visible) == 0 {
		if m.filter != "" {
			lines = append(lines, dimStyle.Render("No match"))
		} else {
			lines = append(lines, dimStyle.Render("No chats yet"))
		}
	}

	top := max(0, min(m.listTop, len(m.visible)-rows))
	for _, c := range m.visible[top:min(top+rows, len(m.visible))] {
		l1, l2 := m.chatRow(c, w, now)
		lines = append(lines, l1, l2)
	}

	style := borderStyle
	if m.focus == focusChatList {
		style = borderFocusedStyle
	}
	return style.Padding(0, 1).Width(l.sideW - 2).Height(l.bodyH - 2).MaxHeight(l.bodyH).Render(strings.Join(lines, "\n"))
}

// chatRow draws one chat as two lines: name and time, then the last
// message and the unread count.
func (m *model) chatRow(c chatItem, w int, now time.Time) (string, string) {
	selected := c.JID == m.selectedJID
	unread := c.UnreadCount > 0 || c.MarkedUnread

	name := c.Title
	if c.Pinned {
		name = "📌 " + name
	}
	when := listTime(c.LastMessageAt, now)

	preview := oneLine(c.LastMessagePreview)
	switch {
	case c.contact:
		preview = "Start a new chat"
	case c.Draft != "" && c.JID != m.openJID:
		preview = "Draft: " + oneLine(c.Draft)
	case m.typingText(c.JID) != "":
		preview = m.typingText(c.JID)
	}
	badge := ""
	switch {
	case c.UnreadCount > 0:
		badge = " " + itoa(c.UnreadCount) + " "
	case c.MarkedUnread:
		badge = " ● "
	}
	flags := ""
	if c.Muted || c.MutedUntil != 0 {
		flags += "🔇"
	}
	if c.Archived {
		flags += "▾"
	}

	if selected {
		l1 := selectedStyle.Render(padRight(spread("› "+name, when, w), w))
		right := flags
		if badge != "" {
			right += badge
		}
		l2 := selectedDimStyle.Render(padRight(spread("  "+preview, right, w), w))
		return l1, l2
	}

	nameStyle := chatNameStyle
	if unread {
		nameStyle = unreadNameStyle
	}
	whenW := ansi.StringWidth(when)
	l1 := "  " + nameStyle.Render(padRight(truncate(name, w-3-whenW), w-2-whenW)) + dimStyle.Render(when)

	rightW := ansi.StringWidth(flags) + ansi.StringWidth(badge)
	left := dimStyle.Render(padRight(truncate(preview, w-3-rightW), w-2-rightW))
	l2 := "  " + left + dimStyle.Render(flags)
	if badge != "" {
		l2 += unreadBadge.Render(badge)
	}
	return l1, l2
}
