// Package tui implements wa-cli's full-screen terminal interface,
// launched by running `wa` with no subcommand.
//
// How it fits together:
//   - run.go connects WhatsApp and starts Bubble Tea.
//   - model.go (this file) holds the state and reacts to everything that
//     happens: keys (keys.go), results of background work (actions.go)
//     and events from WhatsApp.
//   - chatlist.go, messages.go and view.go draw the three areas; editor.go
//     is the message box; viewer.go shows photos full screen.
//
// The TUI only talks to WhatsApp through the Backend interface
// (backend.go), and never blocks: anything slow runs in a tea.Cmd and
// comes back as a message.
package tui

import (
	"context"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/termimg"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// pageSize is how many messages are loaded at a time: when a chat opens,
// and again each time you scroll past the oldest one loaded.
const pageSize = 80

type focusArea int

const (
	focusChatList focusArea = iota // typing filters the chat list
	focusInput                     // typing writes a message
	focusMessages                  // a message is selected; keys act on it
)

// Options are the user's settings the TUI needs (from config.json).
type Options struct {
	Mouse         bool
	ImageProtocol termimg.Protocol
	MediaDir      string // where media opened for viewing is kept
	DownloadDir   string // where "save" puts files
}

type model struct {
	ctx  context.Context
	be   Backend
	opts Options
	out  io.Writer // the terminal itself, for drawing photos

	width, height int
	ready         bool
	focus         focusArea

	// --- chat list (chatlist.go) ---
	chats        []chatItem
	chatIdx      map[string]int // JID -> index in chats
	visible      []chatItem     // chats (and contacts) passing the filter
	filter       string
	selectedJID  string
	listTop      int // first visible row of the list
	showArchived bool
	contacts     []whatsapp.Contact
	contactsOK   bool // contacts were loaded
	chatsPending bool // a full chat reload is scheduled
	namesPending bool

	// --- open chat (messages.go) ---
	openJID      string
	open         chatItem
	msgs         []msgItem // loaded messages, oldest first
	msgIdx       map[string]int
	haveOlder    bool // the store may hold older messages
	loadingOlder bool
	loadSeq      int                  // bumps on every chat switch, to drop stale loads
	askedPhone   map[string]time.Time // chats whose older history was requested
	lines        []string             // every loaded message, drawn
	lineMsg      []int                // for each line: its message's index, or -1
	msgStart     []int                // for each message: its first line
	scrollBack   int                  // lines hidden below the bottom of the pane
	selID        string               // selected message (focusMessages)
	unreadFrom   string               // first unread message when the chat opened
	newBelow     int                  // messages that arrived while scrolled up
	jump         *jumpTarget          // a message being scrolled to (search)
	previews     *previewCache

	// --- message box ---
	ed       editor
	replyTo  *whatsapp.MessageView
	editing  *whatsapp.MessageView
	attach   *attachment
	drafts   map[string]string
	typingOn bool      // "typing…" is currently shown to the other side
	typingAt time.Time // last keystroke
	sug      suggestions
	react    *reactPicker
	confirm  *confirmPrompt
	overlay  *overlay
	viewer   *viewer
	prompt   *linePrompt

	// --- live state ---
	conn     whatsapp.ConnState
	presence map[string]whatsapp.PresenceEvent
	typing   map[string]map[string]typingInfo // chat -> sender -> state
	focused  bool                             // the terminal window has focus

	status    string
	statusErr bool
	statusSeq int
	titleN    int // unread chats shown in the window title; -1 = not set yet

	// burstGuard is on when key presses come from Bubble Tea's own reader
	// (see pastedBurst); lastKeyAt is when the previous key arrived.
	burstGuard bool
	lastKeyAt  time.Time
}

type typingInfo struct {
	name  string
	state whatsapp.TypingState
	at    time.Time
}

// attachment is a file waiting to be sent with the next Enter.
type attachment struct {
	path       string
	size       int64
	asDocument bool
}

func newModel(ctx context.Context, be Backend, opts Options, out io.Writer) *model {
	return &model{
		ctx:        ctx,
		be:         be,
		opts:       opts,
		out:        out,
		focus:      focusChatList,
		chatIdx:    map[string]int{},
		msgIdx:     map[string]int{},
		askedPhone: map[string]time.Time{},
		drafts:     map[string]string{},
		presence:   map[string]whatsapp.PresenceEvent{},
		typing:     map[string]map[string]typingInfo{},
		previews:   newPreviewCache(),
		ed:         newEditor(),
		conn:       whatsapp.ConnConnecting,
		focused:    true,
		titleN:     -1,
	}
}

// Init satisfies tea.Model: load the chat list right away so the first
// frame has content while the connection comes up.
func (m *model) Init() tea.Cmd {
	return loadChatsCmd(m.ctx, m.be)
}

// Update satisfies tea.Model: the single place every event lands.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.resized()
		if m.viewer != nil {
			return m, m.viewer.redraw(m)
		}
		return m, m.wantPreviews()

	case tea.KeyMsg:
		dlog("key %q paste=%v focus=%d", msg.String(), msg.Paste, m.focus)
		return m, m.handleKey(msg)

	case tea.MouseMsg:
		return m, m.handleMouse(msg)

	case tea.FocusMsg:
		m.focused = true
		return m, tea.Batch(setOnlineCmd(m.ctx, m.be, true), m.markReadIfWatching())

	case tea.BlurMsg:
		m.focused = false
		return m, tea.Batch(setOnlineCmd(m.ctx, m.be, false), m.stopTyping())

	case backendEventMsg:
		return m, m.handleEvent(msg.ev)

	case chatsLoadedMsg:
		m.chatsPending = false
		if msg.err != nil {
			return m, m.fail(msg.err)
		}
		m.setChats(msg.chats)
		if m.selectedJID == "" && len(m.visible) > 0 {
			m.selectedJID = m.visible[0].JID
		}
		return m, tea.Batch(m.showSelected(), m.titleCmd())

	case chatUpdatedMsg:
		if msg.err != nil {
			return m, nil
		}
		m.updateChat(msg.jid, msg.chat, msg.found)
		if msg.jid == m.openJID {
			// A message arrived in the chat you're looking at: it's read.
			return m, tea.Batch(m.titleCmd(), m.markReadIfWatching())
		}
		return m, m.titleCmd()

	case contactsLoadedMsg:
		m.contactsOK = true
		m.contacts = msg.contacts
		m.refilter()
		return m, nil

	case pageLoadedMsg:
		return m, m.pageLoaded(msg)

	case msgUpdatedMsg:
		return m, m.msgUpdated(msg)

	case previewsReadyMsg:
		if msg.chatJID == m.openJID {
			m.invalidateMedia()
			m.rebuildLines()
			return m, m.wantPreviews() // more may have scrolled into view
		}
		return m, nil

	case membersLoadedMsg:
		return m, m.sug.membersLoaded(m, msg)

	case reloadChatsMsg:
		return m, loadChatsCmd(m.ctx, m.be)

	case reloadNamesMsg:
		m.namesPending = false
		m.contactsOK, m.contacts = false, nil
		return m, tea.Batch(loadChatsCmd(m.ctx, m.be), m.reloadOpen())

	case typingTickMsg:
		return m, m.typingTick()

	case redrawMsg:
		return m, nil

	case statusClearMsg:
		if msg.seq == m.statusSeq {
			m.status = ""
		}
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			return m, m.fail(msg.err)
		}
		if msg.note != "" {
			return m, m.info(msg.note)
		}
		return m, nil

	case mediaSavedMsg:
		return m, m.mediaSaved(msg)

	case viewerDrawnMsg:
		if m.viewer != nil && msg.seq == m.viewer.seq {
			m.viewer.drawn(msg)
		}
		return m, nil

	case searchResultsMsg:
		if m.overlay != nil {
			m.overlay.searchResults(msg)
		}
		return m, nil

	case infoLoadedMsg:
		if m.overlay != nil {
			m.overlay.infoLoaded(m, msg)
		}
		return m, nil

	case clipboardImageMsg:
		if msg.err != nil {
			return m, m.fail(msg.err)
		}
		return m, m.setAttachment(msg.path)
	}
	return m, nil
}

// handleEvent reacts to something WhatsApp told us.
func (m *model) handleEvent(ev whatsapp.Event) tea.Cmd {
	switch ev := ev.(type) {
	case whatsapp.MessageEvent:
		if ev.ChatJID == m.openJID {
			return fetchMessageCmd(m.ctx, m.be, ev.ChatJID, ev.ID, ev.New)
		}

	case whatsapp.ChatEvent:
		if ev.JID != "" {
			return fetchChatCmd(m.ctx, m.be, ev.JID)
		}
		// Many chats changed (a history sync): reload everything, but at
		// most a few times a second however many of these arrive.
		if m.chatsPending {
			return nil
		}
		m.chatsPending = true
		return tea.Batch(
			tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return reloadChatsMsg{} }),
			m.reloadOpen(),
		)

	case whatsapp.NamesEvent:
		if m.namesPending {
			return nil
		}
		m.namesPending = true
		return tea.Tick(time.Second, func(time.Time) tea.Msg { return reloadNamesMsg{} })

	case whatsapp.PresenceEvent:
		m.presence[ev.JID] = ev

	case whatsapp.TypingEvent:
		chat := m.typing[ev.ChatJID]
		if chat == nil {
			chat = map[string]typingInfo{}
			m.typing[ev.ChatJID] = chat
		}
		if ev.State == whatsapp.TypingNone {
			delete(chat, ev.SenderJID)
			return nil
		}
		chat[ev.SenderJID] = typingInfo{name: ev.SenderName, state: ev.State, at: time.Now()}
		// WhatsApp doesn't always say when someone stops; redraw after the
		// indicator's lifetime so it goes away by itself.
		return tea.Tick(typingLifetime+time.Second, func(time.Time) tea.Msg { return redrawMsg{} })

	case whatsapp.ConnEvent:
		m.conn = ev.State
		switch ev.State {
		case whatsapp.ConnConnected:
			m.status = ""
			cmds := []tea.Cmd{setOnlineCmd(m.ctx, m.be, m.focused)}
			if m.openJID != "" && m.focus != focusChatList {
				cmds = append(cmds, watchPresenceCmd(m.ctx, m.be, m.openJID))
			}
			return tea.Batch(cmds...)
		case whatsapp.ConnLoggedOut:
			m.status, m.statusErr = "This device was unlinked. Quit and run 'wa login' again.", true
		case whatsapp.ConnReplaced:
			m.status, m.statusErr = "WhatsApp was opened with this login somewhere else (another wa running?).", true
		}
	}
	return nil
}

// --- status line ---

const statusLifetime = 6 * time.Second

func (m *model) setStatus(text string, isErr bool) tea.Cmd {
	m.status, m.statusErr = oneLine(text), isErr
	m.statusSeq++
	seq := m.statusSeq
	return tea.Tick(statusLifetime, func(time.Time) tea.Msg { return statusClearMsg{seq} })
}

func (m *model) info(text string) tea.Cmd { return m.setStatus(text, false) }
func (m *model) fail(err error) tea.Cmd   { return m.setStatus(err.Error(), true) }

// titleCmd puts the number of unread chats in the terminal's title bar.
func (m *model) titleCmd() tea.Cmd {
	unread := 0
	for _, c := range m.chats {
		if (c.UnreadCount > 0 || c.MarkedUnread) && !c.Archived {
			unread++
		}
	}
	if unread == m.titleN {
		return nil
	}
	m.titleN = unread
	if unread == 0 {
		return tea.SetWindowTitle("WhatsApp")
	}
	return tea.SetWindowTitle("(" + itoa(unread) + ") WhatsApp")
}

// --- typing indicator (ours) ---

const (
	typingLifetime = 20 * time.Second // how long someone's "typing…" is believed
	typingIdle     = 6 * time.Second  // pause after which ours is cleared
)

// typed is called after every edit of the message box: it shows
// "typing…" to the other side and schedules clearing it.
func (m *model) typed() tea.Cmd {
	if m.openJID == "" {
		return nil
	}
	if m.ed.Empty() {
		return m.stopTyping()
	}
	m.typingAt = time.Now()
	tick := tea.Tick(typingIdle, func(time.Time) tea.Msg { return typingTickMsg{} })
	if m.typingOn {
		return tick
	}
	m.typingOn = true
	return tea.Batch(tick, setTypingCmd(m.ctx, m.be, m.openJID, true))
}

func (m *model) typingTick() tea.Cmd {
	if m.typingOn && time.Since(m.typingAt) >= typingIdle {
		return m.stopTyping()
	}
	return nil
}

func (m *model) stopTyping() tea.Cmd {
	if !m.typingOn || m.openJID == "" {
		m.typingOn = false
		return nil
	}
	m.typingOn = false
	return setTypingCmd(m.ctx, m.be, m.openJID, false)
}

// markReadIfWatching marks the open chat read when you're actually looking
// at its newest messages (not just previewing it from the chat list).
func (m *model) markReadIfWatching() tea.Cmd {
	if m.openJID == "" || m.focus == focusChatList || !m.focused || m.scrollBack > 0 || m.viewer != nil {
		return nil
	}
	if i, ok := m.chatIdx[m.openJID]; ok && m.chats[i].UnreadCount == 0 && !m.chats[i].MarkedUnread {
		return nil
	}
	return markReadCmd(m.ctx, m.be, m.openJID)
}
