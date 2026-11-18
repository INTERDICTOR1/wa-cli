package tui

import (
	"hash/fnv"

	"github.com/charmbracelet/lipgloss"
)

// Colours are 256-colour palette numbers, which every terminal the TUI
// runs in supports. 205 is the purple-pink used for everything "selected".
const accent = lipgloss.Color("205")

var (
	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))

	borderFocusedStyle = borderStyle.BorderForeground(accent)

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	faintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("0")).
			Background(accent).
			Bold(true)
	selectedDimStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("236")).
				Background(accent)

	chatNameStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	unreadNameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	unreadBadge     = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("42")).Bold(true)

	myMsgStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	theirMsgStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	quoteStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	mediaStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("221"))
	systemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)
	readTickStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	mentionStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("221"))
	gutterStyle   = lipgloss.NewStyle().Foreground(accent)

	statusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	statusErrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	onlineStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cursorStyle    = lipgloss.NewStyle().Reverse(true)
)

// senderColors are the colours group members' names cycle through, so
// each person keeps the same colour within a chat (as in WhatsApp).
var senderColors = []lipgloss.Color{"212", "114", "75", "215", "141", "80", "203", "186", "111", "176"}

// senderStyle returns the name style for a sender, picked from their JID.
func senderStyle(jid string) lipgloss.Style {
	h := fnv.New32a()
	h.Write([]byte(jid))
	return lipgloss.NewStyle().Bold(true).Foreground(senderColors[h.Sum32()%uint32(len(senderColors))])
}
