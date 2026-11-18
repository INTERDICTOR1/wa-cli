package tui

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// layout is where everything goes for the current terminal size and
// state. It is recomputed whenever needed (it's a little arithmetic)
// rather than stored, so it can never be stale.
type layout struct {
	sideW int // chat list width, border included
	paneW int // message side width
	bodyH int // height above the status and help lines
	msgH  int // lines of messages shown
	extra int // lines between the messages and the message box
}

func (m *model) calc() layout {
	var l layout
	l.sideW = max(24, min(38, m.width*3/10))
	if m.width < 60 {
		l.sideW = max(16, m.width/3)
	}
	l.paneW = max(20, m.width-l.sideW)
	l.bodyH = max(8, m.height-2)
	inputH := m.ed.Height() + 2
	// The header and rule take two lines and the messages keep at least
	// two; whatever context lines don't fit a short terminal are dropped.
	l.extra = min(len(m.contextLines(l.paneW)), max(0, l.bodyH-4-inputH))
	l.msgH = max(2, l.bodyH-2-l.extra-inputH)
	return l
}

// View satisfies tea.Model: it draws the whole frame from the state. It
// always produces exactly the terminal's height in lines - a taller frame
// would get its top cut off by Bubble Tea.
func (m *model) View() string {
	if !m.ready {
		return "Loading wa-cli…"
	}
	if m.viewer != nil {
		return m.viewer.view(m)
	}
	l := m.calc()

	pane := make([]string, 0, l.bodyH)
	pane = append(pane, m.renderHeader(l), faintStyle.Render(strings.Repeat("─", l.paneW)))
	if m.overlay != nil {
		pane = append(pane, m.overlay.view(m, l.paneW, l.msgH)...)
	} else {
		pane = append(pane, m.renderMessages(l)...)
	}
	if ctx := m.contextLines(l.paneW); l.extra > 0 {
		pane = append(pane, ctx[len(ctx)-l.extra:]...)
	}
	pane = append(pane, m.renderInput(l))

	body := lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(l), strings.Join(pane, "\n"))

	status := ""
	if m.status != "" {
		style := statusStyle
		if m.statusErr {
			style = statusErrStyle
		}
		status = style.Render(truncate(m.status, m.width))
	}
	frame := body + "\n" + status + "\n" + helpStyle.Render(truncate(m.helpText(), m.width))
	dlog("view %d lines, terminal height %d", strings.Count(frame, "\n")+1, m.height)
	return frame
}

// renderHeader is the line above the messages: who you're talking to and
// what they're doing, with the connection state on the right.
func (m *model) renderHeader(l layout) string {
	left := ""
	if m.openJID != "" {
		left = headerStyle.Render(truncate(m.open.Title, l.paneW/2))
		if sub := m.subtitle(); sub != "" {
			style := dimStyle
			if sub == "online" || strings.HasSuffix(sub, "…") {
				style = onlineStyle
			}
			left += "  " + style.Render(sub)
		}
	}
	right := ""
	switch m.conn {
	case whatsapp.ConnConnecting:
		right = dimStyle.Render("connecting…")
	case whatsapp.ConnDisconnected:
		right = statusErrStyle.Render("offline · reconnecting…")
	case whatsapp.ConnLoggedOut:
		right = statusErrStyle.Render("logged out")
	case whatsapp.ConnReplaced:
		right = statusErrStyle.Render("connected elsewhere")
	}
	if m.newBelow > 0 {
		right = statusStyle.Render("↓ "+itoa(m.newBelow)+" new") + " " + right
	}
	return " " + spread(left, right, l.paneW-2)
}

// typingText describes who is typing in a chat right now, or "".
func (m *model) typingText(chatJID string) string {
	chat := m.typing[chatJID]
	if len(chat) == 0 {
		return ""
	}
	var names []string
	recording := false
	for _, t := range chat {
		if time.Since(t.at) > typingLifetime {
			continue
		}
		names = append(names, oneLine(t.name))
		recording = recording || t.state == whatsapp.TypingAudio
	}
	what := "typing…"
	if recording {
		what = "recording audio…"
	}
	switch {
	case len(names) == 0:
		return ""
	case !strings.HasSuffix(chatJID, "@g.us"):
		return what
	case len(names) == 1:
		return names[0] + " is " + what
	}
	return itoa(len(names)) + " people are typing…"
}

func (m *model) subtitle() string {
	if t := m.typingText(m.openJID); t != "" {
		return t
	}
	if m.open.IsGroup {
		return "group"
	}
	if p, ok := m.presence[m.openJID]; ok {
		if p.Online {
			return "online"
		}
		return lastSeenText(p.LastSeen, time.Now())
	}
	return ""
}

// contextLines are the lines between the messages and the message box:
// what you're replying to or editing, a file about to be sent, a
// question waiting for an answer, or the @mention / emoji dropdown.
func (m *model) contextLines(w int) []string {
	var out []string
	bar := func(label, text string) string {
		label = oneLine(label)
		return " " + gutterStyle.Render("▎") + " " + headerStyle.Render(label) + " " + dimStyle.Render(truncate(oneLine(text), max(4, w-6-ansi.StringWidth(label))))
	}
	switch {
	case m.confirm != nil:
		out = append(out, " "+statusStyle.Render(truncate(m.confirm.text, w-2)))
	case m.prompt != nil:
		out = append(out, " "+headerStyle.Render(m.prompt.label)+" "+m.prompt.ed.View(true, ""))
	case m.react != nil:
		out = append(out, m.react.view(w))
	}
	if m.editing != nil {
		out = append(out, bar("Editing", m.editing.Body))
	}
	if m.replyTo != nil {
		who := m.replyTo.Sender
		if m.replyTo.FromMe {
			who = "yourself"
		}
		out = append(out, bar("Reply to "+oneLine(who), previewOf(m.replyTo)))
	}
	if m.attach != nil {
		how := "as " + fileKind(m.attach.path)
		if m.attach.asDocument {
			how = "as a document"
		}
		out = append(out, bar("📎 "+filepath.Base(m.attach.path), humanSize(uint64(m.attach.size))+" · "+how+" · ctrl+d document on/off · esc remove"))
	}
	if m.focus == focusInput {
		out = append(out, m.sug.view(w)...)
	}
	return out
}

// previewOf is a one-line description of a message, for reply bars.
func previewOf(v *whatsapp.MessageView) string {
	body := oneLine(v.Body)
	if v.Media == nil {
		return body
	}
	kind := v.Kind
	if v.Media.FileName != "" {
		kind = v.Media.FileName
	}
	return strings.TrimSpace("[" + kind + "] " + body)
}

func (m *model) renderInput(l layout) string {
	style := borderStyle
	if m.focus == focusInput && m.overlay == nil {
		style = borderFocusedStyle
	}
	placeholder := "Type a message…"
	switch {
	case m.openJID == "":
		placeholder = "Pick a chat first"
	case m.editing != nil:
		placeholder = "Edit your message…"
	case m.attach != nil:
		placeholder = "Add a caption… (Enter sends the file)"
	}
	focused := m.focus == focusInput && m.overlay == nil && m.prompt == nil && m.confirm == nil && m.react == nil
	return style.Padding(0, 1).Width(l.paneW - 2).Render(m.ed.View(focused, placeholder))
}

// helpText is the bottom line: the keys that work right now.
func (m *model) helpText() string {
	switch {
	case m.confirm != nil, m.prompt != nil:
		return "enter confirm · esc cancel"
	case m.react != nil:
		return "←/→ choose · enter react · type any emoji · del remove yours · esc cancel"
	case m.overlay != nil:
		return m.overlay.help()
	}
	switch m.focus {
	case focusChatList:
		return "type to search · ↑/↓ move · enter open · ctrl+p pin · ctrl+a archive · ctrl+t mute · ctrl+u unread · ctrl+r archived · f1 help · ctrl+c quit"
	case focusMessages:
		return "↑/↓ select · r reply · e react · E edit · d delete · f forward · c copy · enter view · o open · s save · esc back · ? help"
	}
	return "enter send · shift+enter new line · ↑ select message · @ mention · : emoji · ctrl+o attach · alt+v paste image · ctrl+f search · tab chats · f1 help"
}
