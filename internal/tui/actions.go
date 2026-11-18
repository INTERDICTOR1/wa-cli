package tui

import (
	"context"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// This file holds the background work the TUI starts (tea.Cmds) and the
// messages that bring the results back to Update. Nothing here touches
// the model: a Cmd runs on its own goroutine.

type (
	backendEventMsg struct{ ev whatsapp.Event }

	chatsLoadedMsg struct {
		chats []whatsapp.ChatView
		err   error
	}
	chatUpdatedMsg struct {
		jid   string
		chat  whatsapp.ChatView
		found bool
		err   error
	}
	contactsLoadedMsg struct{ contacts []whatsapp.Contact }

	// pageLoadedMsg carries one page of a chat's messages.
	pageLoadedMsg struct {
		chatJID string
		seq     int // model.loadSeq when requested
		msgs    []whatsapp.MessageView
		older   bool // a page older than what's loaded (else: the newest page)
		keep    bool // a refresh: keep the scroll position and selection
		limit   int
		err     error
	}
	// msgUpdatedMsg carries one message that was added or changed.
	msgUpdatedMsg struct {
		chatJID, id string
		view        whatsapp.MessageView
		found       bool
		isNew       bool
	}

	reloadChatsMsg struct{}
	reloadNamesMsg struct{}
	typingTickMsg  struct{}
	redrawMsg      struct{}
	statusClearMsg struct{ seq int }

	// actionDoneMsg reports a finished action: an error to show, or a note.
	actionDoneMsg struct {
		note string
		err  error
	}
)

func itoa(n int) string { return strconv.Itoa(n) }

func loadChatsCmd(ctx context.Context, be Backend) tea.Cmd {
	return func() tea.Msg {
		chats, err := be.Chats(ctx)
		return chatsLoadedMsg{chats, err}
	}
}

func fetchChatCmd(ctx context.Context, be Backend, jid string) tea.Cmd {
	return func() tea.Msg {
		chat, found, err := be.Chat(ctx, jid)
		return chatUpdatedMsg{jid, chat, found, err}
	}
}

func loadContactsCmd(ctx context.Context, be Backend) tea.Cmd {
	return func() tea.Msg {
		contacts, _ := be.Contacts(ctx)
		return contactsLoadedMsg{contacts}
	}
}

// loadPageCmd loads the newest limit messages (before == nil) or the page
// just older than before.
func loadPageCmd(ctx context.Context, be Backend, chatJID string, seq int, before *msgstore.Message, limit int, keep bool) tea.Cmd {
	return func() tea.Msg {
		msgs, err := be.Messages(ctx, chatJID, before, limit)
		return pageLoadedMsg{chatJID: chatJID, seq: seq, msgs: msgs, older: before != nil, keep: keep, limit: limit, err: err}
	}
}

func fetchMessageCmd(ctx context.Context, be Backend, chatJID, id string, isNew bool) tea.Cmd {
	return func() tea.Msg {
		view, found, err := be.Message(ctx, chatJID, id)
		if err != nil {
			return nil
		}
		return msgUpdatedMsg{chatJID: chatJID, id: id, view: view, found: found, isNew: isNew}
	}
}

// do runs an action and reports only its error (success shows up through
// the events the action causes).
func do(fn func() error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(); err != nil {
			return actionDoneMsg{err: err}
		}
		return nil
	}
}

// quiet runs an action whose failure isn't worth telling the user about
// (presence, typing, read receipts while offline).
func quiet(fn func() error) tea.Cmd {
	return func() tea.Msg {
		if err := fn(); err != nil {
			dlog("background action failed: %v", err)
		}
		return nil
	}
}

func sendCmd(ctx context.Context, be Backend, chatJID string, out whatsapp.Outgoing) tea.Cmd {
	return do(func() error { return be.Send(ctx, chatJID, out) })
}

func markReadCmd(ctx context.Context, be Backend, chatJID string) tea.Cmd {
	return quiet(func() error { return be.MarkChatRead(ctx, chatJID) })
}

func setOnlineCmd(ctx context.Context, be Backend, online bool) tea.Cmd {
	return quiet(func() error { return be.SetOnline(ctx, online) })
}

func setTypingCmd(ctx context.Context, be Backend, chatJID string, typing bool) tea.Cmd {
	return quiet(func() error { return be.SetTyping(ctx, chatJID, typing) })
}

func watchPresenceCmd(ctx context.Context, be Backend, chatJID string) tea.Cmd {
	return quiet(func() error { return be.WatchPresence(ctx, chatJID) })
}

func saveDraftCmd(be Backend, chatJID, text string) tea.Cmd {
	return quiet(func() error { return be.SetDraft(chatJID, text) })
}

func loadOlderCmd(ctx context.Context, be Backend, chatJID string) tea.Cmd {
	return func() tea.Msg {
		requested, err := be.LoadOlder(ctx, chatJID)
		switch {
		case err != nil:
			return actionDoneMsg{err: err}
		case requested:
			return actionDoneMsg{note: "Asked your phone for older messages…"}
		}
		return actionDoneMsg{note: "No older messages."}
	}
}
