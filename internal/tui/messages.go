package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// msgItem is a loaded message plus its drawn form. Drawing a message
// (wrapping, colours, picture) is the expensive part of a frame, so it is
// done once and kept until the message, the width or its picture changes.
type msgItem struct {
	view whatsapp.MessageView

	block       []string // drawn lines, nil when it needs redrawing
	blockSender bool     // block was drawn with the sender's name on top
}

func isOlder(a, b *msgstore.Message) bool {
	if a.Timestamp != b.Timestamp {
		return a.Timestamp < b.Timestamp
	}
	return a.Seq < b.Seq
}

func (m *model) reindex() {
	clear(m.msgIdx)
	for i := range m.msgs {
		m.msgIdx[m.msgs[i].view.ID] = i
	}
}

// reloadOpen re-reads the messages on screen (names changed, a history
// sync landed) while keeping your place.
func (m *model) reloadOpen() tea.Cmd {
	if m.openJID == "" {
		return nil
	}
	return loadPageCmd(m.ctx, m.be, m.openJID, m.loadSeq, nil, max(pageSize, len(m.msgs)), true)
}

func (m *model) pageLoaded(msg pageLoadedMsg) tea.Cmd {
	if msg.chatJID != m.openJID || msg.seq != m.loadSeq {
		return nil // for a chat we've since left
	}
	if msg.older {
		m.loadingOlder = false
	}
	if msg.err != nil {
		return m.fail(msg.err)
	}

	items := make([]msgItem, len(msg.msgs))
	for i, v := range msg.msgs {
		items[i] = msgItem{view: v}
	}

	switch {
	case msg.older:
		m.haveOlder = len(items) >= msg.limit
		if len(items) == 0 {
			if m.jump != nil {
				return m.continueJump()
			}
			return m.askPhoneForOlder()
		}
		// Measured from the bottom, the scroll position is unaffected by
		// lines added above.
		m.msgs = append(items, m.msgs...)
	case msg.keep:
		m.msgs = items
		m.haveOlder = m.haveOlder || len(items) >= msg.limit
	default:
		m.msgs = items
		m.haveOlder = len(items) >= msg.limit
		m.scrollBack = 0
		m.unreadFrom = m.firstUnread()
	}
	m.reindex()
	if _, ok := m.msgIdx[m.selID]; !ok && m.focus == focusMessages {
		m.selID = ""
		if len(m.msgs) > 0 {
			m.selID = m.msgs[len(m.msgs)-1].view.ID
		}
	}
	m.rebuildLines()
	return tea.Batch(m.wantPreviews(), m.continueJump())
}

// firstUnread finds where the "unread messages" divider goes: above the
// oldest of the chat's UnreadCount newest incoming messages.
func (m *model) firstUnread() string {
	n := m.open.UnreadCount
	if i, ok := m.chatIdx[m.openJID]; ok {
		n = m.chats[i].UnreadCount
	}
	id := ""
	for i := len(m.msgs) - 1; i >= 0 && n > 0; i-- {
		if v := &m.msgs[i].view; !v.FromMe && v.Kind != whatsapp.KindSystem {
			id = v.ID
			n--
		}
	}
	return id
}

func (m *model) msgUpdated(msg msgUpdatedMsg) tea.Cmd {
	if msg.chatJID != m.openJID {
		return nil
	}
	i, known := m.msgIdx[msg.id]
	before := len(m.lines)
	appended := false

	switch {
	case !msg.found:
		if !known {
			return nil
		}
		if m.selID == msg.id {
			m.selID = ""
			if i+1 < len(m.msgs) {
				m.selID = m.msgs[i+1].view.ID
			} else if i > 0 {
				m.selID = m.msgs[i-1].view.ID
			}
		}
		m.msgs = append(m.msgs[:i], m.msgs[i+1:]...)
		m.reindex()
	case known:
		m.msgs[i] = msgItem{view: msg.view}
	case len(m.msgs) == 0 || isOlder(&m.msgs[len(m.msgs)-1].view.Message, &msg.view.Message):
		m.msgs = append(m.msgs, msgItem{view: msg.view})
		m.msgIdx[msg.id] = len(m.msgs) - 1
		appended = true
	case isOlder(&msg.view.Message, &m.msgs[0].view.Message):
		// Older than everything loaded (history arriving from the phone):
		// it will show up when you scroll up.
		m.haveOlder = true
		delete(m.askedPhone, m.openJID)
		return nil
	default:
		// Lands in the middle (messages delivered out of order).
		at := len(m.msgs)
		for at > 0 && isOlder(&msg.view.Message, &m.msgs[at-1].view.Message) {
			at--
		}
		m.msgs = append(m.msgs[:at], append([]msgItem{{view: msg.view}}, m.msgs[at:]...)...)
		m.reindex()
	}

	m.rebuildLines()
	if appended && m.scrollBack > 0 {
		// Reading older messages: stay put and count what arrived below.
		m.scrollBack += len(m.lines) - before
		if !msg.view.FromMe {
			m.newBelow++
		}
	}
	if appended && msg.view.FromMe {
		m.scrollBack, m.newBelow = 0, 0 // your own message: jump to it
	}
	return tea.Batch(m.wantPreviews(), m.markReadIfWatching())
}

// askPhoneForOlder requests history the local store doesn't have, at most
// once every half minute per chat.
func (m *model) askPhoneForOlder() tea.Cmd {
	if m.openJID == "" || len(m.msgs) == 0 || time.Since(m.askedPhone[m.openJID]) < 30*time.Second {
		return nil
	}
	m.askedPhone[m.openJID] = time.Now()
	return loadOlderCmd(m.ctx, m.be, m.openJID)
}

// loadOlder fetches the page before the oldest loaded message.
func (m *model) loadOlder() tea.Cmd {
	if m.loadingOlder || len(m.msgs) == 0 {
		return nil
	}
	if !m.haveOlder {
		return m.askPhoneForOlder()
	}
	m.loadingOlder = true
	oldest := m.msgs[0].view.Message
	return loadPageCmd(m.ctx, m.be, m.openJID, m.loadSeq, &oldest, pageSize, false)
}

// --- scrolling and selection ---

func (m *model) maxScroll() int {
	return max(0, len(m.lines)-m.calc().msgH)
}

// scroll moves the pane by delta lines (positive = towards older).
func (m *model) scroll(delta int) tea.Cmd {
	m.scrollBack = max(0, min(m.scrollBack+delta, m.maxScroll()))
	var cmds []tea.Cmd
	if m.scrollBack == 0 {
		m.newBelow = 0
		cmds = append(cmds, m.markReadIfWatching())
	}
	if delta > 0 && m.scrollBack >= m.maxScroll() {
		cmds = append(cmds, m.loadOlder())
	}
	cmds = append(cmds, m.wantPreviews())
	return tea.Batch(cmds...)
}

func (m *model) selected() *whatsapp.MessageView {
	if i, ok := m.msgIdx[m.selID]; ok {
		return &m.msgs[i].view
	}
	return nil
}

// selectMsg moves the selection by delta messages, skipping info lines.
func (m *model) selectMsg(delta int) tea.Cmd {
	if len(m.msgs) == 0 {
		return nil
	}
	i, ok := m.msgIdx[m.selID]
	if !ok {
		i = len(m.msgs)
		delta = -1
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	j := i
	for n := 0; n != delta; n += step {
		k := j + step
		for k >= 0 && k < len(m.msgs) && m.msgs[k].view.Kind == whatsapp.KindSystem {
			k += step
		}
		if k < 0 || k >= len(m.msgs) {
			break
		}
		j = k
	}
	if j < 0 || j >= len(m.msgs) {
		return nil
	}
	m.selID = m.msgs[j].view.ID
	m.revealSelected()
	var cmd tea.Cmd
	if j == 0 && delta < 0 {
		cmd = m.loadOlder()
	}
	return tea.Batch(cmd, m.wantPreviews())
}

// revealSelected scrolls just enough to show the whole selected message.
func (m *model) revealSelected() {
	i, ok := m.msgIdx[m.selID]
	if !ok || i >= len(m.msgStart) {
		return
	}
	h := m.calc().msgH
	start := m.msgStart[i]
	end := len(m.lines)
	if i+1 < len(m.msgStart) {
		end = m.msgStart[i+1]
	}
	bottom := len(m.lines) - m.scrollBack // line just past the pane
	if end > bottom {
		m.scrollBack = len(m.lines) - end
	}
	if start < len(m.lines)-m.scrollBack-h {
		m.scrollBack = len(m.lines) - h - start
	}
	m.scrollBack = max(0, min(m.scrollBack, m.maxScroll()))
}

// visibleMsgs returns the index range of messages on screen (plus a
// margin), for loading only the pictures that are needed.
func (m *model) visibleMsgs() (from, to int) {
	if len(m.msgs) == 0 || len(m.lineMsg) == 0 {
		return 0, 0
	}
	h := m.calc().msgH
	bottom := len(m.lines) - m.scrollBack
	top := max(0, bottom-h)
	from, to = len(m.msgs), 0
	for li := top; li < bottom && li < len(m.lineMsg); li++ {
		if i := m.lineMsg[li]; i >= 0 {
			from, to = min(from, i), max(to, i+1)
		}
	}
	if from >= to {
		return 0, 0
	}
	return max(0, from-10), min(len(m.msgs), to+10)
}

// --- drawing ---

// msgWidth is the width messages are laid out in: the pane minus the
// selection gutter and a one-column right margin.
func (m *model) msgWidth() int { return max(20, m.calc().paneW-3) }

// bubbleWidth caps how wide one message's text gets.
func (m *model) bubbleWidth() int { return max(16, min(m.msgWidth()-6, 78)) }

func (m *model) invalidateAll() {
	for i := range m.msgs {
		m.msgs[i].block = nil
	}
}

func (m *model) invalidateMedia() {
	for i := range m.msgs {
		if m.msgs[i].view.Media != nil {
			m.msgs[i].block = nil
		}
	}
}

// resized re-lays everything out for a new terminal size.
func (m *model) resized() {
	// A short terminal gets a shorter message box, so messages stay visible.
	m.ed.maxRows = max(1, min(6, (m.height-10)/2))
	l := m.calc()
	m.ed.SetWidth(l.paneW - 6)
	m.invalidateAll()
	m.rebuildLines()
	m.keepSelectionVisible()
	m.scrollBack = max(0, min(m.scrollBack, m.maxScroll()))
}

// rebuildLines lays the loaded messages out as one list of lines, adding
// day separators, the unread divider and a little air between speakers.
func (m *model) rebuildLines() {
	w := m.msgWidth()
	now := time.Now()
	group := m.open.IsGroup

	m.lines = m.lines[:0]
	m.lineMsg = m.lineMsg[:0]
	m.msgStart = m.msgStart[:0]
	add := func(idx int, lines ...string) {
		for _, l := range lines {
			m.lines = append(m.lines, l)
			m.lineMsg = append(m.lineMsg, idx)
		}
	}

	var prev *whatsapp.MessageView
	for i := range m.msgs {
		it := &m.msgs[i]
		v := &it.view
		t := time.UnixMilli(v.Timestamp).Local()

		newDay := prev == nil || !sameDay(t, time.UnixMilli(prev.Timestamp).Local())
		if newDay {
			if prev != nil {
				add(-1, "")
			}
			add(-1, faintStyle.Render(center("── "+dayLabel(t, now)+" ──", w)))
		}
		if v.ID == m.unreadFrom && m.unreadFrom != "" {
			add(-1, statusStyle.Render(center("── unread messages ──", w)))
		}

		// A new "run": someone else starts talking, or after a pause.
		newRun := newDay || prev == nil || prev.FromMe != v.FromMe || prev.SenderJID != v.SenderJID ||
			prev.Kind == whatsapp.KindSystem || v.Timestamp-prev.Timestamp > 10*60*1000
		showSender := group && !v.FromMe && newRun && v.Kind != whatsapp.KindSystem
		if newRun && !newDay {
			add(-1, "")
		}

		if it.block == nil || it.blockSender != showSender {
			it.block = m.renderBlock(v, showSender, w)
			it.blockSender = showSender
		}
		m.msgStart = append(m.msgStart, len(m.lines))
		add(i, it.block...)
		prev = v
	}
	m.scrollBack = max(0, min(m.scrollBack, m.maxScroll()))
}

// renderBlock draws one message as lines w columns wide at most: incoming
// ones against the left edge, your own against the right.
func (m *model) renderBlock(v *whatsapp.MessageView, showSender bool, w int) []string {
	if v.Kind == whatsapp.KindSystem {
		var out []string
		for _, line := range strings.Split(ansi.Wrap(sanitize(v.Body), w-4, ""), "\n") {
			out = append(out, systemStyle.Render(center(line, w)))
		}
		return out
	}

	bw := m.bubbleWidth()
	base := theirMsgStyle
	if v.FromMe {
		base = myMsgStyle
	}
	var out []string

	if showSender {
		out = append(out, senderStyle(v.SenderJID).Render(truncate(oneLine(v.Sender), bw)))
	}
	if v.Forwarded {
		out = append(out, quoteStyle.Render("↪ Forwarded"))
	}
	if q := v.Quote; q != nil {
		who := oneLine(q.Sender)
		if who != "" {
			who += ": "
		}
		out = append(out, quoteStyle.Render("▎ "+truncate(who+oneLine(q.Text), bw-2)))
	}

	switch {
	case v.Kind == whatsapp.KindDeleted || v.Deleted:
		text := "🚫 This message was deleted"
		if v.FromMe {
			text = "🚫 You deleted this message"
		}
		out = append(out, systemStyle.Render(text))
	case v.Media != nil:
		out = append(out, m.mediaLines(v, bw)...)
	case v.Poll != nil:
		out = append(out, pollLines(v.Poll, bw)...)
	}

	if body := strings.TrimRight(sanitize(v.Body), "\n "); body != "" && !v.Deleted && v.Poll == nil {
		for _, line := range strings.Split(ansi.Wrap(body, bw, ""), "\n") {
			out = append(out, base.Render(line))
		}
	}

	// Time and ticks go on the end of the last line when there's room.
	meta, metaW := metaText(v)
	if n := len(out); n > 0 && ansi.StringWidth(out[n-1])+1+metaW <= w && !isPicture(out[n-1]) {
		out[n-1] += " " + meta
	} else {
		out = append(out, meta)
	}

	if len(v.Reactions) > 0 {
		var parts []string
		for _, r := range v.Reactions {
			part := oneLine(r.Emoji) // from the network, like everything else here
			if r.Count > 1 {
				part += " " + itoa(r.Count)
			}
			if r.Mine {
				part = lipgloss.NewStyle().Underline(true).Render(part)
			}
			parts = append(parts, part)
		}
		out = append(out, truncate(strings.Join(parts, "  "), w))
	}

	if v.Mentioned && !v.FromMe {
		out[0] = mentionStyle.Render("@") + " " + out[0]
	}

	// Line the block up: left for them, right for you.
	width := 0
	for i, line := range out {
		if lw := ansi.StringWidth(line); lw > w {
			out[i] = ansi.Truncate(line, w, "")
			width = w
		} else {
			width = max(width, lw)
		}
	}
	if v.FromMe {
		pad := strings.Repeat(" ", w-width)
		for i := range out {
			out[i] = pad + out[i]
		}
	}
	return out
}

// isPicture reports whether a drawn line is part of a half-block picture,
// which must not have text appended.
func isPicture(line string) bool { return strings.Contains(line, "▀") }

// metaText is the small print after a message: "edited", the time, and
// for your own messages the delivery ticks.
func metaText(v *whatsapp.MessageView) (string, int) {
	text := time.UnixMilli(v.Timestamp).Local().Format("15:04")
	if v.Edited {
		text = "edited " + text
	}
	width := ansi.StringWidth(text)
	out := faintStyle.Render(text)
	if !v.FromMe {
		return out, width
	}
	tick, style := "", faintStyle
	switch v.Status {
	case msgstore.StatusPending:
		tick = "…"
	case msgstore.StatusSent:
		tick = "✓"
	case msgstore.StatusDelivered:
		tick = "✓✓"
	case msgstore.StatusRead, msgstore.StatusPlayed:
		tick, style = "✓✓", readTickStyle
	case msgstore.StatusFailed:
		tick, style = "✗ not sent", statusErrStyle
	}
	if tick == "" {
		return out, width
	}
	return out + " " + style.Render(tick), width + 1 + ansi.StringWidth(tick)
}

// mediaLines draws an attachment: its picture when there is one, and a
// line saying what it is.
func (m *model) mediaLines(v *whatsapp.MessageView, bw int) []string {
	mi := v.Media
	var out []string

	if cols, rows := previewBox(v, bw); cols > 0 {
		if pic, state := m.previews.get(v.ID, cols, rows); state == previewReady {
			out = append(out, strings.Split(pic, "\n")...)
		} else if state != previewFailed {
			// Hold the picture's place so the chat doesn't jump when it loads.
			filler := faintStyle.Render(strings.Repeat("░", cols))
			for i := 0; i < rows; i++ {
				out = append(out, filler)
			}
		}
	}

	var parts []string
	switch v.Kind {
	case whatsapp.KindImage:
		label := "📷 Photo"
		if v.HD {
			label += " · HD"
		}
		parts = append(parts, label)
	case whatsapp.KindVideo:
		parts = append(parts, "🎬 Video")
	case whatsapp.KindGIF:
		parts = append(parts, "🎬 GIF")
	case whatsapp.KindVoice:
		parts = append(parts, "🎤 Voice message")
	case whatsapp.KindAudio:
		parts = append(parts, "🎵 Audio")
	case whatsapp.KindSticker:
		parts = append(parts, "Sticker")
	default:
		name := oneLine(mi.FileName)
		if name == "" {
			name = "Document"
		}
		parts = append(parts, "📄 "+name)
	}
	if mi.ViewOnce {
		parts = append(parts, "view once (open it on your phone)")
	}
	if mi.Seconds > 0 {
		parts = append(parts, clock(mi.Seconds))
	}
	if mi.Width > 0 && mi.Height > 0 && (v.Kind == whatsapp.KindImage || v.Kind == whatsapp.KindVideo) {
		parts = append(parts, itoa(mi.Width)+"×"+itoa(mi.Height))
	}
	if mi.Pages > 0 {
		parts = append(parts, itoa(mi.Pages)+" pages")
	}
	if size := humanSize(mi.Size); size != "" {
		parts = append(parts, size)
	}
	return append(out, mediaStyle.Render(truncate(strings.Join(parts, " · "), bw)))
}

// pollLines draws a poll: the question, then each option with a bar.
func pollLines(p *whatsapp.PollView, bw int) []string {
	bw = min(bw, 46)
	out := []string{mediaStyle.Render(truncate("📊 "+oneLine(p.Question), bw))}
	most := 1
	for _, o := range p.Options {
		most = max(most, o.Votes)
	}
	const bar = 8
	for _, o := range p.Options {
		mark := "○"
		if o.Mine {
			mark = "●"
		}
		filled := o.Votes * bar / most
		line := mark + " " + padRight(truncate(oneLine(o.Name), bw-bar-8), bw-bar-8) + " " +
			strings.Repeat("█", filled) + strings.Repeat("░", bar-filled) + " " + itoa(o.Votes)
		out = append(out, theirMsgStyle.Render(line))
	}
	return out
}

// renderMessages returns exactly h lines: the part of the chat in view,
// pushed to the bottom when it doesn't fill the pane.
func (m *model) renderMessages(l layout) []string {
	h := l.msgH
	out := make([]string, 0, h)

	if len(m.lines) == 0 {
		text := "No messages here yet."
		if m.openJID == "" {
			text = "Pick a chat with ↑/↓ and press Enter."
		}
		for len(out) < h-1 {
			out = append(out, "")
		}
		return append(out, "  "+dimStyle.Render(text))
	}

	bottom := len(m.lines) - min(m.scrollBack, m.maxScroll())
	top := max(0, bottom-h)
	for i := 0; i < h-(bottom-top); i++ {
		out = append(out, "")
	}
	sel := -1
	if m.focus == focusMessages {
		if i, ok := m.msgIdx[m.selID]; ok {
			sel = i
		}
	}
	for li := top; li < bottom; li++ {
		if sel >= 0 && m.lineMsg[li] == sel {
			out = append(out, gutterStyle.Render("▌")+" "+m.lines[li])
		} else {
			out = append(out, "  "+m.lines[li])
		}
	}
	return out
}
