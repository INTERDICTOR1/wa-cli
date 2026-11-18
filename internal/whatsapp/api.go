package whatsapp

import (
	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// This file is the contract between internal/whatsapp and internal/tui:
// the view types the TUI renders, what it can send, and the events it is
// told about. The TUI never touches whatsmeow or protobufs directly.

// ChatView is a chat as the TUI lists it.
type ChatView struct {
	chatstore.Chat
	// Title is the name to show: saved contact name, group subject, push
	// name, or formatted phone number - never empty.
	Title string
}

// Message kinds (MessageView.Kind).
const (
	KindText        = "text"
	KindImage       = "image"
	KindVideo       = "video"
	KindGIF         = "gif"
	KindAudio       = "audio"
	KindVoice       = "voice"
	KindDocument    = "document"
	KindSticker     = "sticker"
	KindLocation    = "location"
	KindContact     = "contact"
	KindPoll        = "poll"
	KindSystem      = "system"      // group changes, calls, pins: a centred info line
	KindDeleted     = "deleted"     // deleted for everyone
	KindUnsupported = "unsupported" // Body holds a "[type]" placeholder
)

// MessageView is a stored message ready to draw.
type MessageView struct {
	msgstore.Message

	Kind string
	// Sender is the resolved sender name; "" for your own messages.
	Sender string
	// Body is the text to show: the message text or media caption with
	// @mentions resolved to names. For location/contact/system/unsupported
	// kinds it is the full human-readable description. May be "" for media
	// without a caption.
	Body      string
	Quote     *Quote     // the message this one replies to, if any
	Forwarded bool       // marked as forwarded
	Media     *MediaInfo // set for image/video/gif/audio/voice/document/sticker
	Poll      *PollView  // set for polls
	Reactions []ReactionView
	// Mentioned is true when the message @mentions you.
	Mentioned bool
}

// Quote is the replied-to message shown above a reply.
type Quote struct {
	ID     string // the quoted message's ID
	Sender string // resolved name; "You" for your own
	Text   string // one-line snippet; media as "[image] caption"
}

// MediaInfo describes a media attachment without downloading it.
type MediaInfo struct {
	Mime     string
	FileName string // documents only
	Size     uint64 // bytes
	Width    int
	Height   int
	Seconds  int  // audio/video duration
	Pages    int  // documents
	HasThumb bool // Thumbnail() works without a network download
	ViewOnce bool
}

// PollView is a poll with its current tally.
type PollView struct {
	Question    string
	Options     []PollOption
	MultiSelect bool
}

// PollOption is one poll choice.
type PollOption struct {
	Name  string
	Votes int
	Mine  bool // you voted for this option
}

// ReactionView is one emoji's reactions on a message, aggregated.
type ReactionView struct {
	Emoji string
	Count int
	Mine  bool     // you reacted with this emoji
	Names []string // who reacted (resolved names, "You" for yourself)
}

// Member is a participant of a group, for @mentions and group info.
type Member struct {
	JID     string
	Name    string
	IsAdmin bool
}

// Outgoing is a message to send.
type Outgoing struct {
	// Text is the message text, or the caption when FilePath is set.
	Text string
	// Mentions lists the JIDs @mentioned; each must appear in Text as "@"
	// followed by the JID's user part.
	Mentions []string
	// ReplyTo quotes this message.
	ReplyTo *msgstore.Message
	// FilePath sends this file: as an image, video, audio or document
	// depending on its type.
	FilePath string
	// AsDocument sends FilePath as a document, byte for byte.
	AsDocument bool
	// Voice sends an audio file as a voice note.
	Voice bool
}

// Behavior holds the user's privacy-affecting choices.
type Behavior struct {
	// SendReadReceipts tells senders (blue ticks) when you open a chat.
	// When false, chats are still marked read on your own devices.
	SendReadReceipts bool
	// SendPresence shows you as online while the TUI is focused and as
	// typing while you type. Needed to see others' online/typing state.
	SendPresence bool
}

// Event is something the TUI should react to. Delivered through the
// handler registered with Client.OnEvent, from background goroutines.
type Event interface{ isEvent() }

// MessageEvent: a message was stored (New) or changed in place (edited,
// reacted to, deleted, delivery status changed, media replaced). If the
// message no longer exists (deleted locally), Message() reports not found.
type MessageEvent struct {
	ChatJID string
	ID      string
	New     bool
}

// ChatEvent: a chat's list entry changed (unread count, last message,
// name, pin/archive/mute). JID == "" means many chats changed: reload.
type ChatEvent struct{ JID string }

// PresenceEvent: a contact came online or went offline.
type PresenceEvent struct {
	JID      string // the chat (1:1) this is about, canonical form
	Online   bool
	LastSeen int64 // unix millis; 0 if unknown/hidden
}

// TypingState is what someone is doing in a chat.
type TypingState int

const (
	TypingNone TypingState = iota
	TypingText
	TypingAudio // recording a voice note
)

// TypingEvent: someone started or stopped typing in a chat.
type TypingEvent struct {
	ChatJID    string
	SenderJID  string
	SenderName string
	State      TypingState
}

// ConnState is the connection's state.
type ConnState int

const (
	ConnConnecting ConnState = iota
	ConnConnected
	ConnDisconnected // will retry automatically
	ConnLoggedOut    // the phone unlinked this device: `wa login` again
	ConnReplaced     // another session with this login took over
)

// ConnEvent: the connection state changed.
type ConnEvent struct {
	State  ConnState
	Detail string
}

// NamesEvent: contact or group names changed; titles and sender names
// should be re-read.
type NamesEvent struct{}

func (MessageEvent) isEvent()  {}
func (ChatEvent) isEvent()     {}
func (PresenceEvent) isEvent() {}
func (TypingEvent) isEvent()   {}
func (ConnEvent) isEvent()     {}
func (NamesEvent) isEvent()    {}
