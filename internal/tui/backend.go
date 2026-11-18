package tui

import (
	"context"
	"image"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// Backend is everything the TUI needs from WhatsApp and the local stores.
// *whatsapp.Client implements it; tests use a fake. Every method is safe
// to call from a tea.Cmd goroutine. Methods taking a context may use the
// network; the rest are local.
type Backend interface {
	// --- reading (local) ---

	// Chats returns every chat (archived ones included, flagged), pinned
	// first, then most recent first.
	Chats(ctx context.Context) ([]whatsapp.ChatView, error)
	// Chat returns one chat's list entry.
	Chat(ctx context.Context, jid string) (whatsapp.ChatView, bool, error)
	// Messages returns up to limit messages of a chat, oldest first: the
	// newest ones when before is nil, otherwise the ones just older than
	// before. Messages that shouldn't be shown are already left out.
	Messages(ctx context.Context, chatJID string, before *msgstore.Message, limit int) ([]whatsapp.MessageView, error)
	// Message returns one message, or false if it isn't stored (any more).
	Message(ctx context.Context, chatJID, id string) (whatsapp.MessageView, bool, error)
	// Search finds messages containing query (case-insensitive), newest
	// first; chatJID "" searches every chat.
	Search(ctx context.Context, chatJID, query string, limit int) ([]whatsapp.MessageView, error)
	// Contacts returns the address book, sorted by name.
	Contacts(ctx context.Context) ([]whatsapp.Contact, error)
	// Members returns a group's participants (not you), sorted by name.
	// Uses the network the first time for a group.
	Members(ctx context.Context, groupJID string) ([]whatsapp.Member, error)

	// --- sending ---

	// Send sends a text or file message. The message is stored as pending
	// and announced with a MessageEvent before the network round trip;
	// its final status arrives as another MessageEvent.
	Send(ctx context.Context, chatJID string, out whatsapp.Outgoing) error
	// Retry re-sends a message whose status is StatusFailed.
	Retry(ctx context.Context, msg msgstore.Message) error
	// React sets your reaction on msg; emoji "" removes it.
	React(ctx context.Context, msg msgstore.Message, emoji string) error
	// Edit replaces the text of a message you sent.
	Edit(ctx context.Context, msg msgstore.Message, text string) error
	// Revoke deletes a message you sent, for everyone.
	Revoke(ctx context.Context, msg msgstore.Message) error
	// DeleteLocal removes a message from this device only.
	DeleteLocal(msg msgstore.Message) error
	// Forward sends msg's content to another chat, marked as forwarded.
	Forward(ctx context.Context, toChatJID string, msg msgstore.Message) error

	// --- chat state ---

	// MarkChatRead zeroes the unread count and tells WhatsApp the chat's
	// messages were read.
	MarkChatRead(ctx context.Context, chatJID string) error
	// MarkChatUnread sets the manual "unread" flag.
	MarkChatUnread(ctx context.Context, chatJID string) error
	SetPinned(ctx context.Context, chatJID string, on bool) error
	SetArchived(ctx context.Context, chatJID string, on bool) error
	SetMuted(ctx context.Context, chatJID string, on bool) error
	// SetDraft remembers unsent text for a chat.
	SetDraft(chatJID, text string) error
	// LoadOlder asks the phone for messages older than the oldest stored
	// one. They arrive later as MessageEvents. requested is false when
	// there is nothing to anchor the request on.
	LoadOlder(ctx context.Context, chatJID string) (requested bool, err error)

	// --- presence ---

	// SetOnline shows you as online or offline.
	SetOnline(ctx context.Context, online bool) error
	// SetTyping shows or clears "typing…" in a chat.
	SetTyping(ctx context.Context, chatJID string, typing bool) error
	// WatchPresence asks for online/last-seen updates for a 1:1 chat.
	WatchPresence(ctx context.Context, chatJID string) error

	// --- media ---

	// Thumbnail returns the small picture embedded in an image, video,
	// sticker or document message (no download when MediaInfo.HasThumb).
	Thumbnail(ctx context.Context, msg msgstore.Message) (image.Image, error)
	// SaveMedia downloads msg's attachment into dir, streaming to disk,
	// and returns the file's path. An already-downloaded file is reused.
	SaveMedia(ctx context.Context, msg msgstore.Message, dir string) (string, error)
}
