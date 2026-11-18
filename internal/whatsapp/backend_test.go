package whatsapp

import (
	"context"
	"image"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// backend is a copy of internal/tui.Backend, the method set the TUI needs
// from *Client. It lives here, rather than importing tui, so this package
// keeps compiling on its own; if tui.Backend changes, change this too.
type backend interface {
	// --- reading (local) ---
	Chats(ctx context.Context) ([]ChatView, error)
	Chat(ctx context.Context, jid string) (ChatView, bool, error)
	Messages(ctx context.Context, chatJID string, before *msgstore.Message, limit int) ([]MessageView, error)
	Message(ctx context.Context, chatJID, id string) (MessageView, bool, error)
	Search(ctx context.Context, chatJID, query string, limit int) ([]MessageView, error)
	Contacts(ctx context.Context) ([]Contact, error)
	Members(ctx context.Context, groupJID string) ([]Member, error)

	// --- sending ---
	Send(ctx context.Context, chatJID string, out Outgoing) error
	Retry(ctx context.Context, msg msgstore.Message) error
	React(ctx context.Context, msg msgstore.Message, emoji string) error
	Edit(ctx context.Context, msg msgstore.Message, text string) error
	Revoke(ctx context.Context, msg msgstore.Message) error
	DeleteLocal(msg msgstore.Message) error
	Forward(ctx context.Context, toChatJID string, msg msgstore.Message) error

	// --- chat state ---
	MarkChatRead(ctx context.Context, chatJID string) error
	MarkChatUnread(ctx context.Context, chatJID string) error
	SetPinned(ctx context.Context, chatJID string, on bool) error
	SetArchived(ctx context.Context, chatJID string, on bool) error
	SetMuted(ctx context.Context, chatJID string, on bool) error
	SetDraft(chatJID, text string) error
	LoadOlder(ctx context.Context, chatJID string) (requested bool, err error)

	// --- presence ---
	SetOnline(ctx context.Context, online bool) error
	SetTyping(ctx context.Context, chatJID string, typing bool) error
	WatchPresence(ctx context.Context, chatJID string) error

	// --- media ---
	Thumbnail(ctx context.Context, msg msgstore.Message) (image.Image, error)
	SaveMedia(ctx context.Context, msg msgstore.Message, dir string) (string, error)
}

// lifecycle is what the TUI's entry point uses beyond Backend.
type lifecycle interface {
	OnEvent(fn func(Event))
	SetBehavior(b Behavior)
	SetNotifications(enabled, groups, showPreview bool)
	Start(ctx context.Context) error
	Stop()
}

var (
	_ backend   = (*Client)(nil)
	_ lifecycle = (*Client)(nil)
)
