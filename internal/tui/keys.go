package tui

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// handleKey routes a key press to whatever currently owns the keyboard:
// the photo viewer, a question, an overlay, or the focused area.
func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyCtrlC {
		return m.quit()
	}
	if m.pastedBurst(msg) {
		return m.burstKey(msg)
	}
	switch {
	case m.viewer != nil:
		return m.viewer.key(m, msg)
	case m.confirm != nil:
		return m.confirm.key(m, msg)
	case m.prompt != nil:
		return m.prompt.key(m, msg)
	case m.overlay != nil:
		return m.overlay.key(m, msg)
	case m.react != nil:
		return m.react.key(m, msg)
	}
	if msg.Type == tea.KeyF1 {
		return m.openHelp()
	}
	switch m.focus {
	case focusChatList:
		return m.listKey(msg)
	case focusMessages:
		return m.messagesKey(msg)
	}
	return m.inputKey(msg)
}

// burstGap is the longest pause between two keys of a paste that arrives
// as separate key presses. Nobody types two keys this close together.
const burstGap = 12 * time.Millisecond

// pastedBurst reports whether a text or Enter key arrived so soon after
// the previous key that it must be part of pasted text.
//
// Normally a paste arrives as one message marked Paste (from our console
// reader on Windows, from bracketed paste elsewhere). This guard is for
// terminals where neither works and a paste shows up as a rapid burst of
// ordinary key presses - which must never be taken as "send" or as
// commands like delete or forward.
func (m *model) pastedBurst(msg tea.KeyMsg) bool {
	now := time.Now()
	burst := m.burstGuard && !msg.Paste && now.Sub(m.lastKeyAt) < burstGap
	m.lastKeyAt = now
	if !burst {
		return false
	}
	switch msg.Type {
	case tea.KeyRunes, tea.KeySpace, tea.KeyEnter:
		return !msg.Alt
	}
	return false
}

// burstKey handles a key that is part of a pasted burst: text where text
// is being written, otherwise nothing.
func (m *model) burstKey(msg tea.KeyMsg) tea.Cmd {
	plain := m.viewer == nil && m.confirm == nil && m.prompt == nil && m.overlay == nil && m.react == nil
	switch {
	case plain && m.focus == focusInput && m.openJID != "":
		if msg.Type == tea.KeyEnter {
			m.ed.Insert("\n") // a pasted line break, not "send"
		} else {
			m.ed.Insert(keyText(msg))
		}
		return m.edited()
	case plain && m.focus == focusChatList && msg.Type != tea.KeyEnter:
		return m.setFilter(m.filter + oneLine(keyText(msg)))
	}
	return nil
}

func (m *model) quit() tea.Cmd {
	// Keep what was typed as the chat's draft, then leave.
	if m.openJID != "" && m.editing == nil {
		if text := m.ed.Value(); text != m.open.Draft || m.drafts[m.openJID] != "" {
			_ = m.be.SetDraft(m.openJID, text)
		}
	}
	return tea.Quit
}

// keyText returns what a key press types, or "" for a non-text key.
func keyText(msg tea.KeyMsg) string {
	if msg.Alt {
		return ""
	}
	switch msg.Type {
	case tea.KeyRunes:
		return string(msg.Runes)
	case tea.KeySpace:
		return " "
	}
	return ""
}

// --- chat list ---

func (m *model) listKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyUp:
		return m.moveSelection(-1)
	case tea.KeyDown:
		return m.moveSelection(1)
	case tea.KeyPgUp:
		return m.moveSelection(-m.listRows())
	case tea.KeyPgDown:
		return m.moveSelection(m.listRows())
	case tea.KeyHome:
		return m.moveSelection(-len(m.visible))
	case tea.KeyEnd:
		return m.moveSelection(len(m.visible))
	case tea.KeyEnter, tea.KeyTab:
		return m.enterChat()
	case tea.KeyEsc:
		if m.filter != "" {
			return m.setFilter("")
		}
		return nil
	case tea.KeyBackspace:
		if r := []rune(m.filter); len(r) > 0 {
			return m.setFilter(string(r[:len(r)-1]))
		}
		return nil
	case tea.KeyCtrlU:
		return m.chatAction("unread")
	case tea.KeyCtrlP:
		return m.chatAction("pin")
	case tea.KeyCtrlA:
		return m.chatAction("archive")
	case tea.KeyCtrlT:
		return m.chatAction("mute")
	case tea.KeyCtrlR:
		m.showArchived = !m.showArchived
		m.refilter()
		return m.showSelected()
	case tea.KeyCtrlF:
		return m.openSearch("")
	}
	if text := keyText(msg); text != "" {
		return m.setFilter(m.filter + oneLine(text))
	}
	return nil
}

// chatAction toggles a property of the highlighted chat.
func (m *model) chatAction(what string) tea.Cmd {
	c, ok := m.selectedItem()
	if !ok || c.contact {
		return nil
	}
	ctx, be, jid := m.ctx, m.be, c.JID
	switch what {
	case "pin":
		return do(func() error { return be.SetPinned(ctx, jid, !c.Pinned) })
	case "archive":
		return do(func() error { return be.SetArchived(ctx, jid, !c.Archived) })
	case "mute":
		return do(func() error { return be.SetMuted(ctx, jid, !(c.Muted || c.MutedUntil != 0)) })
	case "unread":
		if c.UnreadCount > 0 || c.MarkedUnread {
			return do(func() error { return be.MarkChatRead(ctx, jid) })
		}
		return do(func() error { return be.MarkChatUnread(ctx, jid) })
	}
	return nil
}

// --- message box ---

func (m *model) inputKey(msg tea.KeyMsg) tea.Cmd {
	if m.sug.active() {
		if cmd, handled := m.sug.key(m, msg); handled {
			return cmd
		}
	}

	switch msg.Type {
	case tea.KeyEnter:
		if msg.Alt { // alt+enter: a line break, like shift+enter
			m.ed.Insert("\n")
			return m.edited()
		}
		return m.send()
	case tea.KeyCtrlJ: // shift+enter and ctrl+enter arrive as this
		m.ed.Insert("\n")
		return m.edited()
	case tea.KeyEsc:
		switch {
		case m.editing != nil:
			m.editing = nil
			m.ed.SetValue(m.drafts[m.openJID])
		case m.replyTo != nil:
			m.replyTo = nil
		case m.attach != nil:
			m.attach = nil
		default:
			return m.focusList()
		}
		return nil
	case tea.KeyTab:
		return m.focusList()
	case tea.KeyUp:
		if !m.ed.Up() {
			return m.focusMessagesAtEnd()
		}
		return nil
	case tea.KeyDown:
		m.ed.Down()
		return nil
	case tea.KeyPgUp:
		return m.scroll(max(1, m.calc().msgH-2))
	case tea.KeyPgDown:
		return m.scroll(-max(1, m.calc().msgH-2))
	case tea.KeyShiftUp:
		return m.scroll(3)
	case tea.KeyShiftDown:
		return m.scroll(-3)
	case tea.KeyLeft:
		m.ed.Left()
		return m.sug.update(m)
	case tea.KeyRight:
		m.ed.Right()
		return m.sug.update(m)
	case tea.KeyCtrlLeft:
		m.ed.WordLeft()
		return nil
	case tea.KeyCtrlRight:
		m.ed.WordRight()
		return nil
	case tea.KeyHome:
		m.ed.Home()
		return nil
	case tea.KeyEnd:
		m.ed.LineEnd()
		return nil
	case tea.KeyCtrlHome:
		m.ed.Start()
		return nil
	case tea.KeyCtrlEnd:
		m.ed.End()
		return nil
	case tea.KeyBackspace:
		if msg.Alt {
			m.ed.DeleteWordBack()
		} else {
			m.ed.Backspace()
		}
		return m.edited()
	case tea.KeyDelete:
		m.ed.Delete()
		return m.edited()
	case tea.KeyCtrlW:
		m.ed.DeleteWordBack()
		return m.edited()
	case tea.KeyCtrlU:
		m.ed.KillToLineStart()
		return m.edited()
	case tea.KeyCtrlK:
		m.ed.KillToLineEnd()
		return m.edited()
	case tea.KeyCtrlD:
		if m.attach != nil {
			m.attach.asDocument = !m.attach.asDocument
		}
		return nil
	case tea.KeyCtrlO:
		return m.openAttachPrompt()
	case tea.KeyCtrlV:
		return m.pasteClipboard()
	case tea.KeyCtrlF:
		return m.openSearch(m.openJID)
	case tea.KeyCtrlG:
		return m.openInfo()
	}

	if msg.Alt && msg.Type == tea.KeyRunes && string(msg.Runes) == "v" {
		return pasteImageCmd(m.opts.MediaDir)
	}
	text := keyText(msg)
	if text == "" || m.openJID == "" {
		return nil
	}
	if msg.Paste {
		// A file dropped on the terminal, or its path pasted, becomes an
		// attachment rather than text.
		if path, ok := asFilePath(text); ok && m.attach == nil && m.ed.Empty() {
			return m.setAttachment(path)
		}
	}
	m.ed.Insert(text)
	return m.edited()
}

// edited runs after the message box's text changed.
func (m *model) edited() tea.Cmd {
	return tea.Batch(m.sug.update(m), m.typed())
}

func (m *model) focusList() tea.Cmd {
	m.focus = focusChatList
	if m.openJID != "" {
		m.selectedJID = m.openJID
		m.keepSelectionVisible()
	}
	return tea.Batch(m.stopTyping(), m.stashDraft())
}

func (m *model) focusMessagesAtEnd() tea.Cmd {
	if len(m.msgs) == 0 {
		return nil
	}
	m.focus = focusMessages
	m.selID = ""
	return m.selectMsg(-1)
}

func (m *model) pasteClipboard() tea.Cmd {
	text, err := clipboard.ReadAll()
	if err != nil || text == "" {
		return pasteImageCmd(m.opts.MediaDir) // maybe it holds a picture
	}
	if path, ok := asFilePath(text); ok && m.attach == nil && m.ed.Empty() {
		return m.setAttachment(path)
	}
	m.ed.Insert(text)
	return m.edited()
}

// send sends what's in the message box: an edit, a file, or text.
func (m *model) send() tea.Cmd {
	if m.openJID == "" {
		return nil
	}
	text := strings.TrimSpace(m.ed.Value())
	ctx, be, jid := m.ctx, m.be, m.openJID

	if m.editing != nil {
		if text == "" {
			return nil
		}
		target := m.editing.Message
		wire, _ := applyMentions(text, m.sug.picked)
		m.editing = nil
		m.sug.reset()
		m.ed.SetValue(m.drafts[jid])
		if wire == target.Text {
			return nil
		}
		return do(func() error { return be.Edit(ctx, target, wire) })
	}

	if text == "" && m.attach == nil {
		return nil
	}
	wire, mentioned := applyMentions(text, m.sug.picked)
	out := whatsapp.Outgoing{Text: wire, Mentions: mentioned}
	if m.replyTo != nil {
		quoted := m.replyTo.Message
		out.ReplyTo = &quoted
	}
	if m.attach != nil {
		out.FilePath, out.AsDocument = m.attach.path, m.attach.asDocument
		ext := strings.ToLower(filepath.Ext(m.attach.path))
		out.Voice = !out.AsDocument && (ext == ".ogg" || ext == ".opus")
	}

	m.ed.Reset()
	m.replyTo, m.attach = nil, nil
	m.sug.reset()
	m.typingOn = false // sending clears "typing…" on the other side
	m.scrollBack, m.newBelow = 0, 0
	delete(m.drafts, jid)
	cmds := []tea.Cmd{sendCmd(ctx, be, jid, out)}
	if m.open.Draft != "" {
		m.open.Draft = ""
		cmds = append(cmds, saveDraftCmd(be, jid, ""))
	}
	return tea.Batch(cmds...)
}

// --- selected message ---

func (m *model) messagesKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyUp:
		return m.selectMsg(-1)
	case tea.KeyDown:
		if i, ok := m.msgIdx[m.selID]; ok && i == len(m.msgs)-1 {
			return m.backToInput()
		}
		return m.selectMsg(1)
	case tea.KeyPgUp:
		return m.selectMsg(-8)
	case tea.KeyPgDown:
		return m.selectMsg(8)
	case tea.KeyHome:
		return m.selectMsg(-len(m.msgs))
	case tea.KeyEnd:
		return m.selectMsg(len(m.msgs))
	case tea.KeyEsc:
		return m.backToInput()
	case tea.KeyTab:
		m.focus = focusInput
		return m.focusList()
	case tea.KeyEnter:
		return m.activate()
	case tea.KeyDelete:
		return m.askDelete()
	case tea.KeyCtrlF:
		return m.openSearch(m.openJID)
	case tea.KeyCtrlG:
		return m.openInfo()
	}

	if msg.Paste {
		// Pasted text is meant for the message box, never as commands.
		cmd := m.backToInput()
		return tea.Batch(cmd, m.inputKey(msg))
	}
	v := m.selected()
	if v == nil {
		return m.backToInput()
	}
	switch keyText(msg) {
	case "r":
		if v.Deleted {
			return nil
		}
		m.replyTo, m.editing = v, nil
		return m.backToInput()
	case "e":
		if v.Deleted {
			return nil
		}
		m.react = newReactPicker(*v)
	case "E":
		return m.startEdit(v)
	case "d", "x":
		return m.askDelete()
	case "f":
		if v.Deleted || v.Message.RawProto == "" {
			return m.info("This message can't be forwarded.")
		}
		return m.openForward(*v)
	case "c", "y":
		text := v.Body
		if text == "" {
			return m.info("Nothing to copy.")
		}
		if err := clipboard.WriteAll(text); err != nil {
			return m.fail(err)
		}
		return m.info("Copied.")
	case "o":
		return m.openSelected()
	case "s":
		if v.Media == nil {
			return m.info("Nothing to save.")
		}
		return tea.Batch(m.info("Saving…"), saveMediaCmd(m.ctx, m.be, *v, m.opts.DownloadDir, purposeSave))
	case "v":
		return m.activate()
	case "R":
		if v.FromMe && v.Status == msgstore.StatusFailed {
			return m.retry(v)
		}
	case "i":
		return m.openInfo()
	case "/":
		return m.openSearch(m.openJID)
	case "?":
		return m.openHelp()
	case "g":
		return m.selectMsg(-len(m.msgs))
	case "G":
		return m.selectMsg(len(m.msgs))
	}
	return nil
}

func (m *model) backToInput() tea.Cmd {
	m.focus = focusInput
	m.scrollBack, m.newBelow = 0, 0
	return m.markReadIfWatching()
}

func (m *model) retry(v *whatsapp.MessageView) tea.Cmd {
	ctx, be, target := m.ctx, m.be, v.Message
	return do(func() error { return be.Retry(ctx, target) })
}

// activate is Enter on a message: retry a failed one, view a photo, open
// other media or a link, otherwise reply.
func (m *model) activate() tea.Cmd {
	v := m.selected()
	if v == nil {
		return nil
	}
	switch {
	case v.FromMe && v.Status == msgstore.StatusFailed:
		return m.retry(v)
	case v.Deleted:
		return nil
	case v.Media != nil && v.Media.ViewOnce:
		return m.info("View-once media can only be opened on your phone.")
	case v.Kind == whatsapp.KindImage || v.Kind == whatsapp.KindSticker:
		return tea.Batch(m.info("Loading photo…"), saveMediaCmd(m.ctx, m.be, *v, m.opts.MediaDir, purposeView))
	case v.Media != nil:
		return m.openSelected()
	case firstURL(v.Body) != "":
		return m.openSelected()
	}
	m.replyTo, m.editing = v, nil
	return m.backToInput()
}

// openSelected opens the selected message's attachment (or first link)
// with the system's default app.
func (m *model) openSelected() tea.Cmd {
	v := m.selected()
	if v == nil {
		return nil
	}
	if v.Media != nil && !v.Deleted {
		if v.Media.ViewOnce {
			return m.info("View-once media can only be opened on your phone.")
		}
		return tea.Batch(m.info("Downloading…"), saveMediaCmd(m.ctx, m.be, *v, m.opts.MediaDir, purposeOpen))
	}
	if url := firstURL(v.Body); url != "" {
		if err := launch(url); err != nil {
			return m.fail(err)
		}
		return m.info("Opened " + url)
	}
	return m.info("Nothing to open.")
}

// editWindow is how long WhatsApp lets you edit a sent message.
const editWindow = 15 * time.Minute

func (m *model) startEdit(v *whatsapp.MessageView) tea.Cmd {
	switch {
	case !v.FromMe:
		return m.info("You can only edit your own messages.")
	case v.Deleted || v.Poll != nil || (v.Media != nil && v.Body == ""):
		return m.info("This message can't be edited.")
	case time.Since(time.UnixMilli(v.Timestamp)) > editWindow:
		return m.info("Messages can only be edited for 15 minutes.")
	}
	if m.editing == nil {
		m.drafts[m.openJID] = m.ed.Value()
	}
	m.editing, m.replyTo, m.attach = v, nil, nil
	m.ed.SetValue(v.Message.Text)
	m.focus = focusInput
	return nil
}

func (m *model) askDelete() tea.Cmd {
	v := m.selected()
	if v == nil {
		return nil
	}
	ctx, be, target := m.ctx, m.be, v.Message
	forMe := func() tea.Cmd { return do(func() error { return be.DeleteLocal(target) }) }
	if v.FromMe && !v.Deleted && v.Status != msgstore.StatusFailed {
		m.confirm = &confirmPrompt{
			opened: time.Now(),
			text:   "Delete this message?  e = for everyone · m = just here · esc = cancel",
			keys: map[string]func() tea.Cmd{
				"e": func() tea.Cmd { return do(func() error { return be.Revoke(ctx, target) }) },
				"m": forMe,
			},
		}
		return nil
	}
	m.confirm = &confirmPrompt{
		opened: time.Now(),
		text:   "Remove this message from this device?  y = yes · esc = cancel",
		keys:   map[string]func() tea.Cmd{"y": forMe},
	}
	return nil
}

// --- mouse ---

func (m *model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.viewer != nil || m.overlay != nil {
		return nil
	}
	l := m.calc()
	inList := msg.X < l.sideW
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if inList {
			m.listTop = max(0, m.listTop-2)
			return nil
		}
		return m.scroll(3)
	case tea.MouseButtonWheelDown:
		if inList {
			m.listTop = max(0, min(m.listTop+2, len(m.visible)-m.listRows()))
			return nil
		}
		return m.scroll(-3)
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		if inList {
			// Rows start below the border, header and blank line.
			i := m.listTop + (msg.Y-3)/2
			if msg.Y < 3 || i < 0 || i >= len(m.visible) {
				return nil
			}
			m.selectedJID = m.visible[i].JID
			return m.enterChat()
		}
		// A click on a message selects it; below the messages, the box.
		top := 2
		if y := msg.Y - top; y >= 0 && y < l.msgH && len(m.lines) > 0 {
			bottom := len(m.lines) - min(m.scrollBack, m.maxScroll())
			li := max(0, bottom-l.msgH) + y - max(0, l.msgH-bottom)
			if li >= 0 && li < len(m.lineMsg) && m.lineMsg[li] >= 0 {
				m.selID = m.msgs[m.lineMsg[li]].view.ID
				m.focus = focusMessages
			}
			return nil
		}
		if m.openJID != "" {
			m.focus = focusInput
		}
	}
	return nil
}
